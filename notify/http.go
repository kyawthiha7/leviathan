package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"syscall"
	"time"
)

// allowLocalTargets decides whether delivering a message to a loopback /
// link-local address is allowed.
//
// Denied by default. These address ranges aren't where an IM bot or public
// mail server would ever live, and what they *can* reach is sensitive: the
// admin port of another service on the same host, and the cloud metadata
// endpoint (169.254.169.254, which can leak instance credentials). The
// delivery address is set by an admin, but an admin session hijacked via
// XSS/CSRF, or a second person sharing the same JWT, could use a config
// change to read response content back out—doJSON writes the first 200
// bytes of a 4xx/5xx response body into last_error, and the delivery
// history endpoint echoes it back, which is a semi-blind read primitive.
//
// But a "local SMTP relay" (postfix on 127.0.0.1:25) is a common setup for
// self-hosted mail, and a blanket block would get people stuck. So there's
// an explicit escape hatch instead of a hardcoded allowance: set
// LEVIATHAN_NOTIFY_ALLOW_LOCAL=1 to allow it.
//
// Exported as AllowLocalTargetsEnv so tests can explicitly turn it on—this
// package's and the server package's test suites heavily rely on httptest
// fake receivers on 127.0.0.1, which would all be blocked by the guard
// otherwise.
const AllowLocalTargetsEnv = "LEVIATHAN_NOTIFY_ALLOW_LOCAL"

func allowLocalTargets() bool {
	v := strings.TrimSpace(os.Getenv(AllowLocalTargetsEnv))
	return v == "1" || strings.EqualFold(v, "true")
}

// isBlockedDialIP reports whether the target IP falls in an address range
// that's denied for delivery by default.
//
// Only rejects loopback, link-local (including the cloud metadata address
// 169.254.169.254), unspecified, and multicast. **Does not** reject RFC1918
// private ranges: a self-hosted Mattermost / SMTP relay on the internal
// network is a common legitimate use case, and blocking those too would
// make the feature unusable in real deployments. This trade-off is
// deliberate—the guard should stop genuinely sensitive targets without
// breaking normal deployments.
func isBlockedDialIP(ip net.IP) bool {
	if ip == nil {
		return true
	}
	// An IPv4-mapped IPv6 address (::ffff:127.0.0.1) must be normalized
	// back to IPv4 before checking, otherwise it bypasses the check.
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	}
	return ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() || ip.IsUnspecified() || ip.IsMulticast()
}

// blockInternalDial is the Control hook for http.Transport's dialer; it
// checks the target address at **connection-establishment time**.
//
// Why here rather than only validating when saving config: this is the
// final point where it actually takes effect. It also covers two ways of
// bypassing config-time validation—DNS rebinding (resolving to a public IP
// at validation time, then to an internal address at actual connection
// time) and redirects (we already reject cross-host redirects, but a
// same-host redirect can still point the path somewhere else).
func blockInternalDial(_, address string, _ syscall.RawConn) error {
	if allowLocalTargets() {
		return nil
	}
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return fmt.Errorf("cannot resolve target address %q", host)
	}
	if isBlockedDialIP(ip) {
		return fmt.Errorf("refusing to deliver to a local/link-local address %s (set %s=1 if delivering to a local service is actually intended)", ip, AllowLocalTargetsEnv)
	}
	return nil
}

// notifyTransport adds just one dial guard on top of the default
// Transport. Uses Clone to preserve all of the default tuning (connection
// pool, HTTP/2, timeouts, proxy, etc.), avoiding unrelated behavior changes
// just to add one check.
var notifyTransport = func() *http.Transport {
	t, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return &http.Transport{}
	}
	clone := t.Clone()
	clone.DialContext = (&net.Dialer{Timeout: 10 * time.Second, Control: blockInternalDial}).DialContext
	return clone
}()

// httpClient is the shared client for all channel deliveries.
//
// Deliberately **does not** reuse the project's global egress proxy (the
// server side's GlobalProxy): that proxy is meant for traffic to pentest
// targets and is often an unstable tunnel, whereas notification
// availability shouldn't be held hostage by target-network flakiness. IM
// push can go direct. Timeout is 15 seconds—a peer slower than that is
// effectively already in a failure state.
//
// Cross-host redirects are rejected: this feature's delivery addresses are
// all "one fixed endpoint" in shape and normally never redirect to a
// different host, while the credentials for several of these platforms
// (DingTalk's access_token, WeCom's key, Telegram's bot token) **live in
// the URL itself**—following a cross-host redirect would hand the
// credential to the redirect target. Same-host redirects (e.g. an added
// trailing slash) are still allowed.
var httpClient = &http.Client{
	Timeout:   15 * time.Second,
	Transport: notifyTransport,
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return errors.New("too many redirects")
		}
		if len(via) > 0 && req.URL.Host != via[0].URL.Host {
			return fmt.Errorf("refusing cross-host redirect (%s -> %s)", via[0].URL.Host, req.URL.Host)
		}
		return nil
	},
}

// respBodyLimit caps how much of the response body gets read. A
// misbehaving peer can return an oversized body, but we only need the
// status code and a short error description to show in delivery history.
const respBodyLimit = 8 << 10

