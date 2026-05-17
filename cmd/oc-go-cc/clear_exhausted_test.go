package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeTestStateFile creates a state file with one exhausted key and one
// active key. Returns the path.
func writeTestStateFile(t *testing.T) string {
	t.Helper()
	exhausted := time.Now().Add(-1 * time.Hour)
	reset := time.Now().Add(30 * 24 * time.Hour)
	sf := keyStateFile{
		APIKeys: []keyStateRow{
			{Token: "sk-a", Account: "primary", ExhaustedAt: &exhausted, ResetDate: reset},
			{Token: "sk-b", Account: "secondary"},
		},
		FreeFallback: freeFallbackBlock{BaseURL: "https://test.invalid", Models: []string{"x"}},
	}
	data, err := json.MarshalIndent(sf, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	path := filepath.Join(t.TempDir(), "key-states.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	return path
}

func readState(t *testing.T, path string) *keyStateFile {
	t.Helper()
	state, err := loadKeyState(path)
	if err != nil {
		t.Fatalf("loadKeyState: %v", err)
	}
	return state
}

func TestClearKeys_SingleAccount(t *testing.T) {
	path := writeTestStateFile(t)
	state := readState(t, path)

	cleared, err := clearKeys(state, []string{"primary"}, false)
	if err != nil {
		t.Fatalf("clearKeys: %v", err)
	}
	if len(cleared) != 1 || cleared[0] != "primary" {
		t.Errorf("cleared = %v, want [primary]", cleared)
	}
	if state.APIKeys[0].ExhaustedAt != nil {
		t.Errorf("primary ExhaustedAt should be nil, got %v", state.APIKeys[0].ExhaustedAt)
	}
	if !state.APIKeys[0].ResetDate.IsZero() {
		t.Errorf("primary ResetDate should be zero, got %v", state.APIKeys[0].ResetDate)
	}
	// secondary untouched
	if state.APIKeys[1].ExhaustedAt != nil {
		t.Errorf("secondary should remain active")
	}
}

func TestClearKeys_AllFlag(t *testing.T) {
	path := writeTestStateFile(t)
	state := readState(t, path)

	// Pre-exhaust secondary too so --all has multiple to clear.
	now := time.Now()
	state.APIKeys[1].ExhaustedAt = &now
	state.APIKeys[1].ResetDate = time.Now().Add(7 * 24 * time.Hour)

	cleared, err := clearKeys(state, nil, true)
	if err != nil {
		t.Fatalf("clearKeys --all: %v", err)
	}
	if len(cleared) != 2 {
		t.Errorf("cleared = %d accounts, want 2", len(cleared))
	}
	for _, k := range state.APIKeys {
		if k.ExhaustedAt != nil {
			t.Errorf("%s should be cleared", k.Account)
		}
	}
}

func TestClearKeys_AllWithNothingExhausted_Errors(t *testing.T) {
	state := &keyStateFile{
		APIKeys: []keyStateRow{
			{Token: "sk-a", Account: "primary"},
		},
	}
	_, err := clearKeys(state, nil, true)
	if err == nil {
		t.Error("expected error when --all is used on a pool with no exhausted keys")
	}
}

func TestClearKeys_UnknownAccount_Errors(t *testing.T) {
	path := writeTestStateFile(t)
	state := readState(t, path)

	_, err := clearKeys(state, []string{"does-not-exist"}, false)
	if err == nil {
		t.Error("expected error for unknown account")
	}
}

func TestClearKeys_AlreadyActiveAccount_NoOp(t *testing.T) {
	path := writeTestStateFile(t)
	state := readState(t, path)

	// secondary is already active — clearKeys should be a no-op (no error, no clears)
	cleared, err := clearKeys(state, []string{"secondary"}, false)
	if err != nil {
		t.Fatalf("clearKeys: %v", err)
	}
	if len(cleared) != 0 {
		t.Errorf("cleared = %v, want empty (no-op on active key)", cleared)
	}
}

func TestSaveKeyState_RoundTrip(t *testing.T) {
	path := writeTestStateFile(t)
	state := readState(t, path)

	// Clear and save.
	_, _ = clearKeys(state, []string{"primary"}, false)
	if err := saveKeyState(path, state); err != nil {
		t.Fatalf("saveKeyState: %v", err)
	}

	// Re-read and confirm primary is active.
	reread := readState(t, path)
	if reread.APIKeys[0].ExhaustedAt != nil {
		t.Errorf("after save+reload, primary should still be cleared")
	}
}

func TestSaveKeyState_AtomicWrite(t *testing.T) {
	path := writeTestStateFile(t)
	state := readState(t, path)

	// Confirm no .tmp file lingers after a successful save.
	if err := saveKeyState(path, state); err != nil {
		t.Fatalf("saveKeyState: %v", err)
	}
	if _, err := os.Stat(path + ".tmp"); err == nil {
		t.Errorf(".tmp file should not linger after successful rename")
	}
}
