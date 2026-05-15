package freepool

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"oc-go-cc/internal/keypool"
)

// captureEmitter records all emitted events for assertions.
type captureEmitter struct {
	mu     sync.Mutex
	events []keypool.Event
}

func (c *captureEmitter) Emit(e keypool.Event) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, e)
}

func (c *captureEmitter) eventTypes() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, len(c.events))
	for i, e := range c.events {
		out[i] = e.Type
	}
	return out
}

func TestResolveOpenAI_FirstModelSucceeds(t *testing.T) {
	var seenModels []string
	var mu sync.Mutex
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req map[string]any
		_ = json.Unmarshal(body, &req)
		mu.Lock()
		seenModels = append(seenModels, req["model"].(string))
		mu.Unlock()
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"id":"x","cost":"0"}`))
	}))
	defer stub.Close()

	emit := &captureEmitter{}
	r := New(stub.URL, []string{"deepseek-v4-flash-free", "qwen3.6-plus-free"}, emit)

	origBody := []byte(`{"model":"deepseek-v4-pro","messages":[{"role":"user","content":"hi"}]}`)
	resp, err := r.ResolveOpenAI(context.Background(), origBody, false, "fallback")
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("status = %d", resp.StatusCode)
	}

	// Only first model attempted.
	if len(seenModels) != 1 || seenModels[0] != "deepseek-v4-flash-free" {
		t.Errorf("expected one attempt with deepseek-v4-flash-free, got %v", seenModels)
	}

	// Engagement event emitted.
	types := emit.eventTypes()
	foundEngage := false
	for _, et := range types {
		if et == keypool.EventFreeFallbackEngaged {
			foundEngage = true
		}
	}
	if !foundEngage {
		t.Errorf("expected free_fallback_engaged event, got %v", types)
	}
}

func TestResolveOpenAI_FirstFails_SecondSucceeds(t *testing.T) {
	var seenModels []string
	var mu sync.Mutex
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req map[string]any
		_ = json.Unmarshal(body, &req)
		model := req["model"].(string)
		mu.Lock()
		seenModels = append(seenModels, model)
		mu.Unlock()
		if model == "deepseek-v4-flash-free" {
			w.WriteHeader(503)
			return
		}
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"id":"x"}`))
	}))
	defer stub.Close()

	emit := &captureEmitter{}
	r := New(stub.URL, []string{"deepseek-v4-flash-free", "qwen3.6-plus-free"}, emit)

	origBody := []byte(`{"model":"deepseek-v4-pro","messages":[]}`)
	resp, err := r.ResolveOpenAI(context.Background(), origBody, false, "fallback")
	if err != nil {
		t.Fatalf("should have fallen to second model: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("status = %d", resp.StatusCode)
	}

	if len(seenModels) != 2 {
		t.Errorf("expected 2 attempts, got %d: %v", len(seenModels), seenModels)
	}

	// Failure event emitted for first model.
	failureCount := 0
	for _, e := range emit.events {
		if e.Type == keypool.EventFreeFallbackModelFail {
			failureCount++
			if e.Model != "deepseek-v4-flash-free" {
				t.Errorf("failure event for wrong model: %s", e.Model)
			}
			if !strings.Contains(e.Reason, "503") {
				t.Errorf("failure reason should mention 503: %q", e.Reason)
			}
		}
	}
	if failureCount != 1 {
		t.Errorf("expected 1 failure event, got %d", failureCount)
	}
}

func TestResolveOpenAI_AllModelsFail(t *testing.T) {
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(503)
	}))
	defer stub.Close()

	r := New(stub.URL, []string{"a-free", "b-free", "c-free"}, nil)

	origBody := []byte(`{"model":"x","messages":[]}`)
	_, err := r.ResolveOpenAI(context.Background(), origBody, false, "fallback")
	if !errors.Is(err, ErrAllFreeFailed) {
		t.Fatalf("want ErrAllFreeFailed, got %v", err)
	}
}

