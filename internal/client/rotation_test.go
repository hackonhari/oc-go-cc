package client

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"oc-go-cc/internal/config"
	"oc-go-cc/internal/freepool"
	"oc-go-cc/internal/keypool"
	"oc-go-cc/pkg/types"
)

// newTestPool constructs a pool seeded with the given tokens, persisted
// to a temp file unique to this test.
func newTestPool(t *testing.T, tokens ...string) *keypool.KeyPool {
	t.Helper()
	path := filepath.Join(t.TempDir(), "key-states.json")
	pool := keypool.New(path, nil)
	keys := make([]*keypool.Key, 0, len(tokens))
	for i, tok := range tokens {
		keys = append(keys, &keypool.Key{
			Token:     tok,
			Account:   "test-account-" + string(rune('a'+i)),
			ResetDate: time.Now().Add(30 * 24 * time.Hour),
		})
	}
	if err := pool.SeedKeys(keys, keypool.FreeFallback{}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	return pool
}

// trivialReq returns a minimal valid ChatCompletionRequest body for tests.
func trivialReq() *types.ChatCompletionRequest {
	return &types.ChatCompletionRequest{
		Model: "deepseek-v4-flash",
		Messages: []types.ChatMessage{
			{Role: "user", Content: "hi"},
		},
	}
}

func TestChatCompletion_HappyPath(t *testing.T) {
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer sk-good" {
			t.Errorf("wrong auth: %q", got)
		}
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"id":"x","model":"y","choices":[]}`))
	}))
	defer stub.Close()

	pool := newTestPool(t, "sk-good")
	c := NewOpenCodeClient(
		config.OpenCodeGoConfig{BaseURL: stub.URL, AnthropicBaseURL: stub.URL},
		pool, keypool.NewClassifier(), nil,
	)

	resp, err := c.ChatCompletion(context.Background(), "deepseek-v4-flash", trivialReq())
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("status = %d", resp.StatusCode)
	}
}

func TestChatCompletion_401CreditsError_RotatesToNextKey(t *testing.T) {
	var calls sync.Map
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		calls.Store(auth, true)
		if strings.HasSuffix(auth, "sk-exhausted") {
			w.WriteHeader(401)
			_, _ = w.Write([]byte(`{"type":"error","error":{"type":"CreditsError","message":"Insufficient balance"}}`))
			return
		}
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"id":"x","model":"y","choices":[]}`))
	}))
	defer stub.Close()

	pool := newTestPool(t, "sk-exhausted", "sk-good")
	c := NewOpenCodeClient(
		config.OpenCodeGoConfig{BaseURL: stub.URL, AnthropicBaseURL: stub.URL},
		pool, keypool.NewClassifier(), nil,
	)

	resp, err := c.ChatCompletion(context.Background(), "deepseek-v4-flash", trivialReq())
	if err != nil {
		t.Fatalf("rotation should succeed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("expected eventual 200 from sk-good, got %d", resp.StatusCode)
	}

	// Both tokens should have been seen.
	_, sawExhausted := calls.Load("Bearer sk-exhausted")
	_, sawGood := calls.Load("Bearer sk-good")
	if !sawExhausted || !sawGood {
		t.Errorf("expected both tokens tried; exhausted=%v good=%v", sawExhausted, sawGood)
	}

	// State assertions.
	snap := pool.Snapshot()
	if snap[0].ExhaustedAt == nil {
		t.Errorf("sk-exhausted should be marked exhausted")
	}
	if snap[1].ExhaustedAt != nil {
		t.Errorf("sk-good should remain active")
	}
}

