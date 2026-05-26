package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadJSON(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")

	cfgJSON := `{
		"api_key": "test-key-123",
		"host": "0.0.0.0",
		"port": 8080,
		"opencode_go": {
			"base_url": "https://custom.url/v1",
			"timeout_ms": 60000
		},
		"logging": {
			"level": "debug",
			"requests": true
		}
	}`

	if err := os.WriteFile(cfgPath, []byte(cfgJSON), 0644); err != nil {
		t.Fatalf("failed to write test config: %v", err)
	}

	_ = os.Setenv("OC_GO_CC_CONFIG", cfgPath)
	defer func() { _ = os.Unsetenv("OC_GO_CC_CONFIG") }()

	// Prevent env var API key from overriding test config
	oldAPIKey := os.Getenv("OC_GO_CC_API_KEY")
	_ = os.Unsetenv("OC_GO_CC_API_KEY")
	defer func() { _ = os.Setenv("OC_GO_CC_API_KEY", oldAPIKey) }()

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	// Legacy api_key field is auto-migrated into APIKeys[0] and cleared.
	if cfg.APIKey != "" {
		t.Errorf("APIKey should be cleared after migration, got %q", cfg.APIKey)
	}
	if len(cfg.APIKeys) != 1 || cfg.APIKeys[0].Token != "test-key-123" {
		t.Errorf("APIKeys mismatch: %+v", cfg.APIKeys)
	}
	if cfg.APIKeys[0].Account != "legacy" {
		t.Errorf("APIKeys[0].Account = %q, want legacy", cfg.APIKeys[0].Account)
	}
	if cfg.Host != "0.0.0.0" {
		t.Errorf("Host = %q, want %q", cfg.Host, "0.0.0.0")
	}
	if cfg.Port != 8080 {
		t.Errorf("Port = %d, want %d", cfg.Port, 8080)
	}
	if cfg.OpenCodeGo.BaseURL != "https://custom.url/v1" {
		t.Errorf("BaseURL = %q, want %q", cfg.OpenCodeGo.BaseURL, "https://custom.url/v1")
	}
	if cfg.OpenCodeGo.TimeoutMs != 60000 {
		t.Errorf("TimeoutMs = %d, want %d", cfg.OpenCodeGo.TimeoutMs, 60000)
	}
	if cfg.Logging.Level != "debug" {
		t.Errorf("LogLevel = %q, want %q", cfg.Logging.Level, "debug")
	}
	if !cfg.Logging.Requests {
		t.Error("Logging.Requests = false, want true")
	}
}

func TestLoadMissingAPIKey(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")

	cfgJSON := `{"host": "127.0.0.1"}`
	if err := os.WriteFile(cfgPath, []byte(cfgJSON), 0644); err != nil {
		t.Fatalf("failed to write test config: %v", err)
	}

	_ = os.Setenv("OC_GO_CC_CONFIG", cfgPath)
	defer func() { _ = os.Unsetenv("OC_GO_CC_CONFIG") }()

	// Prevent env var API key from making this test pass incorrectly
	oldAPIKey := os.Getenv("OC_GO_CC_API_KEY")
	_ = os.Unsetenv("OC_GO_CC_API_KEY")
	defer func() { _ = os.Setenv("OC_GO_CC_API_KEY", oldAPIKey) }()

	_, err := Load()
	if err == nil {
		t.Fatal("Load() expected error for missing API key, got nil")
	}
}