func TestResolveOpenAI_NoAuthorizationHeaderSent(t *testing.T) {
	var sawAuth atomic.Bool
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			sawAuth.Store(true)
		}
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"id":"x"}`))
	}))
	defer stub.Close()

	r := New(stub.URL, []string{"deepseek-v4-flash-free"}, nil)

	origBody := []byte(`{"model":"x","messages":[]}`)
	resp, _ := r.ResolveOpenAI(context.Background(), origBody, false, "forced")
	if resp != nil {
		_ = resp.Body.Close()
	}

	if sawAuth.Load() {
		t.Errorf("free resolver MUST NOT send Authorization header")
	}
}

func TestResolveOpenAI_ModelFieldRewritten(t *testing.T) {
	var bodySeen string
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		bodySeen = string(body)
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"id":"x"}`))
	}))
	defer stub.Close()

	r := New(stub.URL, []string{"deepseek-v4-flash-free"}, nil)

	origBody := []byte(`{"model":"deepseek-v4-pro","messages":[{"role":"user","content":"hi"}],"max_tokens":5}`)
	resp, _ := r.ResolveOpenAI(context.Background(), origBody, false, "forced")
	if resp != nil {
		_ = resp.Body.Close()
	}

	if !strings.Contains(bodySeen, `"model":"deepseek-v4-flash-free"`) {
		t.Errorf("body model not rewritten: %s", bodySeen)
	}
	// Other fields preserved.
	if !strings.Contains(bodySeen, `"max_tokens":5`) {
		t.Errorf("max_tokens field lost: %s", bodySeen)
	}
	if !strings.Contains(bodySeen, `"messages"`) {
		t.Errorf("messages field lost: %s", bodySeen)
	}
}

func TestResolveOpenAI_MiniMaxFreeUsesMessagesEndpoint(t *testing.T) {
	var pathSeen string
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pathSeen = r.URL.Path
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"id":"x"}`))
	}))
	defer stub.Close()

	r := New(stub.URL, []string{"minimax-m2.5-free"}, nil)

	origBody := []byte(`{"model":"x","messages":[]}`)
	resp, _ := r.ResolveOpenAI(context.Background(), origBody, false, "forced")
	if resp != nil {
		_ = resp.Body.Close()
	}

	if pathSeen != "/messages" {
		t.Errorf("minimax-* should hit /messages, got %s", pathSeen)
	}
}

func TestResolveOpenAI_DeepSeekFlashUsesChatCompletionsEndpoint(t *testing.T) {
	var pathSeen string
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pathSeen = r.URL.Path
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"id":"x"}`))
	}))
	defer stub.Close()

	r := New(stub.URL, []string{"deepseek-v4-flash-free"}, nil)

	origBody := []byte(`{"model":"x","messages":[]}`)
	resp, _ := r.ResolveOpenAI(context.Background(), origBody, false, "forced")
	if resp != nil {
		_ = resp.Body.Close()
	}

	if pathSeen != "/chat/completions" {
		t.Errorf("non-minimax should hit /chat/completions, got %s", pathSeen)
	}
}

func TestResolveAnthropic_RawBodyForwarded(t *testing.T) {
	var pathSeen string
	var bodySeen string
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pathSeen = r.URL.Path
		body, _ := io.ReadAll(r.Body)
		bodySeen = string(body)
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"type":"message"}`))
	}))
	defer stub.Close()

	r := New(stub.URL, []string{"minimax-m2.5-free"}, nil)

	origBody := []byte(`{"model":"minimax-m2.5","messages":[{"role":"user","content":"hi"}]}`)
	resp, err := r.ResolveAnthropic(context.Background(), origBody, false, "fallback")
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	defer resp.Body.Close()

	if pathSeen != "/messages" {
		t.Errorf("ResolveAnthropic must hit /messages, got %s", pathSeen)
	}
	if !strings.Contains(bodySeen, `"model":"minimax-m2.5-free"`) {
		t.Errorf("model not rewritten in anthropic body: %s", bodySeen)
	}
}
