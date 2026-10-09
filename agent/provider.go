// Package agent wires real LLM-driven planner and work agents (on top of the
// agent-core SDK) to the dual SQLite graph. See the architecture design doc
// §4.3 (planner) and §4.4 (work agent).
//
// Provider configuration is read from the environment so the system runs with
// any Anthropic- or OpenAI-format endpoint. If no key is configured, FromEnv
// returns ok=false and the exploration engine stays idle (an LLM is required).
package agent

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"leviathan/llmrec"
	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/compaction"
	"github.com/Autumn-27/norma/llm"
	acperm "github.com/Autumn-27/norma/permission"
	"github.com/Autumn-27/norma/transcript"
)

// Config describes the LLM backend resolved from the environment.
type Config struct {
	Format  llm.Format
	BaseURL string
	APIKey  string
	Model   string
	// ProviderName is the provider id this config was resolved from
	// ("anthropic", "openai", "openai-responses", "deepseek", "glm", "kiwi").
	// Provider() surfaces it so logs and the DB format column keep the provider
	// the user picked even when several providers share one wire format. Empty
	// (a config built as a bare struct) falls back to inferring it from Format.
	ProviderName string
	// Proxy routes all LLM requests through the given proxy URL (http/https/socks5,
	// optionally with user:pass@ credentials). Empty means direct — it does NOT
	// fall back to the standard *_PROXY environment variables.
	Proxy string
	// RatePerSecond / RatePerMinute cap the shared request rate across ALL agents
	// using the provider (0 = that window unlimited).
	RatePerSecond float64
	RatePerMinute float64
	// ContextWindowK is the model's context window in K tokens (user-configured),
	// used to size compaction thresholds. 0 = default; see CompactionWindow.
	ContextWindowK int
	// ThinkingType independently controls the thinking "toggle" field (thinking.type):
	//   "" = don't send (default, compatible with models lacking the field);
	//   "disabled" = explicitly off; "enabled" = on. Fully decoupled from
	//   ReasoningEffort — some APIs have no thinking field and activate reasoning
	//   from the intensity alone, so the two are set independently.
	ThinkingType string
	// ReasoningEffort independently controls the thinking "intensity" field:
	//   "" = don't send (default); "low"/"medium"/"high"/"xhigh"/"max".
	//   OpenAI maps it to top-level reasoning_effort; Anthropic to output_config.effort.
	ReasoningEffort string
	// Stream controls whether this profile uses the streaming (SSE) interface.
	// true (default) = streaming; false = truly non-streaming (send stream:false,
	// take one complete JSON via Provider.Complete). Non-streaming sidesteps some
	// gateways' poor SSE implementations (empty frames, dropped thinking fields) at
	// the cost of live progress/token counting. Maps to agentcore.Options.NonStreaming = !Stream.
	Stream bool
	// MaxTokens is the per-reply output cap (tokens). 0 = don't send the field, let
	// the server default decide (historical behavior). Unlike ContextWindowK (the
	// model's total capacity, used only locally to size compaction thresholds), this
	// value is sent with every request. Maps to agentcore.Options.MaxTokens.
	MaxTokens int
	// MaxTokensField picks which request field name carries MaxTokens; only
	// meaningful for format=openai:
	//   "" = max_tokens (default); "max_completion_tokens" = the newer field.
	// OpenAI reasoning models (o-series/GPT-5) accept only the latter and reject
	// max_tokens with unsupported_parameter, while most compatible gateways accept
	// only the former — so it is not auto-inferred; the user picks per endpoint.
	MaxTokensField string
	// SessionHeaderKey, when non-empty, makes every LLM request carry a custom HTTP
	// header whose name is this value and whose value is the CURRENT session's
	// session id (chat session = conv-<id>, worker = exp<x>-worker-i<intent>, etc.;
	// see WorkerSessionID). For gateways that do prompt caching/sticky routing off a
	// session-id header. Empty = don't send. The value is attached to the request
	// context by transcript.WithSessionID and read by the RoundTripper, so one shared
	// provider still emits a different header value per session.
	SessionHeaderKey string
	// Retry is this config's resolved retry parameters (profile override → global
	// policy → built-in default, resolved server-side). See RetryConfig for the three
	// layers; a zero value means the built-in defaults are used unchanged.
	Retry RetryConfig
}

