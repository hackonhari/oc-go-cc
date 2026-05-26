// Package router defines HTTP route registration and middleware chaining,
// as well as model selection based on request scenarios.
package router

import (
	"fmt"
	"strings"

	"oc-go-cc/internal/config"
)

// ModelRouter handles model selection based on scenarios.
type ModelRouter struct {
	config *config.Config
}

// NewModelRouter creates a new model router.
func NewModelRouter(cfg *config.Config) *ModelRouter {
	return &ModelRouter{config: cfg}
}

// RouteResult contains the selected model and fallback chain.
type RouteResult struct {
	Primary   config.ModelConfig
	Fallbacks []config.ModelConfig
	Scenario  Scenario
}

// ResolveExplicitModel attempts to resolve a requested model name to a configured model.
//
// Resolution order:
//  1. Check `model_aliases` map (e.g. "sonnet" -> "deepseek-v4-pro")
//  2. Check Anthropic-style names (claude-3-5-sonnet, claude-haiku, etc.) for sonnet/opus/haiku patterns
//  4. Check if the requested name matches a known OpenCode Go model_id directly
//  4. Fall back to default_model from config (when configured)
//  5. Return found=false to signal scenario-based routing should be used (legacy)
//
// Returns:
//
//	model — the resolved ModelConfig (only valid when found=true)
//	found — true if explicit resolution succeeded
func (r *ModelRouter) ResolveExplicitModel(requested string) (config.ModelConfig, bool) {
	// Claude Code extended-context pin: e.g. deepseek-v4-pro[1m] or claude-opus-4-7[1m].
	// Strip the suffix for OpenCode model_id resolution while Claude uses [1m] locally
	// for 1M compaction/UI. See https://code.claude.com/en/model-config#extended-context
	requested = stripExtendedContextSuffix(strings.TrimSpace(requested))

	lower := strings.ToLower(requested)

	// 1. Direct alias lookup
	if r.config.ModelAliases != nil && lower != "" {
		if target, ok := r.config.ModelAliases[lower]; ok {
			return r.buildModelConfig(target), true
		}
	}

	// 2. Check provider models BEFORE Anthropic pattern matching.
	// When multi-provider is configured, a model like "claude-sonnet-4-6"
	// should route directly to the provider that owns it, not get caught
	// by the "sonnet" pattern and mapped to an opencode alias.
	if r.config.HasProviders() && lower != "" {
		for _, prov := range r.config.Providers {
			for _, m := range prov.Models {
				if strings.EqualFold(m, lower) {
					return r.buildModelConfig(lower), true
				}
			}
		}
	}

	// 3. Anthropic-style name patterns -> map via aliases if defined
	if r.config.ModelAliases != nil && lower != "" {
		switch {
		case strings.Contains(lower, "sonnet"):
			if target, ok := r.config.ModelAliases["sonnet"]; ok {
				return r.buildModelConfig(target), true
			}
		case strings.Contains(lower, "opus"):
			if target, ok := r.config.ModelAliases["opus"]; ok {
				return r.buildModelConfig(target), true
			}
		case strings.Contains(lower, "haiku"):
			if target, ok := r.config.ModelAliases["haiku"]; ok {
				return r.buildModelConfig(target), true
			}
		}
	}

	// 4. Direct model_id match (e.g. user passes "deepseek-v4-pro" or "kimi-k2.6")
	if lower != "" && isKnownModelID(lower) {
		return r.buildModelConfig(lower), true
	}

	// (moved to step 2 above)
	// direct resolution. This prevents models
	// like "gemini-3.1-flash-lite" from being caught by Anthropic patterns.
	// Provider model check moved to step 2 above

	// 5. Fall back to default_model when configured (new schema only)
	if r.config.HasNewSchema() && r.config.DefaultModel != "" {
		return r.buildModelConfig(r.config.DefaultModel), true
	}

	return config.ModelConfig{}, false
}

// buildModelConfig builds a ModelConfig for a target model_id.
//
// New schema: looks up parameters in ModelConfigs[modelID].
// Legacy schema: inherits from Models["default"] and overrides ModelID.
func (r *ModelRouter) buildModelConfig(modelID string) config.ModelConfig {
	if cfg, ok := r.config.LookupModelConfig(modelID); ok {
		return cfg
	}
	return config.ModelConfig{
		Provider:    "opencode-go",
		ModelID:     modelID,
		Temperature: 0.1,
		MaxTokens:   8192,
	}
}