func TestEnvOverrides(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")

	cfgJSON := `{"api_key": "file-key"}`
	if err := os.WriteFile(cfgPath, []byte(cfgJSON), 0644); err != nil {
		t.Fatalf("failed to write test config: %v", err)
	}

	_ = os.Setenv("OC_GO_CC_CONFIG", cfgPath)
	_ = os.Setenv("OC_GO_CC_API_KEY", "env-key")
	_ = os.Setenv("OC_GO_CC_HOST", "env-host")
	_ = os.Setenv("OC_GO_CC_PORT", "9999")
	_ = os.Setenv("OC_GO_CC_OPENCODE_URL", "https://env-url/v1")
	_ = os.Setenv("OC_GO_CC_LOG_LEVEL", "warn")
	defer func() {
		_ = os.Unsetenv("OC_GO_CC_CONFIG")
		_ = os.Unsetenv("OC_GO_CC_API_KEY")
		_ = os.Unsetenv("OC_GO_CC_HOST")
		_ = os.Unsetenv("OC_GO_CC_PORT")
		_ = os.Unsetenv("OC_GO_CC_OPENCODE_URL")
		_ = os.Unsetenv("OC_GO_CC_LOG_LEVEL")
	}()

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	// env override sets APIKey, then migration moves it into APIKeys[0] and clears APIKey.
	if cfg.APIKey != "" {
		t.Errorf("APIKey should be cleared after migration, got %q", cfg.APIKey)
	}
	if len(cfg.APIKeys) != 1 || cfg.APIKeys[0].Token != "env-key" {
		t.Errorf("APIKeys after env-override migration: %+v", cfg.APIKeys)
	}
	if cfg.Host != "env-host" {
		t.Errorf("Host = %q, want %q", cfg.Host, "env-host")
	}
	if cfg.Port != 9999 {
		t.Errorf("Port = %d, want %d", cfg.Port, 9999)
	}
	if cfg.OpenCodeGo.BaseURL != "https://env-url/v1" {
		t.Errorf("BaseURL = %q, want %q", cfg.OpenCodeGo.BaseURL, "https://env-url/v1")
	}
	if cfg.Logging.Level != "warn" {
		t.Errorf("LogLevel = %q, want %q", cfg.Logging.Level, "warn")
	}
}

func TestDefaults(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")

	// Minimal config — only API key, everything else should default.
	cfgJSON := `{"api_key": "test-key"}`
	if err := os.WriteFile(cfgPath, []byte(cfgJSON), 0644); err != nil {
		t.Fatalf("failed to write test config: %v", err)
	}

	_ = os.Setenv("OC_GO_CC_CONFIG", cfgPath)
	defer func() { _ = os.Unsetenv("OC_GO_CC_CONFIG") }()

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	if cfg.Host != defaultHost {
		t.Errorf("Host = %q, want %q", cfg.Host, defaultHost)
	}
	if cfg.Port != defaultPort {
		t.Errorf("Port = %d, want %d", cfg.Port, defaultPort)
	}
	if cfg.OpenCodeGo.BaseURL != defaultBaseURL {
		t.Errorf("BaseURL = %q, want %q", cfg.OpenCodeGo.BaseURL, defaultBaseURL)
	}
	if cfg.OpenCodeGo.TimeoutMs != defaultTimeoutMs {
		t.Errorf("TimeoutMs = %d, want %d", cfg.OpenCodeGo.TimeoutMs, defaultTimeoutMs)
	}
	if cfg.Logging.Level != defaultLogLevel {
		t.Errorf("LogLevel = %q, want %q", cfg.Logging.Level, defaultLogLevel)
	}
}

func TestInterpolateEnvVars(t *testing.T) {
	_ = os.Setenv("TEST_SECRET", "my-secret-value")
	defer func() { _ = os.Unsetenv("TEST_SECRET") }()

	input := `{"api_key": "${TEST_SECRET}", "host": "${UNSET_VAR:-fallback}"}`
	result := interpolateEnvVars(input)

	want := `{"api_key": "my-secret-value", "host": "${UNSET_VAR:-fallback}"}`
	if result != want {
		t.Errorf("interpolateEnvVars() = %q, want %q", result, want)
	}
}

