package server

import (
	"reflect"
	"testing"

	"oc-go-cc/internal/config"
	"oc-go-cc/internal/keypool"
)

// TestResolveFreeFallback_StateFileWinsWhenNonEmpty validates the cycle 2
// fix: when key-states.json has any free_fallback.models, it takes
// precedence over config.json defaults. Operators can edit the state file
// at runtime; the hot-reload picks up paid key changes, and a proxy
// restart (or future hot-reload for free_fallback) picks up free model
// changes — all without touching config.json.
func TestResolveFreeFallback_StateFileWinsWhenNonEmpty(t *testing.T) {
	cfgFB := config.FreeFallback{
		BaseURL: "https://config-url/v1",
		Models:  []string{"config-model-1", "config-model-2"},
	}
	stateFB := keypool.FreeFallback{
		BaseURL: "https://state-url/v1",
		Models:  []string{"state-model-a", "state-model-b", "state-model-c"},
	}

	baseURL, models, source := resolveFreeFallback(cfgFB, stateFB)

	if source != "state-file" {
		t.Errorf("source = %q, want state-file", source)
	}
	if baseURL != "https://state-url/v1" {
		t.Errorf("baseURL = %q, want state-file URL", baseURL)
	}
	wantModels := []string{"state-model-a", "state-model-b", "state-model-c"}
	if !reflect.DeepEqual(models, wantModels) {
		t.Errorf("models = %v, want %v", models, wantModels)
	}
}

// TestResolveFreeFallback_EmptyStateFallsBackToConfig — fresh install
// scenario where the proxy has never run, state file has no free_fallback.
// Routing must come from config.json defaults.
func TestResolveFreeFallback_EmptyStateFallsBackToConfig(t *testing.T) {
	cfgFB := config.FreeFallback{
		BaseURL: "https://config-url/v1",
		Models:  []string{"config-only"},
	}
	stateFB := keypool.FreeFallback{} // zero — empty Models

	baseURL, models, source := resolveFreeFallback(cfgFB, stateFB)

	if source != "config-default" {
		t.Errorf("source = %q, want config-default", source)
	}
	if baseURL != "https://config-url/v1" {
		t.Errorf("baseURL = %q, want config URL", baseURL)
	}
	if len(models) != 1 || models[0] != "config-only" {
		t.Errorf("models = %v, want [config-only]", models)
	}
}

// TestResolveFreeFallback_StateModelsButNoURL_BorrowsConfigURL handles the
// edge case where an operator added models to state file but forgot the
// base_url. Don't fail closed — borrow the config URL.
func TestResolveFreeFallback_StateModelsButNoURL_BorrowsConfigURL(t *testing.T) {
	cfgFB := config.FreeFallback{
		BaseURL: "https://config-url/v1",
		Models:  []string{"config-model"},
	}
	stateFB := keypool.FreeFallback{
		// BaseURL omitted
		Models: []string{"state-model"},
	}

	baseURL, models, source := resolveFreeFallback(cfgFB, stateFB)

	if source != "state-file" {
		t.Errorf("source = %q, want state-file (operator intent is state-defined)", source)
	}
	if baseURL != "https://config-url/v1" {
		t.Errorf("baseURL = %q, want config URL borrowed", baseURL)
	}
	if len(models) != 1 || models[0] != "state-model" {
		t.Errorf("models = %v, want state-model", models)
	}
}
