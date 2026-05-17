package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	defaultConfigPath       = "~/.config/oc-go-cc/config.json"
	defaultHost             = "127.0.0.1"
	defaultPort             = 3456
	defaultBaseURL          = "https://opencode.ai/zen/go/v1/chat/completions"
	defaultAnthropicBaseURL = "https://opencode.ai/zen/go/v1/messages"
	defaultTimeoutMs        = 300000
	defaultLogLevel         = "info"

	// defaultFreeFallbackBaseURL is the anonymous Zen endpoint used when all
	// paid keys are exhausted (or when the request matches /free/v1/*).
	defaultFreeFallbackBaseURL = "https://opencode.ai/zen/v1"
)

// defaultFreeFallbackModels is the ordered list of Zen free-tier models
// tried in fallback. Order matters — deepseek-v4-flash-free first because
// it mirrors the paid Go default model family. nemotron added 2026-05-17
// (NVIDIA 120B reasoning) as a 4th option after a real-net 24h window
// observed the original 3 simultaneously 429-locked.
var defaultFreeFallbackModels = []string{
	"deepseek-v4-flash-free",
	"qwen3.6-plus-free",
	"minimax-m2.5-free",
	"nemotron-3-super-free",
}

// envVarPattern matches ${ENV_VAR} placeholders in config values.
var envVarPattern = regexp.MustCompile(`\$\{([A-Za-z0-9_]+)\}`)

// Load reads configuration from a JSON file and applies environment variable overrides.
// Config path resolution:
//  1. OC_GO_CC_CONFIG env var (explicit override)
//  2. ~/.config/oc-go-cc/config.json (default)
//
// If the loaded config still uses the legacy single api_key field,
// migrateLegacyAPIKey wraps it into APIKeys[0] and persists the migrated
// shape back to disk (with a timestamped backup written first).
func Load() (*Config, error) {
	configPath := resolveConfigPath()

	cfg, err := loadJSON(configPath)
	if err != nil {
		return nil, fmt.Errorf("loading config from %s: %w", configPath, err)
	}

	applyEnvOverrides(cfg)
	applyDefaults(cfg)

	if err := migrateLegacyAPIKey(cfg, configPath); err != nil {
		return nil, fmt.Errorf("migrating legacy api_key: %w", err)
	}

	if err := validate(cfg); err != nil {
		return nil, fmt.Errorf("validating config: %w", err)
	}

	return cfg, nil
}

// migrateLegacyAPIKey wraps a non-empty legacy APIKey into APIKeys[0] if
// APIKeys is currently empty. The original config.json is backed up to
// "<path>.backup.<unix-ts>" before mutation, and the migrated shape is
// persisted back atomically.
//
// Idempotent: if APIKeys is already populated OR APIKey is empty, no-op.
//
// configPath may be empty (in-memory tests); in that case the migration
// runs on the struct only and persistence is skipped.
func migrateLegacyAPIKey(cfg *Config, configPath string) error {
	if len(cfg.APIKeys) > 0 {
		// New schema already populated — clear any stale legacy field.
		cfg.APIKey = ""
		return nil
	}
	if cfg.APIKey == "" {
		return nil
	}

	// Wrap legacy key into pool format.
	cfg.APIKeys = []KeyConfig{
		{Token: cfg.APIKey, Account: "legacy"},
	}
	cfg.APIKey = ""

	// In-memory only (tests) — skip persistence.
	if configPath == "" {
		return nil
	}

	// Backup current on-disk content before mutation.
	original, err := os.ReadFile(configPath)
	if err != nil {
		return fmt.Errorf("read for backup: %w", err)
	}
	backupPath := fmt.Sprintf("%s.backup.%d", configPath, time.Now().Unix())
	if err := os.WriteFile(backupPath, original, 0o600); err != nil {
		return fmt.Errorf("write backup %s: %w", backupPath, err)
	}

	// Persist migrated config atomically.
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal migrated config: %w", err)
	}
	tmpPath := configPath + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0o600); err != nil {
		return fmt.Errorf("write tmp config: %w", err)
	}
	if err := os.Rename(tmpPath, configPath); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("rename migrated config: %w", err)
	}
	return nil
}