func TestChatCompletion_AllExhausted_ReturnsErrAllExhausted(t *testing.T) {
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		_, _ = w.Write([]byte(`{"error":{"type":"CreditsError"}}`))
	}))
	defer stub.Close()

	pool := newTestPool(t, "sk-1", "sk-2")
	c := NewOpenCodeClient(
		config.OpenCodeGoConfig{BaseURL: stub.URL, AnthropicBaseURL: stub.URL},
		pool, keypool.NewClassifier(), nil,
	)

	_, err := c.ChatCompletion(context.Background(), "deepseek-v4-flash", trivialReq())
	if !errors.Is(err, ErrAllKeysExhausted) {
		t.Fatalf("want ErrAllKeysExhausted, got %v", err)
	}
	if !IsAllExhausted(err) {
		t.Errorf("IsAllExhausted should return true")
	}

	for i, k := range pool.Snapshot() {
		if k.ExhaustedAt == nil {
			t.Errorf("key %d (%s) should be exhausted", i, k.Account)
		}
	}
}

func TestChatCompletion_429Transient_RetriesSameKeyWithBackoff(t *testing.T) {
	var hits int32
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count := atomic.AddInt32(&hits, 1)
		if count == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(429)
			_, _ = w.Write([]byte(`{"error":"slow down"}`)) // benign body, no quota keyword
			return
		}
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"id":"x","model":"y","choices":[]}`))
	}))
	defer stub.Close()

	pool := newTestPool(t, "sk-only")
	c := NewOpenCodeClient(
		config.OpenCodeGoConfig{BaseURL: stub.URL, AnthropicBaseURL: stub.URL},
		pool, keypool.NewClassifier(), nil,
	)

	start := time.Now()
	resp, err := c.ChatCompletion(context.Background(), "deepseek-v4-flash", trivialReq())
	if err != nil {
		t.Fatalf("transient should retry and succeed: %v", err)
	}
	defer resp.Body.Close()

	if elapsed := time.Since(start); elapsed < 900*time.Millisecond {
		t.Errorf("Retry-After backoff not honored; elapsed=%v", elapsed)
	}
	if atomic.LoadInt32(&hits) != 2 {
		t.Errorf("expected 2 stub hits, got %d", hits)
	}
	if pool.Snapshot()[0].ExhaustedAt != nil {
		t.Errorf("transient should NOT mark exhausted")
	}
}

// TestChatCompletion_TransientRetriesExhausted_UsesShortTTL guards against
// regression of cycle 1's escalation-with-zero-ResetDate bug. After
// maxTransientRetries+1 ambiguous 429s on the same key, the proxy escalates
// to MarkExhausted. The TTL must be the short transientEscalationTTL
// constant (15 min as of 2026-05-18), NOT zero (which permanently kills the
// key) and NOT 30 days (which is the cap for real CreditsError exhaustion).
func TestChatCompletion_TransientRetriesExhausted_UsesShortTTL(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test exercises real Transient retry backoff; ~240s")
	}
	// Always return ambiguous 429 — no Retry-After header, no quota keyword.
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(429)
		_, _ = w.Write([]byte(`{"error":"please slow"}`))
	}))
	defer stub.Close()

	// Two-key pool. The escalation should rotate from sk-bursty to sk-spare.
	pool := newTestPool(t, "sk-bursty", "sk-spare")
	c := NewOpenCodeClient(
		config.OpenCodeGoConfig{BaseURL: stub.URL, AnthropicBaseURL: stub.URL},
		pool, keypool.NewClassifier(), nil,
	)

	before := time.Now()
	_, _ = c.ChatCompletion(context.Background(), "deepseek-v4-flash", trivialReq())
	after := time.Now()

	snap := pool.Snapshot()
	// sk-bursty should be exhausted with SHORT TTL.
	if snap[0].ExhaustedAt == nil {
		t.Fatal("sk-bursty should be exhausted after transient retries exhausted")
	}
	if snap[0].ResetDate.IsZero() {
		t.Errorf("sk-bursty ResetDate must NOT be zero (the cycle 1 bug); got %v", snap[0].ResetDate)
	}
	// Allow generous bounds — 15 min ± 5 min margin for clock skew + test slowness.
	minExpected := before.Add(10 * time.Minute)
	maxExpected := after.Add(20 * time.Minute)
	if snap[0].ResetDate.Before(minExpected) || snap[0].ResetDate.After(maxExpected) {
		t.Errorf("ResetDate = %v, want between %v and %v (15-min TTL)", snap[0].ResetDate, minExpected, maxExpected)
	}
}

func TestChatCompletion_5xx_NotMarkedExhausted(t *testing.T) {
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(503)
		_, _ = w.Write([]byte("upstream unavailable"))
	}))
	defer stub.Close()

	pool := newTestPool(t, "sk-only")
	c := NewOpenCodeClient(
		config.OpenCodeGoConfig{BaseURL: stub.URL, AnthropicBaseURL: stub.URL},
		pool, keypool.NewClassifier(), nil,
	)

	resp, _ := c.ChatCompletion(context.Background(), "deepseek-v4-flash", trivialReq())
	if resp != nil {
		_ = resp.Body.Close()
	}
	if pool.Snapshot()[0].ExhaustedAt != nil {
		t.Errorf("5xx must NOT mark key exhausted")
	}
}

func TestSendAnthropicRequest_AlsoRotates(t *testing.T) {
	var calls int32
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		if r.Header.Get("Authorization") == "Bearer sk-exhausted" {
			w.WriteHeader(401)
			_, _ = w.Write([]byte(`{"error":{"type":"CreditsError"}}`))
			return
		}
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"type":"message"}`))
	}))
	defer stub.Close()

	pool := newTestPool(t, "sk-exhausted", "sk-good")
	c := NewOpenCodeClient(
		config.OpenCodeGoConfig{BaseURL: stub.URL, AnthropicBaseURL: stub.URL},
		pool, keypool.NewClassifier(), nil,
	)

	resp, err := c.SendAnthropicRequest(context.Background(), []byte(`{"model":"minimax-m2.5"}`), false)
	if err != nil {
		t.Fatalf("rotation via Anthropic path should succeed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("status = %d", resp.StatusCode)
	}
	if atomic.LoadInt32(&calls) != 2 {
		t.Errorf("expected 2 upstream calls (exhausted + good), got %d", calls)
	}
}