// doJSON sends one request and returns the (length-capped) response body.
//
// A nil payload sends an empty body (for GET, or platforms that don't
// require one). Entries in headers are attached as-is, used for generic
// webhook custom headers.
//
// Error classification is this function's core responsibility: network-layer
// failures and 5xx/408/429 are classified as "retryable"; the remaining 4xx
// codes are classified as "permanent failure"—retrying a 403 just prints the
// same error to the log three more times.
func doJSON(ctx context.Context, method, url string, headers map[string]string, payload any) ([]byte, error) {
	var body io.Reader
	if payload != nil {
		raw, err := json.Marshal(payload)
		if err != nil {
			// A serialization failure is a local bug (a config field has
			// the wrong type); retrying won't help.
			return nil, Permanent(fmt.Errorf("failed to build request body: %w", err))
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		// An invalid URL is almost always a user typo in the address,
		// which is a permanent failure. err itself must not be passed
		// through either: url.Parse's error text includes the full
		// address.
		return nil, Permanent(fmt.Errorf("invalid request address: %s", redactRequestTarget(url)))
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json; charset=utf-8")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		// Connection refused, DNS failure, timeout—mostly transient
		// failures, hand off to backoff retry.
		//
		// The error text must be redacted before propagating it. Reason:
		// http.Client.Do returns a *url.Error, whose Error() is
		// `Op "full URL": underlying error`, and for this feature the
		// credentials for several platforms **live in the URL itself**
		// (DingTalk's access_token, WeCom's key, Feishu's hook id,
		// Telegram's /bot<token>/). Without redaction, the credential would
		// leak through this error string into four places: the
		// notification_deliveries last_error column (stored in plaintext),
		// the delivery-history endpoint's response (**bypassing the
		// channel config's masking**), server logs, and the 502 text
		// returned to the frontend by the test-send endpoint.
		return nil, fmt.Errorf("request failed: %s", redactTransportError(err))
	}
	defer resp.Body.Close()
	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, respBodyLimit))
	if readErr != nil {
		return nil, fmt.Errorf("failed to read response: %w", readErr)
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return raw, nil
	}
	// 429 (rate limited) and 408 (timeout) are worth retrying; other 4xx
	// codes are config or permission problems, where retrying is pointless.
	if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusRequestTimeout {
		return nil, fmt.Errorf("peer rate-limited or timed out (HTTP %d): %s", resp.StatusCode, snippet(raw))
	}
	if resp.StatusCode >= 500 {
		return nil, fmt.Errorf("peer service error (HTTP %d): %s", resp.StatusCode, snippet(raw))
	}
	return nil, Permanent(fmt.Errorf("peer rejected the request (HTTP %d): %s", resp.StatusCode, snippet(raw)))
}

// snippet compresses the response body into a short single line, for use
// in error messages. The response may contain newlines and lots of
// whitespace, and stuffing that straight into last_error would break the
// delivery-history page's layout.
func snippet(raw []byte) string {
	return OneLine(string(raw), 200)
}

// redactRequestTarget compresses a delivery address down to
// "scheme://host/…", for use in error messages.
//
// This is this package's only address-redaction policy, deliberately
// **blunt**: everything except scheme and host is discarded. The reason is
// there's no "general and safe" way to tell which segment of a URL is the
// credential:
//
//	DingTalk  credential is in the query   /robot/send?access_token=xxx
//	WeCom     credential is in the query   /cgi-bin/webhook/send?key=xxx
//	Feishu    credential is in the **final path segment** /open-apis/bot/v2/hook/<hook_id>
//	Telegram  credential is in the **middle of the path** /bot<token>/sendMessage
//
// Trying to "keep only the useful part" would require patching per
// channel, and missing even one is a credential leak. Keeping the host is
// already enough for troubleshooting (DNS failures, connection failures,
// cert errors are all locatable from it); which specific bot it is can be
// identified from the masked suffix shown in the channel config.
//
// Returns a fixed placeholder on parse failure—the raw string is never
// echoed back.
func redactRequestTarget(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "(address could not be parsed)"
	}
	return u.Scheme + "://" + u.Host + "/…"
}

// redactTransportError strips the address out of a transport-layer error,
// keeping only the underlying cause.
//
// *url.Error's structure is {Op, URL, Err}; its Error() prints the URL
// alongside everything else. Here we explicitly take the Err field,
// bypassing its Error()—this is more reliable than a post-hoc string
// replacement, since a replacement would need to correctly handle every
// variant of URL encoding/escaping and is easy to get wrong.
func redactTransportError(err error) string {
	var uerr *url.Error
	if errors.As(err, &uerr) {
		host := ""
		if u, parseErr := url.Parse(uerr.URL); parseErr == nil {
			host = u.Host
		}
		if uerr.Err != nil {
			return fmt.Sprintf("%s %s: %s", uerr.Op, host, uerr.Err)
		}
		return fmt.Sprintf("%s %s: unknown error", uerr.Op, host)
	}
	// A non-*url.Error (e.g. an error from the redirect policy) can also
	// carry an address, so redact it the same way.
	return redactURLsInText(err.Error())
}

// redactURLsInText replaces any http(s) addresses appearing in a block of
// text with their redacted form.
//
// Used as a catch-all for errors that don't expose structured fields
// (redirect-policy errors, third-party library custom errors). Only
// recognizes the http/https prefix and splits on whitespace and quotes—an
// address never contains those characters.
func redactURLsInText(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		rest := s[i:]
		if strings.HasPrefix(rest, "http://") || strings.HasPrefix(rest, "https://") {
			end := len(rest)
			if j := strings.IndexAny(rest, " \t\n\"'"); j >= 0 {
				end = j
			}
			b.WriteString(redactRequestTarget(rest[:end]))
			i += end
			continue
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}
