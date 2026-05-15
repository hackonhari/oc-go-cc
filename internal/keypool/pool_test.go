package keypool

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// stateFilePath returns a fresh state file path inside the test's temp dir.
func stateFilePath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "key-states.json")
}

// mkKey is a small constructor for test fixtures.
func mkKey(token, account string, exhausted *time.Time, reset time.Time) *Key {
	return &Key{
		Token:       token,
		Account:     account,
		ExhaustedAt: exhausted,
		ResetDate:   reset,
	}
}

func TestAcquire_SingleActiveKey(t *testing.T) {
	pool := New(stateFilePath(t), nil)
	if err := pool.SeedKeys([]*Key{
		mkKey("sk-a", "primary", nil, time.Now().Add(30*24*time.Hour)),
	}, FreeFallback{}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	k, err := pool.Acquire()
	if err != nil {
		t.Fatalf("Acquire(): %v", err)
	}
	if k.Token != "sk-a" {
		t.Errorf("got token %q, want sk-a", k.Token)
	}
	if k.LastUsed == nil {
		t.Errorf("LastUsed not set")
	}
}

func TestAcquire_StickyReuse(t *testing.T) {
	pool := New(stateFilePath(t), nil)
	_ = pool.SeedKeys([]*Key{
		mkKey("sk-a", "primary", nil, time.Now().Add(30*24*time.Hour)),
		mkKey("sk-b", "secondary", nil, time.Now().Add(30*24*time.Hour)),
	}, FreeFallback{})

	for i := 0; i < 10; i++ {
		k, err := pool.Acquire()
		if err != nil {
			t.Fatalf("Acquire(): %v", err)
		}
		if k.Token != "sk-a" {
			t.Errorf("iteration %d: got %q, want sk-a (sticky)", i, k.Token)
		}
	}
}

func TestAcquire_MultiKeyPrimaryHealthy(t *testing.T) {
	pool := New(stateFilePath(t), nil)
	_ = pool.SeedKeys([]*Key{
		mkKey("sk-a", "primary", nil, time.Now().Add(30*24*time.Hour)),
		mkKey("sk-b", "secondary", nil, time.Now().Add(30*24*time.Hour)),
	}, FreeFallback{})

	k, _ := pool.Acquire()
	if k.Token != "sk-a" {
		t.Errorf("got %q, want sk-a (first active)", k.Token)
	}

	// Snapshot should show only primary's LastUsed advanced.
	snap := pool.Snapshot()
	if snap[0].LastUsed == nil {
		t.Errorf("primary LastUsed should be set")
	}
	if snap[1].LastUsed != nil {
		t.Errorf("secondary LastUsed should be nil (untouched)")
	}
}

func TestMarkExhausted_RotatesToNext(t *testing.T) {
	pool := New(stateFilePath(t), nil)
	_ = pool.SeedKeys([]*Key{
		mkKey("sk-a", "primary", nil, time.Now().Add(30*24*time.Hour)),
		mkKey("sk-b", "secondary", nil, time.Now().Add(30*24*time.Hour)),
	}, FreeFallback{})

	// Acquire primary, then mark it exhausted.
	k, _ := pool.Acquire()
	if err := pool.MarkExhausted(k.Token, time.Now().Add(7*24*time.Hour), "test"); err != nil {
		t.Fatalf("MarkExhausted: %v", err)
	}

	// Next Acquire should return secondary.
	next, err := pool.Acquire()
	if err != nil {
		t.Fatalf("Acquire after mark: %v", err)
	}
	if next.Token != "sk-b" {
		t.Errorf("got %q, want sk-b", next.Token)
	}
}

func TestMarkExhausted_AllExhausted(t *testing.T) {
	pool := New(stateFilePath(t), nil)
	_ = pool.SeedKeys([]*Key{
		mkKey("sk-a", "primary", nil, time.Now().Add(30*24*time.Hour)),
		mkKey("sk-b", "secondary", nil, time.Now().Add(30*24*time.Hour)),
	}, FreeFallback{})

	_ = pool.MarkExhausted("sk-a", time.Now().Add(7*24*time.Hour), "test")
	_ = pool.MarkExhausted("sk-b", time.Now().Add(7*24*time.Hour), "test")

	_, err := pool.Acquire()
	if err != ErrAllExhausted {
		t.Errorf("want ErrAllExhausted, got %v", err)
	}
}

func TestMarkExhausted_KeyNotFound(t *testing.T) {
	pool := New(stateFilePath(t), nil)
	_ = pool.SeedKeys([]*Key{
		mkKey("sk-a", "primary", nil, time.Now().Add(30*24*time.Hour)),
	}, FreeFallback{})

	err := pool.MarkExhausted("sk-nonexistent", time.Now(), "test")
	if err != ErrKeyNotFound {
		t.Errorf("want ErrKeyNotFound, got %v", err)
	}
}

func TestAcquire_AutoResetOnDatePass(t *testing.T) {
	pool := New(stateFilePath(t), nil)

	// Key marked exhausted with resetDate yesterday — should auto-reset.
	yesterday := time.Now().Add(-24 * time.Hour)
	exhaustedAt := time.Now().Add(-7 * 24 * time.Hour)
	_ = pool.SeedKeys([]*Key{
		mkKey("sk-a", "primary", &exhaustedAt, yesterday),
	}, FreeFallback{})

	k, err := pool.Acquire()
	if err != nil {
		t.Fatalf("Acquire(): %v", err)
	}
	if k.Token != "sk-a" {
		t.Errorf("got %q, want sk-a", k.Token)
	}

	// Snapshot should show exhaustedAt cleared.
	snap := pool.Snapshot()
	if snap[0].ExhaustedAt != nil {
		t.Errorf("ExhaustedAt should be nil after auto-reset")
	}
}

func TestAcquire_AutoResetSameDay(t *testing.T) {
	pool := New(stateFilePath(t), nil)

	// Key marked exhausted with resetDate today (truncated to day, so eligible).
	today := time.Now()
	exhaustedAt := time.Now().Add(-7 * 24 * time.Hour)
	_ = pool.SeedKeys([]*Key{
		mkKey("sk-a", "primary", &exhaustedAt, today),
	}, FreeFallback{})

	k, err := pool.Acquire()
	if err != nil {
		t.Fatalf("Acquire(): %v", err)
	}
	if k.Token != "sk-a" {
		t.Errorf("want sk-a auto-reset on same day")
	}
}

func TestAcquire_FutureResetDateBlocks(t *testing.T) {
	pool := New(stateFilePath(t), nil)

	// Key marked exhausted with future resetDate — should NOT auto-reset.
	future := time.Now().Add(24 * time.Hour)
	exhaustedAt := time.Now().Add(-7 * 24 * time.Hour)
	_ = pool.SeedKeys([]*Key{
		mkKey("sk-a", "primary", &exhaustedAt, future),
	}, FreeFallback{})

	_, err := pool.Acquire()
	if err != ErrAllExhausted {
		t.Errorf("want ErrAllExhausted (future reset), got %v", err)
	}
}

func TestLoadFromDisk_MissingFileReturnsEmptyPool(t *testing.T) {
	path := stateFilePath(t)
	pool := New(path, nil)
	if err := pool.LoadFromDisk(); err != nil {
		t.Errorf("LoadFromDisk on missing file: %v", err)
	}
	if len(pool.Snapshot()) != 0 {
		t.Errorf("pool should be empty after missing-file load")
	}
}

func TestLoadFromDisk_CorruptedFileRecovers(t *testing.T) {
	path := stateFilePath(t)
	if err := os.WriteFile(path, []byte("{ not json"), 0o600); err != nil {
		t.Fatalf("write corrupt: %v", err)
	}

	pool := New(path, nil)
	if err := pool.LoadFromDisk(); err != nil {
		t.Errorf("LoadFromDisk on corrupted: %v", err)
	}

	// Pool is empty, and a .corrupt.* file exists.
	if len(pool.Snapshot()) != 0 {
		t.Errorf("pool should be empty after corrupt load")
	}

	matches, _ := filepath.Glob(path + ".corrupt.*")
	if len(matches) != 1 {
		t.Errorf("expected one .corrupt.* file, got %d", len(matches))
	}
}

func TestSeedKeys_PersistsToFile(t *testing.T) {
	path := stateFilePath(t)
	pool := New(path, nil)
	if err := pool.SeedKeys([]*Key{
		mkKey("sk-a", "primary", nil, time.Now().Add(30*24*time.Hour)),
	}, FreeFallback{
		BaseURL: "https://opencode.ai/zen/v1",
		Models:  []string{"deepseek-v4-flash-free"},
	}); err != nil {
		t.Fatalf("SeedKeys: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read state: %v", err)
	}
	var sf stateFile
	if err := json.Unmarshal(data, &sf); err != nil {
		t.Fatalf("parse persisted state: %v", err)
	}
	if len(sf.APIKeys) != 1 || sf.APIKeys[0].Token != "sk-a" {
		t.Errorf("persisted key mismatch: %+v", sf.APIKeys)
	}
	if sf.FreeFallback.BaseURL != "https://opencode.ai/zen/v1" {
		t.Errorf("free fallback not persisted")
	}
}

func TestRoundTrip_SeedSaveLoad(t *testing.T) {
	path := stateFilePath(t)
	now := time.Now()
	exhaustedAt := now.Add(-1 * time.Hour)
	reset := now.Add(7 * 24 * time.Hour)

	pool1 := New(path, nil)
	_ = pool1.SeedKeys([]*Key{
		mkKey("sk-a", "primary", &exhaustedAt, reset),
		mkKey("sk-b", "secondary", nil, reset),
	}, FreeFallback{Models: []string{"deepseek-v4-flash-free"}})

	pool2 := New(path, nil)
	if err := pool2.LoadFromDisk(); err != nil {
		t.Fatalf("LoadFromDisk: %v", err)
	}

	snap := pool2.Snapshot()
	if len(snap) != 2 {
		t.Fatalf("expected 2 keys, got %d", len(snap))
	}
	if snap[0].ExhaustedAt == nil {
		t.Errorf("exhaustedAt should round-trip")
	}
	if !snap[1].IsActive() {
		t.Errorf("active key should remain active after round-trip")
	}
}

func TestEarliestResetDate(t *testing.T) {
	pool := New(stateFilePath(t), nil)
	now := time.Now()
	exhausted := now.Add(-1 * time.Hour)
	_ = pool.SeedKeys([]*Key{
		mkKey("sk-a", "p1", &exhausted, now.Add(10*24*time.Hour)),
		mkKey("sk-b", "p2", &exhausted, now.Add(3*24*time.Hour)),
		mkKey("sk-c", "active", nil, now.Add(30*24*time.Hour)),
	}, FreeFallback{})

	earliest := pool.EarliestResetDate()
	want := now.Add(3 * 24 * time.Hour)
	// Allow some leeway because of nano resolution drift.
	if earliest.Before(want.Add(-time.Second)) || earliest.After(want.Add(time.Second)) {
		t.Errorf("earliest = %v, want ~%v", earliest, want)
	}
}

func TestEarliestResetDate_NoneExhausted(t *testing.T) {
	pool := New(stateFilePath(t), nil)
	_ = pool.SeedKeys([]*Key{
		mkKey("sk-a", "p1", nil, time.Now().Add(30*24*time.Hour)),
	}, FreeFallback{})

	if !pool.EarliestResetDate().IsZero() {
		t.Errorf("expected zero time when no keys exhausted")
	}
}

// TestConcurrent_RaceFreedom exercises the pool from multiple goroutines
// to verify -race detector finds no issues. Functional correctness is
// covered by the other tests; this one is purely about memory safety.
func TestConcurrent_RaceFreedom(t *testing.T) {
	pool := New(stateFilePath(t), nil)
	_ = pool.SeedKeys([]*Key{
		mkKey("sk-a", "p1", nil, time.Now().Add(30*24*time.Hour)),
		mkKey("sk-b", "p2", nil, time.Now().Add(30*24*time.Hour)),
		mkKey("sk-c", "p3", nil, time.Now().Add(30*24*time.Hour)),
	}, FreeFallback{})

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				_, _ = pool.Acquire()
				_ = pool.EarliestResetDate()
				_ = pool.Snapshot()
				_ = pool.FreeFallback()
			}
		}()
	}

	// Concurrent writer.
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 10; j++ {
				_ = pool.MarkExhausted("sk-a", time.Now().Add(7*24*time.Hour), "race")
			}
		}()
	}

	wg.Wait()
}