// RetryConfig holds the retry parameters carried with one LLM config. The
// "attempts" semantics are the same at every layer: 0 = built-in default count;
// negative = disable that layer; >0 = use this count. The "interval" semantics:
// 0 = the layer's own exponential backoff; >0 = use this fixed interval instead.
type RetryConfig struct {
	// ConnectAttempts/ConnectInterval: SDK connect retries (connection reset/
	// timeout/429/5xx, before the stream starts), mapped directly to
	// llm.Config.MaxRetries / RetryInterval. Default 3 attempts, 0.5s exponential
	// (capped 8s).
	ConnectAttempts int
	ConnectInterval time.Duration
	// EmptyAttempts/EmptyInterval: SDK empty-response retries (completed but no
	// content block, openai format only), mapped to
	// llm.Config.EmptyResponseRetries / EmptyResponseInterval. Default 2 attempts,
	// same exponential ladder.
	EmptyAttempts int
	EmptyInterval time.Duration
	// StreamAttempts/StreamInterval: same-provider safety-window retries — a layer
	// this project adds on top of the SDK. Only replays a dropped stream/overload/
	// in-stream 429 when NO output has been delivered to the caller yet. The SDK
	// never sees it; it is consumed by server/task_llm.go. Default 2 attempts, 0.5s
	// exponential (capped 4s).
	StreamAttempts int
	StreamInterval time.Duration
}

// compaction window resolution bounds (in K tokens). Below the floor the
// threshold math (window − summary reserve − buffer) would go non-positive and
// compaction would fire every turn; above the cap it would never fire.
const (
	defaultWindowK = 200  // unset → assume a 200K window (Claude default)
	minWindowK     = 32   // floor so effectiveWindow stays comfortably positive
	maxWindowK     = 1000 // cap at 1M tokens (user request)
)

// CompactionWindow returns the model context window in TOKENS for compaction
// thresholds, resolved from the user-configured size (ContextWindowK). 0/unset →
// a 200K default; otherwise clamped to [32K, 1M] so compaction stays effective.
func (c Config) CompactionWindow() int {
	k := c.ContextWindowK
	if k <= 0 {
		k = defaultWindowK
	}
	if k < minWindowK {
		k = minWindowK
	}
	if k > maxWindowK {
		k = maxWindowK
	}
	return k * 1000
}

// compactionConfig builds the agent-core compaction config for a context window
// in tokens. agentcore.NewSession wires the summarizer (same provider) when this
// is set on Options.Compaction.
func compactionConfig(windowTokens int) *compaction.Config {
	if windowTokens <= 0 {
		windowTokens = defaultWindowK * 1000
	}
	return &compaction.Config{ContextWindow: windowTokens}
}

// providerSpec describes a selectable LLM provider: the wire format it speaks,
// the endpoint and model applied when the caller leaves them blank, and the
// endpoint suffix tolerated on a full URL (the provider appends the path itself).
type providerSpec struct {
	format       llm.Format
	defaultBase  string
	defaultModel string
	stripSuffix  string
	// anthropicKey marks which credential FromEnv reads: anthropic providers use
	// ANTHROPIC_API_KEY, the rest use OPENAI_API_KEY.
	anthropicKey bool
}

// providerSpecs lists every provider the UI/API can select. deepseek, glm and
// kiwi are OpenAI-compatible endpoints, so they reuse llm.FormatOpenAI; the
// provider id is what distinguishes them (stored in llm_profiles.format and
// returned by Config.Provider()).
var providerSpecs = map[string]providerSpec{
	"anthropic": {
		format: llm.FormatAnthropic, defaultBase: "https://api.anthropic.com",
		defaultModel: "claude-opus-4-8", stripSuffix: "/v1/messages", anthropicKey: true,
	},
	"openai": {
		format: llm.FormatOpenAI, defaultBase: "https://api.openai.com/v1",
		defaultModel: "gpt-4o", stripSuffix: "/chat/completions",
	},
	"openai-responses": {
		format: llm.FormatOpenAIResponses, defaultBase: "https://api.openai.com/v1",
		defaultModel: "gpt-5", stripSuffix: "/responses",
	},
	"deepseek": {
		format: llm.FormatOpenAI, defaultBase: "https://api.deepseek.com/v1",
		defaultModel: "deepseek-chat", stripSuffix: "/chat/completions",
	},
	"glm": {
		format: llm.FormatOpenAI, defaultBase: "https://open.bigmodel.cn/api/paas/v4",
		defaultModel: "glm-4.6", stripSuffix: "/chat/completions",
	},
	"kiwi": {
		format: llm.FormatOpenAI, defaultBase: "https://api.llm.kiwi/v1",
		defaultModel: "auto", stripSuffix: "/chat/completions",
	},
}

