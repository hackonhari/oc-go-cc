package freepool

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func TestIsFreeTierModel(t *testing.T) {
	tests := map[string]bool{
		"deepseek-v4-flash-free": true,
		"qwen3.6-plus-free":      true,
		"minimax-m2.5-free":      true,
		"big-pickle":             true,
		"deepseek-v4-pro":        false,
		"sonnet":                 false,
		"opus":                   false,
		"kimi-k2.6":              false,
		"":                       false,
		"random-string":          false,
	}
	for input, want := range tests {
		got := isFreeTierModel(input)
		if got != want {
			t.Errorf("isFreeTierModel(%q) = %v, want %v", input, got, want)
		}
	}
}

func TestModelChain_UserFreeModelPrependedAndDeduped(t *testing.T) {
	r := New("https://example.com/v1",
		[]string{"deepseek-v4-flash-free", "qwen3.6-plus-free", "minimax-m2.5-free"},
		nil)

	// User requests qwen3.6-plus-free → should be first, deduped from configured list
	body := []byte(`{"model":"qwen3.6-plus-free","messages":[]}`)
	got := r.modelChain(body)
	want := []string{"qwen3.6-plus-free", "deepseek-v4-flash-free", "minimax-m2.5-free"}
	if !equalStringSlice(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestModelChain_UserBigPickleHonored(t *testing.T) {
	r := New("https://example.com/v1",
		[]string{"deepseek-v4-flash-free"},
		nil)

	body := []byte(`{"model":"big-pickle","messages":[]}`)
	got := r.modelChain(body)
	want := []string{"big-pickle", "deepseek-v4-flash-free"}
	if !equalStringSlice(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestModelChain_UserPaidModelIgnored(t *testing.T) {
	r := New("https://example.com/v1",
		[]string{"deepseek-v4-flash-free", "qwen3.6-plus-free"},
		nil)

	// User passes deepseek-v4-pro (paid) — should NOT be tried; chain unchanged.
	body := []byte(`{"model":"deepseek-v4-pro","messages":[]}`)
	got := r.modelChain(body)
	want := []string{"deepseek-v4-flash-free", "qwen3.6-plus-free"}
	if !equalStringSlice(got, want) {
		t.Errorf("paid model should be ignored; got %v, want %v", got, want)
	}
}

func TestModelChain_NoModelField_UsesConfiguredChain(t *testing.T) {
	r := New("https://example.com/v1",
		[]string{"deepseek-v4-flash-free", "qwen3.6-plus-free"},
		nil)

	body := []byte(`{"messages":[]}`) // no model field
	got := r.modelChain(body)
	want := []string{"deepseek-v4-flash-free", "qwen3.6-plus-free"}
	if !equalStringSlice(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestModelChain_UserSpecifiedNotInChain_StillTried(t *testing.T) {
	r := New("https://example.com/v1",
		[]string{"deepseek-v4-flash-free"},
		nil)

	// User picks a free-tier model NOT in configured chain — still honored.
	body := []byte(`{"model":"trinity-large-preview-free","messages":[]}`)
	got := r.modelChain(body)
	want := []string{"trinity-large-preview-free", "deepseek-v4-flash-free"}
	if !equalStringSlice(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

// End-to-end: user-specified free model actually wins the stub.
func TestResolveOpenAI_UserModelOverride_E2E(t *testing.T) {
	var attemptedModels []string
	var mu sync.Mutex
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req map[string]any
		_ = json.Unmarshal(body, &req)
		mu.Lock()
		attemptedModels = append(attemptedModels, req["model"].(string))
		mu.Unlock()
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"id":"x"}`))
	}))
	defer stub.Close()

	r := New(stub.URL, []string{"deepseek-v4-flash-free", "qwen3.6-plus-free"}, nil)

	body := []byte(`{"model":"qwen3.6-plus-free","messages":[{"role":"user","content":"hi"}]}`)
	resp, err := r.ResolveOpenAI(context.Background(), body, false, "forced")
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	defer resp.Body.Close()

	mu.Lock()
	defer mu.Unlock()
	if len(attemptedModels) != 1 {
		t.Errorf("expected exactly 1 upstream attempt (first model succeeds), got %d: %v", len(attemptedModels), attemptedModels)
	}
	if attemptedModels[0] != "qwen3.6-plus-free" {
		t.Errorf("user-specified model should have been tried first, got %q", attemptedModels[0])
	}
}

func equalStringSlice(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// silence unused-import in case future test additions drop a dep.
var _ = strings.HasSuffix
