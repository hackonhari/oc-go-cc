package router

import (
	"testing"

	"oc-go-cc/internal/config"
)

func newTestRouter() *ModelRouter {
	cfg := &config.Config{
		DefaultModel: "deepseek-v4-pro",
		ModelAliases: map[string]string{
			"sonnet":       "deepseek-v4-pro",
			"opus":         "kimi-k2.6",
			"haiku":        "deepseek-v4-flash",
			"reasoning":    "deepseek-v4-pro",
			"fast":         "deepseek-v4-flash",
			"long-context": "minimax-m2.7",
		},
		ModelConfigs: map[string]config.ModelConfig{
			"deepseek-v4-pro": {
				Temperature: 0.1,
				MaxTokens:   8192,
			},
			"deepseek-v4-flash": {
				Temperature: 0.1,
				MaxTokens:   4096,
			},
			"kimi-k2.6": {
				Temperature: 0.7,
				MaxTokens:   4096,
			},
			"minimax-m2.7": {
				Temperature: 0.7,
				MaxTokens:   16384,
			},
		},
		Fallbacks: []string{"kimi-k2.6", "mimo-v2.5-pro"},
	}
	return NewModelRouter(cfg)
}

func TestResolveExplicitModel_DirectAlias(t *testing.T) {
	r := newTestRouter()

	cases := []struct {
		alias    string
		expected string
	}{
		{"sonnet", "deepseek-v4-pro"},
		{"opus", "kimi-k2.6"},
		{"haiku", "deepseek-v4-flash"},
		{"reasoning", "deepseek-v4-pro"},
		{"fast", "deepseek-v4-flash"},
		{"long-context", "minimax-m2.7"},
	}

	for _, c := range cases {
		got, ok := r.ResolveExplicitModel(c.alias)
		if !ok {
			t.Errorf("alias %q expected to resolve, got not found", c.alias)
			continue
		}
		if got.ModelID != c.expected {
			t.Errorf("alias %q expected %q, got %q", c.alias, c.expected, got.ModelID)
		}
	}
}

func TestResolveExplicitModel_AnthropicNamePatterns(t *testing.T) {
	r := newTestRouter()

	cases := []struct {
		requested string
		expected  string
	}{
		{"claude-3-5-sonnet-20241022", "deepseek-v4-pro"},
		{"claude-3-5-sonnet", "deepseek-v4-pro"},
		{"claude-sonnet-4-20250514", "deepseek-v4-pro"},
		{"claude-3-opus-20240229", "kimi-k2.6"},
		{"claude-opus-4", "kimi-k2.6"},
		{"claude-3-haiku-20240307", "deepseek-v4-flash"},
		{"claude-haiku-3-5", "deepseek-v4-flash"},
	}

	for _, c := range cases {
		got, ok := r.ResolveExplicitModel(c.requested)
		if !ok {
			t.Errorf("%q expected to resolve, got not found", c.requested)
			continue
		}
		if got.ModelID != c.expected {
			t.Errorf("%q expected %q, got %q", c.requested, c.expected, got.ModelID)
		}
	}
}

func TestResolveExplicitModel_DirectModelID(t *testing.T) {
	r := newTestRouter()

	cases := []string{
		"deepseek-v4-pro",
		"deepseek-v4-flash",
		"kimi-k2.6",
		"glm-5.1",
		"qwen3.6-plus",
		"minimax-m2.7",
	}

	for _, c := range cases {
		got, ok := r.ResolveExplicitModel(c)
		if !ok {
			t.Errorf("model_id %q expected to resolve directly, got not found", c)
			continue
		}
		if got.ModelID != c {
			t.Errorf("model_id %q expected %q, got %q", c, c, got.ModelID)
		}
	}
}