// ProviderIDs returns the selectable provider ids in a stable order. Handy for
// seeding the UI/docs and for validating the provider field.
func ProviderIDs() []string {
	return []string{"anthropic", "openai", "openai-responses", "deepseek", "glm", "kiwi"}
}

// DefaultBaseURL returns the built-in endpoint for a provider id ("" when the id
// is unknown), so callers that talk to the provider outside ConfigFrom — e.g. the
// model-list endpoint — agree with it on the same defaults.
func DefaultBaseURL(provider string) string {
	if spec, ok := providerSpecs[strings.TrimSpace(provider)]; ok {
		return spec.defaultBase
	}
	return ""
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// FromEnv resolves the LLM provider config:
//
//	LEVIATHAN_LLM_PROVIDER = anthropic|openai|openai-responses|deepseek|glm|kiwi
//	                         (default: inferred from which credential is set)
//	LEVIATHAN_LLM_MODEL    = model id        (default: per provider)
//	LEVIATHAN_LLM_BASE_URL = endpoint        (optional; default: per provider)
//	LEVIATHAN_LLM_PROXY    = proxy URL        (optional; http/https/socks5)
//	LEVIATHAN_LLM_API_KEY  = generic credential (optional; used by any provider)
//	ANTHROPIC_API_KEY                          = credential for anthropic
//	OPENAI_API_KEY                             = credential for the OpenAI-compatible providers
func FromEnv() (Config, bool) {
	prov := strings.TrimSpace(os.Getenv("LEVIATHAN_LLM_PROVIDER"))
	anthKey := os.Getenv("ANTHROPIC_API_KEY")
	oaiKey := os.Getenv("OPENAI_API_KEY")
	genericKey := os.Getenv("LEVIATHAN_LLM_API_KEY")

	if prov == "" {
		switch {
		case anthKey != "":
			prov = "anthropic"
		case oaiKey != "":
			prov = "openai"
		default:
			return Config{}, false
		}
	}
	spec, ok := providerSpecs[prov]
	if !ok {
		prov = "anthropic"
		spec = providerSpecs[prov]
	}

	c := Config{
		BaseURL:      strings.TrimRight(strings.TrimSpace(os.Getenv("LEVIATHAN_LLM_BASE_URL")), "/"),
		Model:        os.Getenv("LEVIATHAN_LLM_MODEL"),
		Proxy:        strings.TrimSpace(os.Getenv("LEVIATHAN_LLM_PROXY")),
		ProviderName: prov,
		Format:       spec.format,
		// Streaming by default; LEVIATHAN_LLM_STREAM=false/0/off switches to non-streaming.
		Stream: !isFalsy(os.Getenv("LEVIATHAN_LLM_STREAM")),
	}
	if c.BaseURL == "" {
		c.BaseURL = spec.defaultBase
	}
	if c.Model == "" {
		c.Model = spec.defaultModel
	}
	if spec.anthropicKey {
		c.APIKey = firstNonEmpty(anthKey, genericKey)
	} else {
		c.APIKey = firstNonEmpty(oaiKey, genericKey)
	}
	if c.APIKey == "" {
		return Config{}, false
	}
	return c, true
}

// ConfigFrom builds a Config from UI-provided strings. The provider id selects
// the wire format and supplies the default endpoint/model when the caller leaves
// them blank (see providerSpecs); an unknown id falls back to anthropic. Inputs
// are trimmed and the base URL is normalized to the API base the provider expects
// (the provider appends the endpoint path itself), so a full endpoint URL is
// tolerated.
func ConfigFrom(provider, model, baseURL, apiKey, proxy string) Config {
	c := Config{
		Model:   strings.TrimSpace(model),
		BaseURL: strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		APIKey:  strings.TrimSpace(apiKey),
		Proxy:   strings.TrimSpace(proxy),
		Stream:  true, // default streaming; callers override per profile
	}
	id := strings.TrimSpace(provider)
	spec, ok := providerSpecs[id]
	if !ok {
		id = "anthropic"
		spec = providerSpecs[id]
	}
	c.ProviderName = id
	c.Format = spec.format
	if spec.stripSuffix != "" {
		// provider appends the endpoint path itself; tolerate a full endpoint URL.
		c.BaseURL = strings.TrimRight(strings.TrimSuffix(c.BaseURL, spec.stripSuffix), "/")
	}
	if c.BaseURL == "" {
		c.BaseURL = spec.defaultBase
	}
	if c.Model == "" {
		c.Model = spec.defaultModel
	}
	return c
}

// isFalsy reports whether an env-var string explicitly requests "off". Empty or
// unrecognized → false (so an unset var keeps the streaming default).
func isFalsy(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "0", "false", "off", "no":
		return true
	}
	return false
}

