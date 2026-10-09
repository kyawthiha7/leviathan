package notify

import (
	"context"
	"errors"
	"sort"
	"strconv"
	"strings"
)

// Channel is an adapter for a notification channel. Implementations must be
// **stateless**: the same instance is shared concurrently across multiple
// channel configs, so credentials always come in through the cfg parameter.
type Channel interface {
	// Kind returns the channel type identifier, which must match the
	// registry's key.
	Kind() string
	// Validate is called when saving a config; it checks required fields and
	// formats. The returned error is shown directly to whoever is
	// configuring the channel, so the message should say "which field is
	// missing" rather than a generic "invalid config".
	Validate(cfg map[string]any) error
	// Send delivers one message and returns the **actual number of items
	// delivered** along with an error.
	//
	// Why return a count: every platform has a message-length cap, and a
	// digest message that doesn't fit the whole batch gets truncated. If the
	// caller unconditionally marked the entire batch as delivered, the
	// truncated items would simply vanish—absent from the message and shown
	// as "delivered" in the delivery history, with no way to notice that a
	// finding was never actually sent. By returning kept, the caller only
	// marks the first kept items, leaving the rest for the next batch.
	//
	// A returned error means delivery failed; a *PermanentError among those
	// means it shouldn't be retried. kept is meaningless on failure and the
	// caller should ignore it.
	Send(ctx context.Context, cfg map[string]any, m Message) (int, error)
	// DefaultRatePerMin returns the platform's officially recommended
	// per-minute delivery cap, used as the default rate limit when creating
	// a new channel instance. Returns 0 if there's no known limit.
	DefaultRatePerMin() int
	// SecretKeys returns the key names in this channel's config that are
	// credentials. These keys' values are masked when echoed back by the
	// API, and an update that receives the masked value keeps the original
	// value in the database. Only the implementation itself knows which
	// fields count as credentials (WeCom's entire webhook URL is the
	// credential, while DingTalk's is only the embedded secret), so this
	// knowledge has to come from the channel, not be guessed by the layer
	// above.
	SecretKeys() []string
	// DestinationKeys returns the key names in this channel's config that
	// determine "where the message is sent".
	//
	// Like SecretKeys, this is security-relevant: the destination address
	// and the credential are two separate sets of fields. If "change only
	// the address, keep the credential as-is" were allowed, anyone able to
	// edit a channel's config could redirect the real credential stored in
	// the database to a server they control, which would make the channel
	// config's masking entirely pointless. See PrepareConfigUpdate for
	// details.
	DestinationKeys() []string
}

// registry is the channel registry. Deliberately a literal map rather than
// init()-based self-registration: this way "which channels exist" is visible
// in one place, and a missing channel shows up as a compile-time omission
// instead of depending on runtime side effects.
var registry = map[string]Channel{
	KindDingTalk: dingTalkChannel{},
	KindFeishu:   feishuChannel{},
	KindWeCom:    weComChannel{},
	KindWebhook:  webhookChannel{},
	KindTelegram: telegramChannel{},
	KindEmail:    emailChannel{},
}

// Get looks up a channel implementation by type.
func Get(kind string) (Channel, bool) {
	c, ok := registry[kind]
	return c, ok
}

// ValidKind reports whether kind is a supported channel type.
func ValidKind(kind string) bool {
	_, ok := registry[kind]
	return ok
}

// Kinds returns all supported channel types, sorted lexicographically (for
// stable display in UI dropdowns).
func Kinds() []string {
	out := make([]string, 0, len(registry))
	for k := range registry {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// PermanentError marks a delivery failure that shouldn't be retried: bad
// credentials, target rejection, malformed request body, etc. Retrying only
// makes sense for transient failures (network blips, rate limiting, 5xx from
// the peer); retrying a permanent failure with repeated backoff won't
// succeed and just buries the real error under retry-log noise.
type PermanentError struct{ Err error }

func (e *PermanentError) Error() string { return e.Err.Error() }
func (e *PermanentError) Unwrap() error { return e.Err }

// Permanent marks err as a permanent failure. Returns nil when err is nil,
// so it can be written as `return Permanent(someCheck())`.
func Permanent(err error) error {
	if err == nil {
		return nil
	}
	return &PermanentError{Err: err}
}

// IsPermanent reports whether err's chain carries the permanent-failure
// marker.
func IsPermanent(err error) bool {
	var pe *PermanentError
	return errors.As(err, &pe)
}

// ---- config-reading helpers ----
//
// Channel configs come from a JSONB column in the database; after
// encoding/json deserialization they're map[string]any, with numbers always
// float64 and arrays always []any. The helpers below normalize that
// conversion and tolerate the type mismatches users introduce by leaving UI
// fields blank (e.g. typing a port number as a string).

// cfgString reads a string config value, always trimming surrounding
// whitespace—easy to pick up when copy-pasting from a web form.
func cfgString(cfg map[string]any, key string) string {
	v, ok := cfg[key]
	if !ok {
		return ""
	}
	s, ok := v.(string)
	if !ok {
		return ""
	}
	return strings.TrimSpace(s)
}

// cfgInt reads an int config value, accepting both float64 (JSON's default)
// and string sources.
func cfgInt(cfg map[string]any, key string) int {
	switch v := cfg[key].(type) {
	case float64:
		return int(v)
	case int:
		return v
	case string:
		n, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil {
			return 0
		}
		return n
	default:
		return 0
	}
}

// cfgBool reads a bool config value, accepting the strings "true"/"1".
func cfgBool(cfg map[string]any, key string) bool {
	switch v := cfg[key].(type) {
	case bool:
		return v
	case string:
		s := strings.ToLower(strings.TrimSpace(v))
		return s == "true" || s == "1" || s == "yes"
	default:
		return false
	}
}

// cfgStrings reads a string-array config value, trimming whitespace and
// dropping empty strings.
func cfgStrings(cfg map[string]any, key string) []string {
	raw, ok := cfg[key].([]any)
	if !ok {
		// Also accept a single string, to accommodate form submissions that
		// only have one value.
		if s := cfgString(cfg, key); s != "" {
			return []string{s}
		}
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		s, ok := v.(string)
		if !ok {
			continue
		}
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// cfgMap reads a string-map config value (e.g. custom HTTP headers),
// trimming whitespace from both keys and values and dropping empty keys.
func cfgMap(cfg map[string]any, key string) map[string]string {
	raw, ok := cfg[key].(map[string]any)
	if !ok {
		return nil
	}
	out := make(map[string]string, len(raw))
	for k, v := range raw {
		k = strings.TrimSpace(k)
		if k == "" {
			continue
		}
		s, ok := v.(string)
		if !ok {
			continue
		}
		out[k] = s
	}
	return out
}
