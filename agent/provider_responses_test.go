package agent

import (
	"testing"

	"leviathan/thirdparty/norma/llm"
)

// TestConfigFromOpenAIResponses locks the openai-responses format wiring:
// ConfigFrom resolves the Responses format, strips a full /responses endpoint
// back to the API base, and Provider() round-trips the short name. NewProvider
// must build a working provider for it.
func TestConfigFromOpenAIResponses(t *testing.T) {
	c := ConfigFrom("openai-responses", "gpt-5", "https://gw.example/v1/responses", "sk-x", "")
	if c.Format != llm.FormatOpenAIResponses {
		t.Fatalf("format=%v, want FormatOpenAIResponses", c.Format)
	}
	if c.BaseURL != "https://gw.example/v1" {
		t.Fatalf("base_url=%q, want the /responses suffix stripped", c.BaseURL)
	}
	if c.Provider() != "openai-responses" {
		t.Fatalf("Provider()=%q", c.Provider())
	}
	if !c.Stream { // default streaming preserved
		t.Fatal("Stream should default true")
	}
	if _, err := c.NewProvider(); err != nil {
		t.Fatalf("NewProvider: %v", err)
	}
}

// TestConfigFromKnownProviders locks the provider presets: each id resolves to a
// wire format plus its default endpoint/model when the caller leaves them blank,
// a full endpoint URL has the provider-appropriate suffix stripped, and Provider()
// round-trips the id (deepseek/glm/kiwi are OpenAI-compatible but must not
// collapse to the openai id).
func TestConfigFromKnownProviders(t *testing.T) {
	cases := []struct {
		id       string
		format   llm.Format
		baseURL  string
		model    string
		fullURL  string // a full endpoint URL, must strip to baseURL above
		stripped string // expected BaseURL after stripping fullURL
	}{
		{"anthropic", llm.FormatAnthropic, "https://api.anthropic.com", "claude-opus-4-8", "https://gw.example/v1/messages", "https://gw.example"},
		{"openai", llm.FormatOpenAI, "https://api.openai.com/v1", "gpt-4o", "https://gw.example/v1/chat/completions", "https://gw.example/v1"},
		{"openai-responses", llm.FormatOpenAIResponses, "https://api.openai.com/v1", "gpt-5", "https://gw.example/v1/responses", "https://gw.example/v1"},
		{"deepseek", llm.FormatOpenAI, "https://api.deepseek.com/v1", "deepseek-chat", "https://api.deepseek.com/v1/chat/completions", "https://api.deepseek.com/v1"},
		{"glm", llm.FormatOpenAI, "https://open.bigmodel.cn/api/paas/v4", "glm-4.6", "https://open.bigmodel.cn/api/paas/v4/chat/completions", "https://open.bigmodel.cn/api/paas/v4"},
		{"kiwi", llm.FormatOpenAI, "https://api.llm.kiwi/v1", "auto", "https://api.llm.kiwi/v1/chat/completions", "https://api.llm.kiwi/v1"},
	}
	for _, tc := range cases {
		t.Run(tc.id, func(t *testing.T) {
			// Blank model/base_url → the provider's defaults.
			c := ConfigFrom(tc.id, "", "", "sk-x", "")
			if c.Format != tc.format {
				t.Fatalf("format=%v, want %v", c.Format, tc.format)
			}
			if c.BaseURL != tc.baseURL {
				t.Fatalf("default base_url=%q, want %q", c.BaseURL, tc.baseURL)
			}
			if c.Model != tc.model {
				t.Fatalf("default model=%q, want %q", c.Model, tc.model)
			}
			if c.Provider() != tc.id {
				t.Fatalf("Provider()=%q, want %q", c.Provider(), tc.id)
			}
			// A full endpoint URL is tolerated and normalized to the API base.
			stripped := ConfigFrom(tc.id, tc.model, tc.fullURL, "sk-x", "")
			if stripped.BaseURL != tc.stripped {
				t.Fatalf("base_url=%q, want %q (suffix stripped)", stripped.BaseURL, tc.stripped)
			}
			if _, err := c.NewProvider(); err != nil {
				t.Fatalf("NewProvider: %v", err)
			}
		})
	}
}