// Provider returns the short provider id ("anthropic"/"openai"/
// "openai-responses"/"deepseek"/"glm"/"kiwi"). It prefers the id the config was
// built from and, for a bare config, infers it from the wire format.
func (c Config) Provider() string {
	if c.ProviderName != "" {
		return c.ProviderName
	}
	switch c.Format {
	case llm.FormatOpenAI:
		return "openai"
	case llm.FormatOpenAIResponses:
		return "openai-responses"
	}
	return "anthropic"
}

// NewProvider builds an llm.Provider from the config. When a rate is set, the
// limiter lives on the single provider instance — so planner + all workers +
// main agent (which share this provider) are bounded by one shared rate limit.
func (c Config) NewProvider() (llm.Provider, error) {
	client, err := quotaAwareHTTPClient(c.Proxy, c.SessionHeaderKey)
	if err != nil {
		return nil, err
	}
	lc := llm.Config{
		Format:     c.Format,
		BaseURL:    c.BaseURL,
		APIKey:     c.APIKey,
		Model:      c.Model,
		HTTPClient: client,
	}
	// The thinking toggle and the reasoning effort are independent and passed
	// through as-is (empty = don't send that field): send only thinking.type, only
	// effort, both, or neither.
	lc.ThinkingType = c.ThinkingType
	lc.ReasoningEffort = c.ReasoningEffort
	// Which request key carries the output cap (empty = max_tokens). The cap VALUE
	// is not set here: it travels per round as agentcore.Options.MaxTokens; the
	// provider only decides which key it goes in.
	lc.MaxTokensField = c.MaxTokensField
	// Retry params share the SDK's semantics (count 0=default/negative=off,
	// interval 0=exponential backoff/>0=fixed); passed through unchanged.
	lc.MaxRetries = c.Retry.ConnectAttempts
	lc.RetryInterval = c.Retry.ConnectInterval
	lc.EmptyResponseRetries = c.Retry.EmptyAttempts
	lc.EmptyResponseInterval = c.Retry.EmptyInterval
	if c.RatePerSecond > 0 || c.RatePerMinute > 0 {
		lc.RateLimit = &llm.RateLimit{PerSecond: c.RatePerSecond, PerMinute: c.RatePerMinute}
	}
	return llm.NewProvider(lc)
}

// IsQuotaExhaustedMessage deliberately recognizes only explicit balance,
// billing, credit, or quota-exhaustion signals. Generic 429/rate-limit text,
// authentication failures, network errors, and server failures are excluded.
var nonFailoverHTTPStatus = regexp.MustCompile(`(?:status(?:\s+code)?|http(?:\s+status)?)\s*[=:]?\s*(?:401|403|5\d\d)\b`)
var transientQuotaLimit = regexp.MustCompile(`(?i)(?:\b(?:rpm|tpm|rpd|qps)\b|quota[_\s-]*metric|rate[_\s-]*limit|too many requests|(?:requests?|tokens?)\s+(?:per|/)\s*(?:second|minute)|(?:per|/)\s*(?:second|minute)\s+(?:requests?|tokens?)|generate[_\s-]*requests[_\s-]*per[_\s-]*(?:minute|second)|tokens?[_\s-]*per[_\s-]*(?:minute|second))`)

