package keypool

import (
	"encoding/json"
	"os"
	"testing"
	"time"
)

func TestHotReload_NewKeyAppearsAfterExternalEdit(t *testing.T) {
	path := stateFilePath(t)

	// Initial seed with one key.
	pool := New(path, nil)
	_ = pool.SeedKeys([]*Key{
		mkKey("sk-a", "primary", nil, time.Now().Add(30*24*time.Hour)),
	}, FreeFallback{})

	// Acquire to set LastUsed in-memory.
	_, _ = pool.Acquire()
	beforeSnap := pool.Snapshot()
	if beforeSnap[0].LastUsed == nil {
		t.Fatalf("LastUsed should be set after Acquire")
	}
	primaryLastUsed := *beforeSnap[0].LastUsed

	// External edit: add a second key directly to the state file.
	// Use a forward-dated mtime so the reload trigger fires deterministically.
	sf := stateFile{
		APIKeys: []*Key{
			mkKey("sk-a", "primary", nil, time.Now().Add(30*24*time.Hour)),
			mkKey("sk-b", "newcomer", nil, time.Now().Add(30*24*time.Hour)),
		},
	}
	data, _ := json.MarshalIndent(sf, "", "  ")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("external edit: %v", err)
	}
	// Bump mtime explicitly so our internal lastMtime is < disk mtime.
	future := time.Now().Add(1 * time.Hour)
	if err := os.Chtimes(path, future, future); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	pool.checkAndReload()

	afterSnap := pool.Snapshot()
	if len(afterSnap) != 2 {
		t.Fatalf("expected 2 keys after reload, got %d", len(afterSnap))
	}
	if afterSnap[1].Token != "sk-b" {
		t.Errorf("expected sk-b at index 1, got %q", afterSnap[1].Token)
	}

	// LastUsed for sk-a should be preserved across reload.
	if afterSnap[0].LastUsed == nil || !afterSnap[0].LastUsed.Equal(primaryLastUsed) {
		t.Errorf("LastUsed should be preserved across reload: got %v, want %v",
			afterSnap[0].LastUsed, primaryLastUsed)
	}
}

func TestHotReload_NoChangeIsNoop(t *testing.T) {
	path := stateFilePath(t)
	pool := New(path, nil)
	_ = pool.SeedKeys([]*Key{
		mkKey("sk-a", "primary", nil, time.Now().Add(30*24*time.Hour)),
	}, FreeFallback{})

	beforeSnap := pool.Snapshot()
	pool.checkAndReload()
	afterSnap := pool.Snapshot()

	if len(beforeSnap) != len(afterSnap) {
		t.Errorf("snapshot length changed: %d -> %d", len(beforeSnap), len(afterSnap))
	}
}

func TestHotReload_CorruptedExternalEditIsIgnored(t *testing.T) {
	path := stateFilePath(t)
	pool := New(path, nil)
	_ = pool.SeedKeys([]*Key{
		mkKey("sk-a", "primary", nil, time.Now().Add(30*24*time.Hour)),
	}, FreeFallback{})

	// Corrupt the file externally.
	if err := os.WriteFile(path, []byte("{ not json"), 0o600); err != nil {
		t.Fatalf("corrupt: %v", err)
	}
	future := time.Now().Add(1 * time.Hour)
	_ = os.Chtimes(path, future, future)

	pool.checkAndReload()

	// Pool should be unchanged (parse failed, skip).
	snap := pool.Snapshot()
	if len(snap) != 1 || snap[0].Token != "sk-a" {
		t.Errorf("pool should be unchanged on corrupt reload: %+v", snap)
	}
}
