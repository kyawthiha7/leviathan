package notify

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"time"
)

// dingTalkChannel implements the DingTalk custom bot.
//
// Platform specifics (these drive the implementation choices below):
//   - A single bot is rate-limited to 20 messages/minute; exceeding it gets
//     silently dropped (HTTP can still return 200), so rate limiting must
//     happen client-side — see DefaultRatePerMin.
//   - Security settings offer three mutually exclusive options: HMAC
//     signing / custom keyword / IP allowlist. Signing is the only option
//     that doesn't depend on message content, so only signing is supported
//     (a bare webhook with none of the three enabled also works).
//   - Both success and failure return HTTP 200; they're distinguished by
//     the errcode in the body — not checking errcode would record a failed
//     delivery as successful.
type dingTalkChannel struct{}

func (dingTalkChannel) Kind() string { return KindDingTalk }

func (dingTalkChannel) DefaultRatePerMin() int { return 20 }

// DingTalk's webhook address carries an access_token, making it a credential
// in itself, so it's masked in full.
func (dingTalkChannel) SecretKeys() []string { return []string{"webhook", "secret"} }

// The target is the DingTalk webhook address itself; changing the address
// requires re-confirming the signing secret for the new address too.
func (dingTalkChannel) DestinationKeys() []string { return []string{"webhook"} }

func (dingTalkChannel) Validate(cfg map[string]any) error {
	hook := cfgString(cfg, "webhook")
	if hook == "" {
		return errors.New("missing webhook address")
	}
	if err := validateHTTPURL(hook); err != nil {
		return fmt.Errorf("invalid webhook address: %w", err)
	}
	return nil
}

// Send delivers one message. Uses an ActionCard (with a button) when there's
// a detail link and it's a single item; otherwise uses markdown.
func (c dingTalkChannel) Send(ctx context.Context, cfg map[string]any, m Message) (int, error) {
	hook := cfgString(cfg, "webhook")
	if err := c.Validate(cfg); err != nil {
		return 0, Permanent(err)
	}
	endpoint, err := dingTalkSignedURL(hook, cfgString(cfg, "secret"), time.Now())
	if err != nil {
		return 0, Permanent(err)
	}

	title := markdownTitle(m)
	// DingTalk's markdown body has no documented byte limit, but we still
	// cap it defensively to avoid an abnormally bloated evidence field.
	text, kept := markdownBody(m, 20000)

	var payload any
	if !m.Batch && len(m.Items) == 1 && m.Items[0].DetailURL != "" {
		payload = map[string]any{
			"msgtype": "actionCard",
			"actionCard": map[string]any{
				"title":          title,
				"text":           text,
				"btnOrientation": "0",
				"singleTitle":    "View details",
				"singleURL":      m.Items[0].DetailURL,
			},
		}
	} else {
		payload = map[string]any{
			"msgtype":  "markdown",
			"markdown": map[string]any{"title": title, "text": text},
		}
	}

	raw, err := doJSON(ctx, "POST", endpoint, nil, payload)
	if err != nil {
		return 0, err
	}
	// DingTalk hides business errors inside the 200 response.
	var res struct {
		ErrCode int    `json:"errcode"`
		ErrMsg  string `json:"errmsg"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return 0, fmt.Errorf("failed to parse DingTalk response: %w (%s)", err, snippet(raw))
	}
	if res.ErrCode != 0 {
		// 301000 is a signature check failure, 310000 is a keyword mismatch —
		// both are configuration errors, and retrying won't self-heal them.
		return 0, Permanent(fmt.Errorf("DingTalk returned error %d: %s", res.ErrCode, res.ErrMsg))
	}
	return kept, nil
}

// dingTalkSignedURL appends the timestamp and sign query parameters to the
// webhook per DingTalk's official signing rule.
//
// Rule: the string to sign = timestamp + "\n" + secret, and the HMAC-SHA256
// **key is also secret**; the result is base64-encoded then URL-encoded.
// timestamp is in milliseconds. Returns the hook unchanged when secret is
// empty, to support bots that don't have signing enabled.
func dingTalkSignedURL(hook, secret string, now time.Time) (string, error) {
	if secret == "" {
		return hook, nil
	}
	ts := strconv.FormatInt(now.UnixMilli(), 10)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(ts + "\n" + secret))
	sign := base64.StdEncoding.EncodeToString(mac.Sum(nil))

	u, err := url.Parse(hook)
	if err != nil {
		// Don't propagate err: url.Parse's error text includes the full
		// address (including the access_token).
		return "", fmt.Errorf("failed to parse webhook address: %s", redactRequestTarget(hook))
	}
	q := u.Query()
	q.Set("timestamp", ts)
	q.Set("sign", sign)
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// validateHTTPURL checks that the address is usable, the scheme is
// supported, and flags literal IP targets that resolve to internal
// networks.
//
// Two things worth noting:
//
//  1. **Error messages must be redacted.** url.Parse's own error is
//     *url.Error, whose Error() includes the **full original address** —
//     and for these providers the address itself embeds a credential
//     (DingTalk's access_token, WeCom's key, Telegram's bot token,
//     Feishu's hook id). This used to just `return err`, so an "invalid
//     address format" error would leak the credential into the test
//     endpoint's 400 response, the last_error column recorded for every
//     delivery, server logs, and the delivery-history API.
//
//  2. **Literal IPs are judged as internal right here**; hostnames are left
//     to the dial stage (blockInternalDial is where this is actually
//     enforced, and it also covers DNS rebinding). Doing it here too just
//     lets us surface the warning when the config is saved, instead of
//     waiting for the first delivery to fail.
//
// Restricting the scheme is a defensive measure: things like file:// or
// gopher:// can make http.Client behave unexpectedly (already blocked by
// the scheme check, but there's no reason to leave this surface open).
func validateHTTPURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("address could not be parsed (%s)", redactRequestTarget(raw))
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("only http/https are supported, got %q", u.Scheme)
	}
	if u.Host == "" {
		return errors.New("missing hostname")
	}
	if ip := net.ParseIP(u.Hostname()); ip != nil && isBlockedDialIP(ip) && !allowLocalTargets() {
		return fmt.Errorf("refusing to deliver to loopback/link-local address %s (set %s=1 if delivering to a local service is intentional)", ip, AllowLocalTargetsEnv)
	}
	return nil
}