func IsQuotaExhaustedMessage(message string) bool {
	message = strings.ToLower(message)
	// Authentication/authorization and provider-side 5xx failures never rotate,
	// even when a gateway happens to echo a quota-looking phrase in the body.
	if nonFailoverHTTPStatus.MatchString(message) {
		return false
	}
	// Provider APIs frequently describe an ordinary rate limit as "quota
	// exceeded", especially Google-style responses containing a quota metric.
	// These limits recover with time and must stay on the current provider.
	if transientQuotaLimit.MatchString(message) {
		return false
	}
	markers := []string{
		"insufficient_quota", "quota_exceeded", "quota exceeded", "quota exhausted",
		"exceeded your current quota", "billing_hard_limit_reached",
		"billing hard limit", "billing_not_active", "credit balance", "insufficient credit",
		"insufficient balance", "balance is too low", "payment required", "status 402",
		"余额不足", "额度不足", "额度已用尽", "欠费",
	}
	for _, marker := range markers {
		if strings.Contains(message, marker) {
			return true
		}
	}
	// gRPC RESOURCE_EXHAUSTED is overloaded for both account quota and ordinary
	// request-rate limiting. Preserve it as an explicit exhaustion signal only
	// when the same error does not identify a transient rate limit.
	return strings.Contains(message, "resource_exhausted") &&
		!strings.Contains(message, "rate limit") &&
		!strings.Contains(message, "too many requests")
}

// quotaAwareTransport preserves Norma's normal retry behavior except for a 429
// whose body explicitly says the account quota/balance is exhausted. Norma's
// retry loop treats every 429 as transient; normalizing only that response to
// 402 lets a task router fail over immediately while retaining the original
// response body for provider-specific classification and audit logs.
type quotaAwareTransport struct {
	base http.RoundTripper
	// sessionHeaderKey, when non-empty, is the HTTP header name each request
	// carries; its value is the session id read from the request context. Empty
	// disables it. See Config.SessionHeaderKey.
	sessionHeaderKey string
}

func (t quotaAwareTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	// Custom session-id header: name is user-configured, value is THIS run's
	// session id (norma stashes it on the context via transcript.WithSessionID).
	// Stable across a session's turns and distinct across sessions — exactly what
	// a session-keyed prompt cache wants. Skipped when no session id is present.
	if t.sessionHeaderKey != "" {
		if sid := transcript.SessionIDFrom(req.Context()); sid != "" {
			req.Header.Set(t.sessionHeaderKey, sid)
		}
	}
	// When LLM recording is on, the Recorder puts a Capture on the context so the
	// raw wire bodies can be persisted. This is the only layer that still sees
	// them: norma builds the request body internally and decodes the SSE response
	// before either reaches the recorder.
	capt := llmrec.CaptureFrom(req.Context())
	capt.SetRequest(requestBodySnapshot(req))

	resp, err := t.base.RoundTrip(req)
	if err != nil || resp == nil {
		return resp, err
	}
	// Tee rather than read: a 200 is an SSE stream that must keep streaming. The
	// 429 branch below reads through this wrapper, so its body lands in the
	// capture before being replaced.
	resp.Body = capt.TeeResponse(resp.StatusCode, resp.Body)

	if resp.StatusCode != http.StatusTooManyRequests {
		return resp, nil
	}
	body, readErr := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	resp.Body = io.NopCloser(bytes.NewReader(body))
	resp.ContentLength = int64(len(body))
	if readErr != nil {
		return resp, nil
	}
	if IsQuotaExhaustedMessage(string(body)) {
		resp.StatusCode = http.StatusPaymentRequired
		resp.Status = "402 Payment Required"
	}
	return resp, nil
}

// requestBodySnapshot copies an outgoing request body without consuming it.
// norma builds every model request from a *bytes.Reader, so net/http populates
// GetBody and the copy has no effect on what gets sent.
func requestBodySnapshot(req *http.Request) string {
	if req.GetBody == nil {
		return ""
	}
	rc, err := req.GetBody()
	if err != nil {
		return ""
	}
	defer rc.Close()
	b, err := io.ReadAll(rc)
	if err != nil {
		return ""
	}
	return string(b)
}

func quotaAwareHTTPClient(proxy, sessionHeaderKey string) (*http.Client, error) {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	proxy = strings.TrimSpace(proxy)
	if proxy == "" {
		transport.Proxy = nil // empty = direct, does NOT fall back to HTTP_PROXY/HTTPS_PROXY env vars
	} else {
		proxyURL, err := url.Parse(proxy)
		if err != nil {
			return nil, fmt.Errorf("llm: invalid proxy %q: %w", proxy, err)
		}
		switch proxyURL.Scheme {
		case "http", "https", "socks5":
		case "":
			return nil, fmt.Errorf("llm: proxy %q missing scheme (use http://, https:// or socks5://)", proxy)
		default:
			return nil, fmt.Errorf("llm: unsupported proxy scheme %q (use http, https or socks5)", proxyURL.Scheme)
		}
		transport.Proxy = http.ProxyURL(proxyURL)
	}
	return &http.Client{Transport: quotaAwareTransport{base: transport, sessionHeaderKey: strings.TrimSpace(sessionHeaderKey)}}, nil
}