func TestMarkTransient_EmitsButDoesNotMarkExhausted(t *testing.T) {
	pool := New(stateFilePath(t), nil)
	emit := &captureEmitter{}
	pool.SetEmitter(emit)

	_ = pool.SeedKeys([]*Key{
		mkKey("sk-a", "primary", nil, time.Now().Add(30*24*time.Hour)),
	}, FreeFallback{})

	pool.MarkTransient("sk-a", 5*time.Second, "burst")

	// Key still active.
	snap := pool.Snapshot()
	if !snap[0].IsActive() {
		t.Errorf("transient should not mark exhausted")
	}

	// Event emitted.
	found := false
	for _, e := range emit.events {
		if e.Type == EventKeyThrottleTransient {
			found = true
			if e.RetryAfter == nil || *e.RetryAfter != 5 {
				t.Errorf("RetryAfter mismatch: %+v", e.RetryAfter)
			}
		}
	}
	if !found {
		t.Errorf("EventKeyThrottleTransient not emitted")
	}
}

func TestEmitter_AcquireAndExhaustEvents(t *testing.T) {
	pool := New(stateFilePath(t), nil)
	emit := &captureEmitter{}
	pool.SetEmitter(emit)

	_ = pool.SeedKeys([]*Key{
		mkKey("sk-a", "primary", nil, time.Now().Add(30*24*time.Hour)),
	}, FreeFallback{})

	k, _ := pool.Acquire()
	_ = pool.MarkExhausted(k.Token, time.Now().Add(7*24*time.Hour), "test")

	types := map[string]int{}
	for _, e := range emit.events {
		types[e.Type]++
	}
	if types[EventKeyAcquired] != 1 {
		t.Errorf("want 1 key_acquired event, got %d", types[EventKeyAcquired])
	}
	if types[EventKeyExhaustedHard] != 1 {
		t.Errorf("want 1 key_exhausted_hard event, got %d", types[EventKeyExhaustedHard])
	}
}

// captureEmitter is a thread-safe in-memory event sink for tests.
type captureEmitter struct {
	mu     sync.Mutex
	events []Event
}

func (c *captureEmitter) Emit(e Event) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, e)
}