// TestChatCompletion_PaidExhausted_FreeFallbackEngages is the end-to-end
// proof that Hari asked for: when all paid keys return real-shape 401
// (CreditsError), the client transparently falls through to the free
// resolver, and a separate "free" upstream succeeds. No special handling
// in the test — just the configured fallback chain doing its job.
func TestChatCompletion_PaidExhausted_FreeFallbackEngages(t *testing.T) {
	// Paid upstream: always returns real-shape CreditsError 401.
	paidStub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"CreditsError","message":"Insufficient balance"}}`))
	}))
	defer paidStub.Close()

	// Free upstream: anonymous (no auth required), returns 200 with cost=0.
	var freeAuthSeen string
	var freeBodySeen string
	freeStub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		freeAuthSeen = r.Header.Get("Authorization")
		body, _ := io.ReadAll(r.Body)
		freeBodySeen = string(body)
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"id":"free-resp","cost":"0"}`))
	}))
	defer freeStub.Close()

	pool := newTestPool(t, "sk-paid-1", "sk-paid-2")
	resolver := freepool.New(freeStub.URL, []string{"deepseek-v4-flash-free"}, nil)
	c := NewOpenCodeClient(
		config.OpenCodeGoConfig{BaseURL: paidStub.URL, AnthropicBaseURL: paidStub.URL},
		pool, keypool.NewClassifier(), resolver,
	)

	resp, err := c.ChatCompletion(context.Background(), "deepseek-v4-flash", trivialReq())
	if err != nil {
		t.Fatalf("fallback should produce 200, got: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("status = %d", resp.StatusCode)
	}

	// Free endpoint MUST be called anonymously.
	if freeAuthSeen != "" {
		t.Errorf("free request leaked Authorization header: %q", freeAuthSeen)
	}

	// Body rewritten to free model.
	if !strings.Contains(freeBodySeen, `"model":"deepseek-v4-flash-free"`) {
		t.Errorf("model not rewritten in free body: %s", freeBodySeen)
	}

	// Both paid keys marked exhausted.
	for i, k := range pool.Snapshot() {
		if k.ExhaustedAt == nil {
			t.Errorf("paid key %d (%s) should be exhausted post-fallback", i, k.Account)
		}
	}
}