// logTestConnection prints the raw HTTP status code(s) and response body of a
// connection test to the server log, so "click Test" leaves a diagnosable trail of
// exactly what the gateway returned — 401 bodies, quota text, empty frames — not
// just the collapsed ok/err the UI shows. Bodies are clipped to keep a chatty
// SSE stream from flooding the log.
func logTestConnection(c Config, capt *llmrec.Capture) {
	attempts := capt.Attempts()
	if len(attempts) == 0 {
		log.Printf("[llm-test] %s / %s @ %s — no HTTP request was sent (config parse or connect failed)",
			c.Provider(), c.Model, c.BaseURL)
		return
	}
	for i, a := range attempts {
		log.Printf("[llm-test] %s / %s @ %s — attempt %d/%d HTTP %d\nresponse body: %s",
			c.Provider(), c.Model, c.BaseURL, i+1, len(attempts), a.Status, clipBody(a.Body))
	}
}

// clipBody trims a wire body for logging. 4K is plenty to show an error JSON or
// the head of an SSE stream while bounding a runaway response.
func clipBody(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "(empty)"
	}
	const max = 4096
	if len(s) > max {
		return s[:max] + fmt.Sprintf("…(truncated, %d bytes total)", len(s))
	}
	return s
}

// TestConnection makes a minimal real completion to verify the provider/model/
// endpoint/key actually work. Returns the round-trip latency and the model's
// reply text.
func TestConnection(ctx context.Context, c Config) (time.Duration, string, error) {
	prov, err := c.NewProvider()
	if err != nil {
		return 0, "", err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	// Capture the raw wire traffic: what a connection test most needs to see is
	// exactly what the gateway returned (status code + body), and norma decodes the
	// response into StreamEvents, losing that. quotaAwareTransport finds this Capture
	// on the context and records each HTTP attempt's status and body.
	ctx, capt := llmrec.NewCapture(ctx)
	defer logTestConnection(c, capt)
	// A connection test is a one-shot path that skips agentcore's session loop, so
	// nothing attaches a session id to the context. For endpoints configured with
	// SessionHeaderKey (e.g. opencode zen requires the x-opencode-session header and
	// 400s with MissingSessionID when it is absent), that causes the "chat works but
	// the Test button 400s" gap. Attach a one-off random session id so the test goes
	// through the same header logic as real chat; endpoints without a SessionHeaderKey
	// ignore it, so there is no side effect.
	ctx = transcript.WithSessionID(ctx, "conntest-"+transcript.NewSessionID())
	start := time.Now()
	// Give MaxTokens enough room: a reasoning model (e.g. deepseek-v4-pro) emits a
	// long burst of thinking before the answer (measured: ~2900 tokens for a single
	// "ping"). With only 32 the model stalls in the thinking phase until it hits the
	// output cap (finish=length) and gets truncated — the test still "passes" (err=nil)
	// but shows a garbled "interrupted/length/resume" state. Give it room to emit OK
	// cleanly (finish=stop). EscalateMaxTokens stays false so a truncation does not
	// retry with a larger cap and loop on resume, wasting tokens.
	reply, err := agentcore.Run(ctx, agentcore.Options{
		Provider:       prov,
		SystemPrompt:   []string{"You are a connection test. Just output the two characters OK; do not think, do not explain, do not add anything else."},
		PermissionMode: acperm.ModeBypass,
		MaxTurns:       1,
		MaxTokens:      8192,
		NonStreaming:   !c.Stream, // test with this profile's real send/receive mode
	}, "ping")
	lat := time.Since(start)
	if err != nil {
		return lat, "", err
	}
	// err==nil is not enough: requests can succeed with the model emitting no text
	// (thinking burns the whole budget, the body is swallowed by a safety policy, a
	// compat layer drops content). In a session such a config simply "says nothing"
	// while the test reports success — exactly the gap this is meant to remove. Any
	// reply with no visible text is a failure.
	reply = strings.TrimSpace(reply)
	if reply == "" {
		return lat, "", fmt.Errorf("model returned no content (request succeeded, but no text was returned)")
	}
	return lat, reply, nil
}