func TestMigrate_LegacyAPIKeyWrapsAndBacksUp(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	originalJSON := `{"api_key": "sk-legacy-xyz", "host": "127.0.0.1"}`
	if err := os.WriteFile(cfgPath, []byte(originalJSON), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	_ = os.Setenv("OC_GO_CC_CONFIG", cfgPath)
	defer os.Unsetenv("OC_GO_CC_CONFIG")
	oldEnvKey := os.Getenv("OC_GO_CC_API_KEY")
	_ = os.Unsetenv("OC_GO_CC_API_KEY")
	defer os.Setenv("OC_GO_CC_API_KEY", oldEnvKey)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.APIKey != "" {
		t.Errorf("legacy APIKey should be cleared, got %q", cfg.APIKey)
	}
	if len(cfg.APIKeys) != 1 || cfg.APIKeys[0].Token != "sk-legacy-xyz" {
		t.Errorf("APIKeys mismatch: %+v", cfg.APIKeys)
	}

	// Backup file exists.
	matches, _ := filepath.Glob(cfgPath + ".backup.*")
	if len(matches) != 1 {
		t.Errorf("expected one backup file, got %d: %v", len(matches), matches)
	}

	// Backup contains the original legacy shape.
	backupData, _ := os.ReadFile(matches[0])
	if !strings.Contains(string(backupData), `"api_key": "sk-legacy-xyz"`) {
		t.Errorf("backup missing original api_key: %s", backupData)
	}

	// Persisted config now has api_keys[] and no api_key field.
	persisted, _ := os.ReadFile(cfgPath)
	if strings.Contains(string(persisted), `"api_key":`) && !strings.Contains(string(persisted), `"api_keys":`) {
		t.Errorf("persisted should have api_keys, not api_key: %s", persisted)
	}
}

func TestMigrate_Idempotent(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	originalJSON := `{"api_keys": [{"token": "sk-1", "account": "primary"}]}`
	if err := os.WriteFile(cfgPath, []byte(originalJSON), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	_ = os.Setenv("OC_GO_CC_CONFIG", cfgPath)
	defer os.Unsetenv("OC_GO_CC_CONFIG")
	oldEnvKey := os.Getenv("OC_GO_CC_API_KEY")
	_ = os.Unsetenv("OC_GO_CC_API_KEY")
	defer os.Setenv("OC_GO_CC_API_KEY", oldEnvKey)

	_, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	// No backup file should have been written (migration was a no-op).
	matches, _ := filepath.Glob(cfgPath + ".backup.*")
	if len(matches) != 0 {
		t.Errorf("idempotent migration should not write backup, got %d files", len(matches))
	}
}

func TestFreeFallback_DefaultsInjected(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	originalJSON := `{"api_keys": [{"token": "sk-1", "account": "primary"}]}`
	_ = os.WriteFile(cfgPath, []byte(originalJSON), 0o644)

	_ = os.Setenv("OC_GO_CC_CONFIG", cfgPath)
	defer os.Unsetenv("OC_GO_CC_CONFIG")
	oldEnvKey := os.Getenv("OC_GO_CC_API_KEY")
	_ = os.Unsetenv("OC_GO_CC_API_KEY")
	defer os.Setenv("OC_GO_CC_API_KEY", oldEnvKey)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.FreeFallback.BaseURL != defaultFreeFallbackBaseURL {
		t.Errorf("BaseURL = %q, want %q", cfg.FreeFallback.BaseURL, defaultFreeFallbackBaseURL)
	}
	if len(cfg.FreeFallback.Models) != len(defaultFreeFallbackModels) {
		t.Errorf("Models count = %d, want %d", len(cfg.FreeFallback.Models), len(defaultFreeFallbackModels))
	}
	if cfg.FreeFallback.Models[0] != "deepseek-v4-flash-free" {
		t.Errorf("first free model = %q, want deepseek-v4-flash-free", cfg.FreeFallback.Models[0])
	}
	// nemotron-3-super-free must be present — confirms 2026-05-17 AC4
	// (defense against simultaneous 429 across the original 3).
	foundNemotron := false
	for _, m := range cfg.FreeFallback.Models {
		if m == "nemotron-3-super-free" {
			foundNemotron = true
			break
		}
	}
	if !foundNemotron {
		t.Errorf("nemotron-3-super-free missing from default Models: %v", cfg.FreeFallback.Models)
	}
}

func TestFreeFallback_UserOverrideRespected(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	originalJSON := `{
		"api_keys": [{"token": "sk-1", "account": "primary"}],
		"free_fallback": {
			"base_url": "https://custom/v1",
			"models": ["custom-model-free"]
		}
	}`
	_ = os.WriteFile(cfgPath, []byte(originalJSON), 0o644)

	_ = os.Setenv("OC_GO_CC_CONFIG", cfgPath)
	defer os.Unsetenv("OC_GO_CC_CONFIG")
	oldEnvKey := os.Getenv("OC_GO_CC_API_KEY")
	_ = os.Unsetenv("OC_GO_CC_API_KEY")
	defer os.Setenv("OC_GO_CC_API_KEY", oldEnvKey)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.FreeFallback.BaseURL != "https://custom/v1" {
		t.Errorf("user override not respected: got %q", cfg.FreeFallback.BaseURL)
	}
	if len(cfg.FreeFallback.Models) != 1 || cfg.FreeFallback.Models[0] != "custom-model-free" {
		t.Errorf("user models override not respected: %+v", cfg.FreeFallback.Models)
	}
}

func TestValidate_RejectsEmptyKeyEntries(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	// Key with empty token — should fail validation.
	originalJSON := `{"api_keys": [{"token": "", "account": "broken"}]}`
	_ = os.WriteFile(cfgPath, []byte(originalJSON), 0o644)

	_ = os.Setenv("OC_GO_CC_CONFIG", cfgPath)
	defer os.Unsetenv("OC_GO_CC_CONFIG")
	oldEnvKey := os.Getenv("OC_GO_CC_API_KEY")
	_ = os.Unsetenv("OC_GO_CC_API_KEY")
	defer os.Setenv("OC_GO_CC_API_KEY", oldEnvKey)

	_, err := Load()
	if err == nil {
		t.Fatal("expected validation error for empty token")
	}
}

func TestMultiProvider_LoadAndResolve(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")

	cfgJSON := `{
		"providers": {
			"opencode": {
				"base_url": "https://opencode.ai/zen/go/v1/chat/completions",
				"protocol": "openai",
				"models": ["deepseek-v4-pro", "deepseek-v4-flash", "kimi-k2.6"],
				"api_keys": [{"token": "sk-oc-1", "account": "primary"}],
				"enable_key_pool": true
			},
			"commandcode": {
				"base_url": "https://api.commandcode.ai/provider/v1",
				"protocol": "openai",
				"models": ["deepseek/deepseek-v4-pro", "deepseek/deepseek-v4-flash"],
				"api_keys": [{"token": "user_cc_1", "account": "primary"}]
			},
			"kimi": {
				"base_url": "https://api.kimi.com/coding",
				"anthropic_base_url": "https://api.kimi.com/coding",
				"protocol": "anthropic",
				"models": ["kimi-for-coding", "kimi-k2.6"]
			}
		}
	}`

	if err := os.WriteFile(cfgPath, []byte(cfgJSON), 0644); err != nil {
		t.Fatalf("failed to write test config: %v", err)
	}

	_ = os.Setenv("OC_GO_CC_CONFIG", cfgPath)
	defer func() { _ = os.Unsetenv("OC_GO_CC_CONFIG") }()

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	if !cfg.HasProviders() {
		t.Fatal("HasProviders() = false, want true")
	}

	// Provider names populated from map keys.
	if cfg.Providers["opencode"].Name != "opencode" {
		t.Errorf("opencode.Name = %q, want opencode", cfg.Providers["opencode"].Name)
	}
	if cfg.Providers["commandcode"].Name != "commandcode" {
		t.Errorf("commandcode.Name = %q, want commandcode", cfg.Providers["commandcode"].Name)
	}
	if cfg.Providers["kimi"].Name != "kimi" {
		t.Errorf("kimi.Name = %q, want kimi", cfg.Providers["kimi"].Name)
	}

	// ResolveProvider
	pc, ok := cfg.ResolveProvider("deepseek-v4-pro")
	if !ok {
		t.Fatal("ResolveProvider(deepseek-v4-pro) not found")
	}
	if pc.Name != "opencode" {
		t.Errorf("ResolveProvider resolved to %q, want opencode", pc.Name)
	}

	pc, ok = cfg.ResolveProvider("deepseek/deepseek-v4-pro")
	if !ok {
		t.Fatal("ResolveProvider(deepseek/deepseek-v4-pro) not found")
	}
	if pc.Name != "commandcode" {
		t.Errorf("ResolveProvider resolved to %q, want commandcode", pc.Name)
	}

	pc, ok = cfg.ResolveProvider("kimi-for-coding")
	if !ok {
		t.Fatal("ResolveProvider(kimi-for-coding) not found")
	}
	if pc.Name != "kimi" {
		t.Errorf("ResolveProvider resolved to %q, want kimi", pc.Name)
	}

	// Unknown model
	_, ok = cfg.ResolveProvider("nonexistent-model")
	if ok {
		t.Fatal("ResolveProvider(nonexistent-model) should not be found")
	}
}

func TestMultiProvider_Validation(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")

	tests := []struct {
		name    string
		json    string
		wantErr string
	}{
		{
			name:    "missing base_url",
			json:    `{"providers": {"bad": {"protocol": "openai"}}}`,
			wantErr: "base_url is required",
		},
		{
			name:    "invalid protocol",
			json:    `{"providers": {"bad": {"base_url": "https://x.com", "protocol": "grpc"}}}`,
			wantErr: "protocol must be 'openai' or 'anthropic'",
		},
		{
			name:    "key pool without keys",
			json:    `{"providers": {"bad": {"base_url": "https://x.com", "protocol": "openai", "enable_key_pool": true}}}`,
			wantErr: "enable_key_pool requires at least one api_key",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := os.WriteFile(cfgPath, []byte(tt.json), 0644); err != nil {
				t.Fatalf("write: %v", err)
			}
			_ = os.Setenv("OC_GO_CC_CONFIG", cfgPath)
			defer func() { _ = os.Unsetenv("OC_GO_CC_CONFIG") }()

			_, err := Load()
			if err == nil {
				t.Fatal("expected validation error, got nil")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error = %q, want containing %q", err.Error(), tt.wantErr)
			}
		})
	}
}

func TestMultiProvider_BackwardCompat(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")

	// Legacy config — no providers field.
	cfgJSON := `{"api_key": "sk-legacy", "host": "127.0.0.1", "opencode_go": {"base_url": "https://custom/v1"}}`
	if err := os.WriteFile(cfgPath, []byte(cfgJSON), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}

	_ = os.Setenv("OC_GO_CC_CONFIG", cfgPath)
	defer func() { _ = os.Unsetenv("OC_GO_CC_CONFIG") }()
	oldEnvKey := os.Getenv("OC_GO_CC_API_KEY")
	_ = os.Unsetenv("OC_GO_CC_API_KEY")
	defer func() { _ = os.Setenv("OC_GO_CC_API_KEY", oldEnvKey) }()

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	if cfg.HasProviders() {
		t.Fatal("HasProviders() = true for legacy config, want false")
	}
	if cfg.OpenCodeGo.BaseURL != "https://custom/v1" {
		t.Errorf("BaseURL = %q, want https://custom/v1", cfg.OpenCodeGo.BaseURL)
	}
}

func TestExpandHome(t *testing.T) {
	home, _ := os.UserHomeDir()

	tests := []struct {
		input string
		want  string
	}{
		{"~/some/path", filepath.Join(home, "some/path")},
		{"/absolute/path", "/absolute/path"},
		{"relative/path", "relative/path"},
	}

	for _, tt := range tests {
		got := expandHome(tt.input)
		if got != tt.want {
			t.Errorf("expandHome(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}