// TestWithFreeOnly_ContextBypassesPaidPool verifies the dual-endpoint
// routing mechanism: a context tagged WithFreeOnly causes ChatCompletion
// to skip paid pool entirely.
func TestWithFreeOnly_ContextBypassesPaidPool(t *testing.T) {
	paidHits := 0
	paidStub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paidHits++
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"id":"PAID-LEAK","model":"y"}`))
	}))
	defer paidStub.Close()

	freeStub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"id":"FREE-RESP","cost":"0"}`))
	}))
	defer freeStub.Close()

	pool := newTestPool(t, "sk-paid")
	resolver := freepool.New(freeStub.URL, []string{"deepseek-v4-flash-free"}, nil)
	c := NewOpenCodeClient(
		config.OpenCodeGoConfig{BaseURL: paidStub.URL, AnthropicBaseURL: paidStub.URL},
		pool, keypool.NewClassifier(), resolver,
	)

	// Use context-carried free-only flag (mimics what server.go does for /free/v1/*).
	ctx := WithFreeOnly(context.Background())
	resp, err := c.ChatCompletion(ctx, "deepseek-v4-flash", trivialReq())
	if err != nil {
		t.Fatalf("ChatCompletion with WithFreeOnly: %v", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "FREE-RESP") {
		t.Errorf("response should come from free upstream, got: %s", body)
	}
	if paidHits != 0 {
		t.Errorf("WithFreeOnly context MUST NOT hit paid upstream, got %d hits", paidHits)
	}
	if pool.Snapshot()[0].ExhaustedAt != nil {
		t.Errorf("paid key MUST NOT be marked exhausted in free-only mode")
	}
}

// TestFreeOnlyChatCompletion_BypassesPaidPool verifies the /free path
// (Phase 6 routing): paid keys are never touched.
func TestFreeOnlyChatCompletion_BypassesPaidPool(t *testing.T) {
	paidHits := 0
	paidStub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paidHits++
		w.WriteHeader(500)
	}))
	defer paidStub.Close()

	freeStub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"id":"x","cost":"0"}`))
	}))
	defer freeStub.Close()

	pool := newTestPool(t, "sk-paid")
	resolver := freepool.New(freeStub.URL, []string{"deepseek-v4-flash-free"}, nil)
	c := NewOpenCodeClient(
		config.OpenCodeGoConfig{BaseURL: paidStub.URL, AnthropicBaseURL: paidStub.URL},
		pool, keypool.NewClassifier(), resolver,
	)

	resp, err := c.FreeOnlyChatCompletion(context.Background(), trivialReq())
	if err != nil {
		t.Fatalf("FreeOnlyChatCompletion: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("status = %d", resp.StatusCode)
	}
	if paidHits != 0 {
		t.Errorf("FreeOnly path MUST NOT hit paid upstream, got %d hits", paidHits)
	}
	if pool.Snapshot()[0].ExhaustedAt != nil {
		t.Errorf("FreeOnly path MUST NOT mark paid key exhausted")
	}
}

func TestChatCompletion_OtherClient4xx_PropagatesWithoutMarking(t *testing.T) {
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(400)
		_, _ = w.Write([]byte(`{"error":"bad request"}`))
	}))
	defer stub.Close()

	pool := newTestPool(t, "sk-only")
	c := NewOpenCodeClient(
		config.OpenCodeGoConfig{BaseURL: stub.URL, AnthropicBaseURL: stub.URL},
		pool, keypool.NewClassifier(), nil,
	)

	resp, _ := c.ChatCompletion(context.Background(), "deepseek-v4-flash", trivialReq())
	if resp != nil {
		_ = resp.Body.Close()
	}
	if pool.Snapshot()[0].ExhaustedAt != nil {
		t.Errorf("400 must NOT mark key exhausted (likely bad request shape, not key issue)")
	}
}
