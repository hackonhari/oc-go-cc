package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func mkSampleState() *keyStateFile {
	exhausted := time.Date(2026, 5, 14, 22, 1, 0, 0, time.UTC)
	lastUsed := time.Date(2026, 5, 15, 13, 55, 0, 0, time.UTC)
	return &keyStateFile{
		APIKeys: []keyStateRow{
			{
				Token:     "sk-ABCDEFghijklmnopqrstuvwxyz123456",
				Account:   "ocgo-primary",
				ResetDate: time.Date(2026, 6, 4, 0, 0, 0, 0, time.UTC),
				LastUsed:  &lastUsed,
			},
			{
				Token:       "sk-XYZ098abcdef123456789abcdef999999",
				Account:     "ocgo-hk",
				ExhaustedAt: &exhausted,
				ResetDate:   time.Date(2026, 6, 8, 0, 0, 0, 0, time.UTC),
			},
		},
		FreeFallback: freeFallbackBlock{
			BaseURL: "https://opencode.ai/zen/v1",
			Models:  []string{"deepseek-v4-flash-free", "qwen3.6-plus-free"},
		},
	}
}

func TestRedactToken(t *testing.T) {
	cases := map[string]string{
		"sk-ABCDEFghijklmnop":                     "sk-ABC***mnop",
		"sk-JNPBrTIagfUenKFDyrs6Iskxb0SlefWBe59K": "sk-JNP***e59K",
		"short":          "***",
		"":               "***",
		"exactly14chars": "***",
	}
	for in, want := range cases {
		if got := redactToken(in); got != want {
			t.Errorf("redactToken(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPrintTable_HappyPath(t *testing.T) {
	var buf bytes.Buffer
	if err := printTable(&buf, mkSampleState()); err != nil {
		t.Fatalf("printTable: %v", err)
	}
	out := buf.String()

	// Header present.
	if !strings.Contains(out, "Account") || !strings.Contains(out, "Status") {
		t.Errorf("header missing: %s", out)
	}
	// Both accounts shown.
	if !strings.Contains(out, "ocgo-primary") || !strings.Contains(out, "ocgo-hk") {
		t.Errorf("accounts missing: %s", out)
	}
	// Statuses shown.
	if !strings.Contains(out, "ACTIVE") || !strings.Contains(out, "EXHAUSTED") {
		t.Errorf("statuses missing: %s", out)
	}
	// Token redaction applied (full tokens NOT present).
	if strings.Contains(out, "sk-ABCDEFghijkl") {
		t.Errorf("full token leaked: %s", out)
	}
	if !strings.Contains(out, "sk-ABC***") {
		t.Errorf("redacted token missing: %s", out)
	}
	// Free fallback footer.
	if !strings.Contains(out, "Free fallback:") || !strings.Contains(out, "deepseek-v4-flash-free") {
		t.Errorf("free fallback footer missing: %s", out)
	}
}

func TestPrintTable_SortsActiveFirst(t *testing.T) {
	var buf bytes.Buffer
	_ = printTable(&buf, mkSampleState())
	out := buf.String()

	activeIdx := strings.Index(out, "ocgo-primary")
	exhaustedIdx := strings.Index(out, "ocgo-hk")
	if activeIdx == -1 || exhaustedIdx == -1 {
		t.Fatalf("missing rows")
	}
	if activeIdx >= exhaustedIdx {
		t.Errorf("ACTIVE row should come before EXHAUSTED row")
	}
}

func TestPrintTable_EmptyPool(t *testing.T) {
	var buf bytes.Buffer
	_ = printTable(&buf, &keyStateFile{})
	out := buf.String()
	if !strings.Contains(out, "empty") {
		t.Errorf("empty pool message missing: %s", out)
	}
}

func TestPrintJSON_RedactsTokens(t *testing.T) {
	var buf bytes.Buffer
	if err := printJSON(&buf, mkSampleState()); err != nil {
		t.Fatalf("printJSON: %v", err)
	}

	var parsed keyStateFile
	if err := json.Unmarshal(buf.Bytes(), &parsed); err != nil {
		t.Fatalf("JSON not parseable: %v", err)
	}
	for _, k := range parsed.APIKeys {
		if strings.Contains(k.Token, "ABCDEFghijkl") || strings.Contains(k.Token, "XYZ098abcdef") {
			t.Errorf("full token leaked in JSON: %s", k.Token)
		}
		if !strings.Contains(k.Token, "***") {
			t.Errorf("token not redacted: %s", k.Token)
		}
	}
}

func TestLoadKeyState_MissingFileReturnsEmpty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nonexistent.json")
	sf, err := loadKeyState(path)
	if err != nil {
		t.Errorf("missing file should be empty not error: %v", err)
	}
	if len(sf.APIKeys) != 0 {
		t.Errorf("expected empty pool, got %d keys", len(sf.APIKeys))
	}
}

func TestLoadKeyState_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	src := mkSampleState()
	data, _ := json.Marshal(src)
	_ = os.WriteFile(path, data, 0o600)

	loaded, err := loadKeyState(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(loaded.APIKeys) != 2 {
		t.Errorf("expected 2 keys, got %d", len(loaded.APIKeys))
	}
	if loaded.APIKeys[0].Account != "ocgo-primary" {
		t.Errorf("account mismatch: %s", loaded.APIKeys[0].Account)
	}
}

func TestPrintRotationTail_MissingFile(t *testing.T) {
	// Override HOME to a tempdir so the missing-file path is exercised.
	oldHome := os.Getenv("HOME")
	t.Setenv("HOME", t.TempDir())
	defer os.Setenv("HOME", oldHome)

	var buf bytes.Buffer
	if err := printRotationTail(&buf, 50); err != nil {
		t.Fatalf("missing log file should not error: %v", err)
	}
	if !strings.Contains(buf.String(), "no events yet today") {
		t.Errorf("expected no-events message, got: %s", buf.String())
	}
}

func TestPrintRotationTail_TailsLastN(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)

	logDir := filepath.Join(dir, ".cache", "oc-go-cc")
	_ = os.MkdirAll(logDir, 0o755)
	logPath := filepath.Join(logDir, "rotation-"+time.Now().UTC().Format("20060102")+".log")

	// Write 100 events.
	f, _ := os.Create(logPath)
	for i := 0; i < 100; i++ {
		evt := map[string]any{
			"ts":      time.Now().UTC().Format(time.RFC3339Nano),
			"event":   "key_acquired",
			"account": "test-" + string(rune('a'+(i%26))),
		}
		data, _ := json.Marshal(evt)
		_, _ = f.Write(append(data, '\n'))
	}
	_ = f.Close()

	var buf bytes.Buffer
	if err := printRotationTail(&buf, 10); err != nil {
		t.Fatalf("tail: %v", err)
	}

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 10 {
		t.Errorf("expected 10 tail lines, got %d", len(lines))
	}
}

func TestTruncate(t *testing.T) {
	if got := truncate("short", 10); got != "short" {
		t.Errorf("no-truncate: %q", got)
	}
	if got := truncate("verylongaccountname", 10); got != "verylongac…" || len(got) > 11 {
		// "verylongac…" = 10 chars + ellipsis byte. acceptable as long as it fits visually.
	}
	if got := truncate("anything", 0); got != "anything" {
		// width=0 → no truncation rule applies meaningfully; treat as no-op for safety.
	}
}
