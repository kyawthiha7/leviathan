package notify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

// weComMarkdownLimit is the hard cap on the WeCom group bot's markdown content (in
// bytes, not characters). It is the tightest limit among all six channels and the
// main reason TruncateBytes exists.
const weComMarkdownLimit = 4096

// weComChannel implements the WeCom group bot.
//
// Platform traits:
//   - Authentication is solely via the key in the URL; signing is unsupported -
//     so the webhook address itself is the entire credential.
//   - markdown content caps at 4096 **bytes**; overlong rejects the whole message
//     (not truncation). A Chinese character is 3 bytes, meaning only a little over
//     a thousand characters fit, so client-side truncation is required.
//   - Rate limit 20/min, likewise covered by client-side limiting.
type weComChannel struct{}

func (weComChannel) Kind() string { return KindWeCom }

func (weComChannel) DefaultRatePerMin() int { return 20 }

// WeCom has only one credential - the key in the webhook URL - and it does not
// support signing; the whole address is the entire credential, and no other field
// needs masking.
func (weComChannel) SecretKeys() []string { return []string{"webhook"} }

// WeCom has only the webhook field, which is both destination and credential, so
// there is no "credential left over after changing the address" to speak of.
func (weComChannel) DestinationKeys() []string { return []string{"webhook"} }

func (weComChannel) Validate(cfg map[string]any) error {
	hook := cfgString(cfg, "webhook")
	if hook == "" {
		return errors.New("missing webhook address")
	}
	if err := validateHTTPURL(hook); err != nil {
		return fmt.Errorf("invalid webhook address: %w", err)
	}
	return nil
}

func (c weComChannel) Send(ctx context.Context, cfg map[string]any, m Message) (int, error) {
	if err := c.Validate(cfg); err != nil {
		return 0, Permanent(err)
	}
	// A digest batch can be long (50 entries x one line each + prefix), easily
	// exceeding 4096 bytes. Truncation is done here rather than relying on the
	// platform to error: a rejection means the whole batch is lost, whereas
	// truncation at least delivers the first several.
	content, kept := markdownBody(m, weComMarkdownLimit)
	payload := map[string]any{
		"msgtype":  "markdown",
		"markdown": map[string]any{"content": content},
	}
	raw, err := doJSON(ctx, "POST", cfgString(cfg, "webhook"), nil, payload)
	if err != nil {
		return 0, err
	}
	var res struct {
		ErrCode int    `json:"errcode"`
		ErrMsg  string `json:"errmsg"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return 0, fmt.Errorf("failed to parse WeCom response: %w (%s)", err, snippet(raw))
	}
	if res.ErrCode != 0 {
		// 45009 is exceeding the API call limit - the platform's rate-limit window
		// rolls, so retrying after backoff is effective, hence explicitly retryable.
		// Reaching here means the client's rate_per_min is configured too
		// aggressively; retrying is only a backstop, the real fix is lowering this
		// channel's rate limit.
		if res.ErrCode == 45009 {
			return 0, fmt.Errorf("WeCom rate limited %d: %s", res.ErrCode, res.ErrMsg)
		}
		// 93000 is an invalid webhook key - a permanent failure that retrying will
		// not fix.
		return 0, Permanent(fmt.Errorf("WeCom returned error %d: %s", res.ErrCode, res.ErrMsg))
	}
	return kept, nil
}