// isKnownModelID returns true if the given lowercase model id matches a known
// OpenCode Go model identifier.
func isKnownModelID(id string) bool {
	known := []string{
		"glm-5", "glm-5.1",
		"kimi-k2.5", "kimi-k2.6",
		"mimo-v2-pro", "mimo-v2-omni", "mimo-v2.5", "mimo-v2.5-pro",
		"minimax-m2.5", "minimax-m2.7",
		"deepseek-v4-pro", "deepseek-v4-flash",
		"qwen3.5-plus", "qwen3.6-plus",
	}
	for _, k := range known {
		if id == k {
			return true
		}
	}
	return false
}

// stripExtendedContextSuffix removes Claude's "[1m]" extended-context marker from
// a requested model name, if present (case-insensitive on the suffix).
func stripExtendedContextSuffix(s string) string {
	s = strings.TrimSpace(s)
	lower := strings.ToLower(s)
	const suf = "[1m]"
	idx := strings.LastIndex(lower, suf)
	if idx < 0 || idx+len(suf) != len(s) {
		return s
	}
	return strings.TrimSpace(s[:idx])
}

// Route determines which model to use for a request.
//
// When a non-empty requestedModel is provided, the router first attempts to honor it
// via ResolveExplicitModel (alias, anthropic-name pattern, or direct model_id).
// In the new schema, fallbacks come from the flat r.config.Fallbacks list.
// In legacy schema, falls back to scenario-based detection.
func (r *ModelRouter) Route(messages []MessageContent, tokenCount int, requestedModel string) (RouteResult, error) {
	// Try explicit resolution first
	if primary, ok := r.ResolveExplicitModel(requestedModel); ok {
		return RouteResult{
			Primary:   primary,
			Fallbacks: r.config.FallbackChain(primary.ModelID),
			Scenario:  Scenario("explicit"),
		}, nil
	}

	// Legacy path: scenario-based routing
	result := DetectScenario(messages, tokenCount, r.config)

	primary, ok := r.config.Models[string(result.Scenario)]
	if !ok {
		primary, ok = r.config.Models["default"]
		if !ok {
			return RouteResult{}, fmt.Errorf("no default model configured")
		}
	}

	fallbacks := r.config.FallbacksLegacy[string(result.Scenario)]
	if len(fallbacks) == 0 {
		fallbacks = r.config.FallbacksLegacy["default"]
	}

	return RouteResult{
		Primary:   primary,
		Fallbacks: fallbacks,
		Scenario:  result.Scenario,
	}, nil
}

// GetModelChain returns the full chain of models to try (primary + fallbacks).
func (rr *RouteResult) GetModelChain() []config.ModelConfig {
	chain := []config.ModelConfig{rr.Primary}
	chain = append(chain, rr.Fallbacks...)
	return chain
}

// RouteForStreaming determines which model to use for streaming requests.
// Prioritizes fast TTFT (time-to-first-token) over capability.
//
// When a non-empty requestedModel is provided, explicit resolution is honored first
// (consistent with Route), giving callers full control over streaming model choice.
func (r *ModelRouter) RouteForStreaming(messages []MessageContent, tokenCount int, requestedModel string) RouteResult {
	// Honor explicit model selection for streaming too
	if primary, ok := r.ResolveExplicitModel(requestedModel); ok {
		return RouteResult{
			Primary:   primary,
			Fallbacks: r.config.FallbackChain(primary.ModelID),
			Scenario:  Scenario("explicit"),
		}
	}

	// Legacy path: scenario-based streaming routing
	result := RouteForStreaming(messages, tokenCount, r.config)

	primary, ok := r.config.Models[string(result.Scenario)]
	if !ok {
		primary, ok = r.config.Models["fast"]
		if !ok {
			primary = r.config.Models["default"]
		}
	}

	fallbacks := r.config.FallbacksLegacy[string(result.Scenario)]
	if len(fallbacks) == 0 {
		fallbacks = r.config.FallbacksLegacy["fast"]
	}

	return RouteResult{
		Primary:   primary,
		Fallbacks: fallbacks,
		Scenario:  result.Scenario,
	}
}