func TestResolveExplicitModel_UnknownFallsBackToDefault(t *testing.T) {
	// With the new schema (default_model set), unknown models resolve to
	// the configured default_model. This avoids surprising routing failures
	// when clients send unfamiliar model names.
	r := newTestRouter()

	cases := []string{
		"",
		"unknown-model",
		"gpt-4-turbo",
		"some-random-string",
	}

	for _, c := range cases {
		got, ok := r.ResolveExplicitModel(c)
		if !ok {
			t.Errorf("expected %q to resolve to default, but didn't", c)
			continue
		}
		if got.ModelID != "deepseek-v4-pro" {
			t.Errorf("expected %q -> deepseek-v4-pro (default), got %q", c, got.ModelID)
		}
	}
}

func TestResolveExplicitModel_UnknownNoDefaultDoesNotResolve(t *testing.T) {
	// Without default_model + without new schema, unknown should fail.
	cfg := &config.Config{
		Models: map[string]config.ModelConfig{
			"default": {Provider: "opencode-go", ModelID: "kimi-k2.6"},
		},
	}
	r := NewModelRouter(cfg)

	if _, ok := r.ResolveExplicitModel("unknown-model"); ok {
		t.Errorf("expected unknown to NOT resolve in legacy schema without default_model")
	}
}

func TestResolveExplicitModel_CaseInsensitive(t *testing.T) {
	r := newTestRouter()

	cases := []struct {
		requested string
		expected  string
	}{
		{"SONNET", "deepseek-v4-pro"},
		{"Sonnet", "deepseek-v4-pro"},
		{"Claude-3-5-SONNET-latest", "deepseek-v4-pro"},
	}

	for _, c := range cases {
		got, ok := r.ResolveExplicitModel(c.requested)
		if !ok {
			t.Errorf("%q expected to resolve case-insensitively, got not found", c.requested)
			continue
		}
		if got.ModelID != c.expected {
			t.Errorf("%q expected %q, got %q", c.requested, c.expected, got.ModelID)
		}
	}
}

func TestResolveExplicitModel_Strips1mSuffix(t *testing.T) {
	r := newTestRouter()
	got, ok := r.ResolveExplicitModel("deepseek-v4-pro[1m]")
	if !ok {
		t.Fatal("expected deepseek-v4-pro[1m] to resolve after stripping [1m]")
	}
	if got.ModelID != "deepseek-v4-pro" {
		t.Errorf("ModelID = %q, want deepseek-v4-pro", got.ModelID)
	}
	got2, ok := r.ResolveExplicitModel("DEEPSEEK-V4-PRO[1M]")
	if !ok || got2.ModelID != "deepseek-v4-pro" {
		t.Errorf("case variant: ok=%v ModelID=%q", ok, got2.ModelID)
	}
}

func TestRoute_HonorsExplicitModel(t *testing.T) {
	r := newTestRouter()

	messages := []MessageContent{
		{Role: "user", Content: "hello"},
	}

	result, err := r.Route(messages, 100, "sonnet")
	if err != nil {
		t.Fatalf("Route returned error: %v", err)
	}

	if result.Primary.ModelID != "deepseek-v4-pro" {
		t.Errorf("expected sonnet -> deepseek-v4-pro, got %q", result.Primary.ModelID)
	}
	if result.Scenario != "explicit" {
		t.Errorf("expected scenario=explicit, got %q", result.Scenario)
	}
}

func TestRoute_UsesDefaultModelWhenNoRequest(t *testing.T) {
	r := newTestRouter()

	messages := []MessageContent{
		{Role: "user", Content: "hello"},
	}

	// Empty request should resolve to DefaultModel via the new schema fallback
	result, err := r.Route(messages, 100, "")
	if err != nil {
		t.Fatalf("Route returned error: %v", err)
	}

	if result.Primary.ModelID != "deepseek-v4-pro" {
		t.Errorf("expected default_model -> deepseek-v4-pro, got %q", result.Primary.ModelID)
	}
}

func TestRouteForStreaming_HonorsExplicitModel(t *testing.T) {
	r := newTestRouter()

	messages := []MessageContent{
		{Role: "user", Content: "hello"},
	}

	result := r.RouteForStreaming(messages, 100, "haiku")

	if result.Primary.ModelID != "deepseek-v4-flash" {
		t.Errorf("expected haiku -> deepseek-v4-flash, got %q", result.Primary.ModelID)
	}
}
