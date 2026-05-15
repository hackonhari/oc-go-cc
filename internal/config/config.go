// Package config handles application configuration loading and validation.
package config

import "encoding/json"

// Config holds the complete application configuration.
//
// Schema (alias-first):
//
//	{
//	  "api_keys":        [{"token":"sk-...","account":"primary"}],  // Phase 1+ key pool
//	  "free_fallback":   {"base_url":"https://opencode.ai/zen/v1","models":[...]},
//	  "model_aliases":   {"sonnet":"deepseek-v4-pro", ...},
//	  "model_configs":   {"deepseek-v4-pro": {...}, ...},   // keyed by model_id
//	  "default_model":   "deepseek-v4-pro",                  // used when request has no model
//	  "fallbacks":       ["kimi-k2.6", "mimo-v2.5-pro"]      // flat chain for any failure
//	}
//
// Legacy fields (APIKey single-string, Models, FallbacksLegacy) are still
// parsed for backward compatibility. APIKey is auto-migrated into APIKeys[0]
// on first load; the legacy field is cleared in the persisted output.
type Config struct {
	// APIKey is the legacy single-key field, retained for migration detection.
	// On first load with a non-empty APIKey and empty APIKeys, the loader
	// wraps it into APIKeys[0] and clears APIKey. New deployments should
	// populate APIKeys directly.
	APIKey       string      `json:"api_key,omitempty"`
	APIKeys      []KeyConfig `json:"api_keys,omitempty"`
	FreeFallback FreeFallback `json:"free_fallback,omitempty"`

	Host         string `json:"host"`
	Port         int    `json:"port"`
	DefaultModel string `json:"default_model,omitempty"`

	// ModelAliases maps human-friendly aliases (sonnet, opus, haiku) and
	// claude-* request names to actual OpenCode Go model_ids.
	ModelAliases map[string]string `json:"model_aliases,omitempty"`

	// ModelConfigs holds per-model parameters keyed by model_id.
	// Replaces the scenario-keyed Models map.
	ModelConfigs map[string]ModelConfig `json:"model_configs,omitempty"`

	// Fallbacks is a flat chain of model_ids tried in order when the primary
	// model fails. Replaces the scenario-keyed FallbacksLegacy map.
	Fallbacks []string `json:"fallbacks,omitempty"`

	// Models / FallbacksLegacy — DEPRECATED scenario-based routing.
	// Kept for backward compatibility with existing config files.
	// New deployments should use ModelConfigs + Fallbacks (flat list) above.
	Models          map[string]ModelConfig   `json:"models,omitempty"`
	FallbacksLegacy map[string][]ModelConfig `json:"fallbacks_legacy,omitempty"`

	OpenCodeGo OpenCodeGoConfig `json:"opencode_go"`
	Logging    LoggingConfig    `json:"logging"`
}

// ModelConfig defines parameters for a specific model.
//
// When stored in ModelConfigs (new schema), Provider and ModelID are derived
// from the map key — they're optional in the JSON and filled in by the loader.
type ModelConfig struct {
	Provider         string          `json:"provider,omitempty"`
	ModelID          string          `json:"model_id,omitempty"`
	Temperature      float64         `json:"temperature"`
	MaxTokens        int             `json:"max_tokens"`
	ContextThreshold int             `json:"context_threshold,omitempty"`
	ReasoningEffort  string          `json:"reasoning_effort,omitempty"`
	Thinking         json.RawMessage `json:"thinking,omitempty"`
}

// HasNewSchema returns true when the alias-first schema is in use.
// New schema is detected by the presence of model_configs.
func (c *Config) HasNewSchema() bool {
	return len(c.ModelConfigs) > 0
}

// LookupModelConfig returns the ModelConfig for a given model_id, with
// Provider and ModelID filled in. Returns ok=false if not configured.
//
// In the new schema, looks up ModelConfigs[modelID].
// Falls back to Models["default"] (legacy schema) when ModelConfigs is empty.
func (c *Config) LookupModelConfig(modelID string) (ModelConfig, bool) {
	if c.HasNewSchema() {
		if cfg, ok := c.ModelConfigs[modelID]; ok {
			cfg.ModelID = modelID
			if cfg.Provider == "" {
				cfg.Provider = "opencode-go"
			}
			return cfg, true
		}
		// Not configured per-model — return a sensible minimal config so the
		// model can still be used (e.g. when a user types a model_id we
		// haven't tuned).
		return ModelConfig{
			Provider:    "opencode-go",
			ModelID:     modelID,
			Temperature: 0.1,
			MaxTokens:   8192,
		}, true
	}
	// Legacy fallback: use Models["default"]'s parameters with the given model_id.
	if def, ok := c.Models["default"]; ok {
		def.ModelID = modelID
		def.Provider = "opencode-go"
		return def, true
	}
	return ModelConfig{}, false
}

// FallbackChain returns the flat fallback chain as ModelConfig entries
// for the given primary model_id. The primary itself is excluded so the
// returned slice can be appended after the primary entry.
//
// In the new schema, uses Fallbacks (flat list).
// Falls back to FallbacksLegacy["default"] for legacy configs.
func (c *Config) FallbackChain(primaryModelID string) []ModelConfig {
	var ids []string
	if c.HasNewSchema() && len(c.Fallbacks) > 0 {
		ids = c.Fallbacks
	}

	chain := make([]ModelConfig, 0, len(ids))
	for _, id := range ids {
		if id == primaryModelID {
			continue
		}
		if cfg, ok := c.LookupModelConfig(id); ok {
			chain = append(chain, cfg)
		}
	}
	return chain
}

// OpenCodeGoConfig holds the upstream OpenCode Go API settings.
type OpenCodeGoConfig struct {
	BaseURL          string `json:"base_url"`
	AnthropicBaseURL string `json:"anthropic_base_url"`
	TimeoutMs        int    `json:"timeout_ms"`
}

// KeyConfig is the declarative seed shape for an upstream API key. The
// state file (key-states.json) mirrors this shape plus runtime fields
// (exhaustedAt, resetDate, lastUsed, usage percentages).
type KeyConfig struct {
	Token   string `json:"token"`
	Account string `json:"account"`
}

// FreeFallback describes the free-tier endpoint + model fallback chain
// engaged when all paid keys are exhausted (or when the request hit
// the /free/v1/* path prefix).
type FreeFallback struct {
	BaseURL string   `json:"base_url"`
	Models  []string `json:"models"`
}

// LoggingConfig controls application logging behavior.
type LoggingConfig struct {
	Level    string `json:"level"`
	Requests bool   `json:"requests"`
}