// resolveConfigPath determines which config file to load.
func resolveConfigPath() string {
	if path := os.Getenv("OC_GO_CC_CONFIG"); path != "" {
		return path
	}
	return expandHome(defaultConfigPath)
}

// expandHome replaces a leading ~ with the user's home directory.
func expandHome(path string) string {
	if strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return path
		}
		return filepath.Join(home, path[2:])
	}
	return path
}

// loadJSON reads and parses the configuration file.
func loadJSON(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	// Interpolate environment variables before parsing.
	data = []byte(interpolateEnvVars(string(data)))

	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parsing JSON: %w", err)
	}

	return &cfg, nil
}

// interpolateEnvVars replaces ${ENV_VAR} patterns with their actual values.
func interpolateEnvVars(s string) string {
	return envVarPattern.ReplaceAllStringFunc(s, func(match string) string {
		// Extract variable name from ${VAR}
		varName := match[2 : len(match)-1]
		if val := os.Getenv(varName); val != "" {
			return val
		}
		// Leave unchanged if env var is not set
		return match
	})
}

// applyEnvOverrides applies environment variable overrides to the config.
func applyEnvOverrides(cfg *Config) {
	if v := os.Getenv("OC_GO_CC_API_KEY"); v != "" {
		cfg.APIKey = v
	}
	if v := os.Getenv("OC_GO_CC_HOST"); v != "" {
		cfg.Host = v
	}
	if v := os.Getenv("OC_GO_CC_PORT"); v != "" {
		if port, err := strconv.Atoi(v); err == nil {
			cfg.Port = port
		}
	}
	if v := os.Getenv("OC_GO_CC_OPENCODE_URL"); v != "" {
		cfg.OpenCodeGo.BaseURL = v
	}
	if v := os.Getenv("OC_GO_CC_LOG_LEVEL"); v != "" {
		cfg.Logging.Level = v
	}
}

// applyDefaults fills in missing configuration values with sensible defaults.
func applyDefaults(cfg *Config) {
	if cfg.Host == "" {
		cfg.Host = defaultHost
	}
	if cfg.Port == 0 {
		cfg.Port = defaultPort
	}
	if cfg.OpenCodeGo.BaseURL == "" {
		cfg.OpenCodeGo.BaseURL = defaultBaseURL
	}
	if cfg.OpenCodeGo.AnthropicBaseURL == "" {
		cfg.OpenCodeGo.AnthropicBaseURL = defaultAnthropicBaseURL
	}
	if cfg.OpenCodeGo.TimeoutMs == 0 {
		cfg.OpenCodeGo.TimeoutMs = defaultTimeoutMs
	}
	if cfg.Logging.Level == "" {
		cfg.Logging.Level = defaultLogLevel
	}
	if cfg.FreeFallback.BaseURL == "" {
		cfg.FreeFallback.BaseURL = defaultFreeFallbackBaseURL
	}
	if len(cfg.FreeFallback.Models) == 0 {
		cfg.FreeFallback.Models = append([]string(nil), defaultFreeFallbackModels...)
	}
}

// validate checks that all required configuration fields are present.
// Post-migration, at least one key must be present in APIKeys (legacy
// APIKey is allowed only as a transient field cleared by migrateLegacyAPIKey).
func validate(cfg *Config) error {
	if len(cfg.APIKeys) == 0 && cfg.APIKey == "" {
		return fmt.Errorf("at least one api key is required (api_keys[] in config, or legacy api_key, or OC_GO_CC_API_KEY env var)")
	}
	for i, k := range cfg.APIKeys {
		if k.Token == "" {
			return fmt.Errorf("api_keys[%d] missing token", i)
		}
		if k.Account == "" {
			return fmt.Errorf("api_keys[%d] missing account label", i)
		}
	}
	return nil
}
