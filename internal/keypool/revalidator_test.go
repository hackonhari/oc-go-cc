package keypool

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// revalidatorTestPool builds a pool with one exhausted key sk-a and one
// active key sk-b. Returns the pool and the path of its state file.
func revalidatorTestPool(t *testing.T) *KeyPool {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state.json")
	pool := New(path, nil)
	exhausted := time.Now().Add(-1 * time.Hour)
	_ = pool.SeedKeys([]*Key{
		{Token: "sk-a", Account: "primary", ExhaustedAt: &exhausted, ResetDate: time.Now().Add(30 * 24 * time.Hour)},
		{Token: "sk-b", Account: "secondary"},
	}, FreeFallback{})
	return pool
}

// runRevalidatorOnce runs probeOnce synchronously against the test pool —
// avoids the goroutine/ticker complexity in unit tests.
func runRevalidatorOnce(t *testing.T, pool *KeyPool, stub *httptest.Server) {
	t.Helper()
	r := NewRevalidatorWithInterval(pool, stub.URL, 1*time.Hour, nil, nil)
	r.probeOnce(context.Background())
}

func TestRevalidator_HTTP200_RevivesKey(t *testing.T) {
	pool := revalidatorTestPool(t)

	var probedTokens []string
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		probedTokens = append(probedTokens, r.Header.Get("Authorization"))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"x","choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer stub.Close()

	runRevalidatorOnce(t, pool, stub)

	snap := pool.Snapshot()
	if snap[0].ExhaustedAt != nil {
		t.Errorf("sk-a should be revived (ExhaustedAt=nil), got %v", snap[0].ExhaustedAt)
	}
	if len(probedTokens) != 1 {
		t.Errorf("expected 1 probe (only sk-a is exhausted), got %d", len(probedTokens))
	}
	if probedTokens[0] != "Bearer sk-a" {
		t.Errorf("expected Bearer sk-a, got %q", probedTokens[0])
	}
}

func TestRevalidator_HTTP401_LeavesExhausted(t *testing.T) {
	pool := revalidatorTestPool(t)

	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"type":"CreditsError"}}`))
	}))
	defer stub.Close()

	runRevalidatorOnce(t, pool, stub)

	snap := pool.Snapshot()
	if snap[0].ExhaustedAt == nil {
		t.Errorf("sk-a should remain exhausted after 401 probe, got ExhaustedAt=nil")
	}
}

func TestRevalidator_NetworkError_DoesNotPanic(t *testing.T) {
	pool := revalidatorTestPool(t)

	// Use a stub that we close immediately to force connection errors.
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	stub.Close()

	runRevalidatorOnce(t, pool, stub) // must not panic

	snap := pool.Snapshot()
	if snap[0].ExhaustedAt == nil {
		t.Errorf("sk-a should remain exhausted after network error")
	}
}

func TestRevalidator_SkipsActiveKeys(t *testing.T) {
	pool := revalidatorTestPool(t)

	var probed int32
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&probed, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer stub.Close()

	runRevalidatorOnce(t, pool, stub)

	// sk-a is exhausted (probed). sk-b is active (NOT probed).
	if got := atomic.LoadInt32(&probed); got != 1 {
		t.Errorf("expected 1 probe (only exhausted key), got %d", got)
	}
}

func TestRevalidator_RunStopsOnContextCancel(t *testing.T) {
	pool := revalidatorTestPool(t)

	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized) // keep exhausted so loop has work
	}))
	defer stub.Close()

	r := NewRevalidatorWithInterval(pool, stub.URL, 50*time.Millisecond, nil, nil)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		r.Run(ctx)
		close(done)
	}()

	// Let the loop tick at least once, then cancel.
	time.Sleep(150 * time.Millisecond)
	cancel()

	select {
	case <-done:
		// expected — clean exit
	case <-time.After(2 * time.Second):
		t.Fatal("revalidator did not exit within 2s of context cancel")
	}
}

// (captureEmitter lives in pool_test.go — shared across keypool tests.)

// TestRevalidator_EmitsStartAndTick guards AC-C: operators must be able to
// confirm revalidator health from the rotation log alone. Two events
// must appear: revalidator_started (on Run entry) and revalidator_tick
// (on every probe sweep including the immediate tick).
func TestRevalidator_EmitsStartAndTick(t *testing.T) {
	pool := revalidatorTestPool(t)
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized) // keep exhausted so tick has work
	}))
	defer stub.Close()

	emitter := &captureEmitter{}
	r := NewRevalidatorWithInterval(pool, stub.URL, 1*time.Hour, nil, emitter)

	ctx, cancel := context.WithCancel(context.Background())
	go r.Run(ctx)

	// Wait briefly for Run to enter and complete its immediate tick.
	time.Sleep(200 * time.Millisecond)
	cancel()
	time.Sleep(100 * time.Millisecond)

	if !emitter.Has(EventRevalidatorStarted) {
		t.Errorf("expected %q event, none seen. Events: %+v",
			EventRevalidatorStarted, emitter.events)
	}
	if !emitter.Has(EventRevalidatorTick) {
		t.Errorf("expected %q event, none seen. Events: %+v",
			EventRevalidatorTick, emitter.events)
	}
}

// TestRevalidator_HTTP200_EmitsKeyRevived guards AC-C: a successful probe
// must surface in the rotation log via key_revived event. The event is
// emitted by pool.ClearExhausted (not the revalidator itself), so we wire
// the emitter into the pool, not the revalidator, for this assertion.
func TestRevalidator_HTTP200_EmitsKeyRevived(t *testing.T) {
	pool := revalidatorTestPool(t)
	emitter := &captureEmitter{}
	pool.SetEmitter(emitter)

	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer stub.Close()

	r := NewRevalidatorWithInterval(pool, stub.URL, 1*time.Hour, nil, nil)
	r.probeOnce(context.Background())

	if !emitter.Has(EventKeyRevived) {
		t.Errorf("expected %q event after HTTP 200 probe, none seen", EventKeyRevived)
	}
}

func TestRevalidator_RunRunsImmediateTick(t *testing.T) {
	pool := revalidatorTestPool(t)

	probed := make(chan struct{}, 8)
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case probed <- struct{}{}:
		default:
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer stub.Close()

	// Use a long interval — only the immediate tick should fire.
	r := NewRevalidatorWithInterval(pool, stub.URL, 1*time.Hour, nil, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.Run(ctx)

	select {
	case <-probed:
		// success — immediate tick fired without waiting 1 hour
	case <-time.After(2 * time.Second):
		t.Fatal("revalidator did not fire immediate tick within 2s")
	}
}
