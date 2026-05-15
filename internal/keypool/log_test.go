package keypool

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestEventLogger_AppendsJSONLines(t *testing.T) {
	dir := t.TempDir()
	l := NewEventLogger(dir, nil)
	defer l.Close()

	events := []Event{
		{Type: EventKeyAcquired, Account: "primary"},
		{Type: EventKeyExhaustedHard, Account: "primary", Reason: "credits_exhausted"},
		{Type: EventFreeFallbackEngaged, Mode: "fallback"},
	}
	for _, e := range events {
		l.Emit(e)
	}

	// Find the file.
	files, _ := filepath.Glob(filepath.Join(dir, "rotation-*.log"))
	if len(files) != 1 {
		t.Fatalf("expected 1 log file, got %d: %v", len(files), files)
	}

	f, err := os.Open(files[0])
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	var read []Event
	for scanner.Scan() {
		var e Event
		if err := json.Unmarshal(scanner.Bytes(), &e); err != nil {
			t.Errorf("bad JSON: %v line=%s", err, scanner.Text())
			continue
		}
		read = append(read, e)
	}

	if len(read) != 3 {
		t.Fatalf("expected 3 events, got %d", len(read))
	}
	if read[0].Type != EventKeyAcquired {
		t.Errorf("event 0 type = %s", read[0].Type)
	}
	if read[1].Reason != "credits_exhausted" {
		t.Errorf("event 1 reason = %s", read[1].Reason)
	}
	if read[2].Mode != "fallback" {
		t.Errorf("event 2 mode = %s", read[2].Mode)
	}
}

func TestEventLogger_DailyRotation(t *testing.T) {
	dir := t.TempDir()
	l := NewEventLogger(dir, nil)
	defer l.Close()

	// Emit event with explicit yesterday timestamp.
	yesterday := time.Now().Add(-24 * time.Hour)
	l.Emit(Event{Type: EventKeyAcquired, Account: "yesterday", Timestamp: yesterday})

	// Emit event with today's timestamp.
	l.Emit(Event{Type: EventKeyAcquired, Account: "today", Timestamp: time.Now()})

	files, _ := filepath.Glob(filepath.Join(dir, "rotation-*.log"))
	if len(files) != 2 {
		t.Fatalf("expected 2 daily-rotated files, got %d: %v", len(files), files)
	}

	// Each file contains exactly one record from the correct day.
	for _, path := range files {
		data, _ := os.ReadFile(path)
		var e Event
		_ = json.Unmarshal([]byte(strings.TrimSpace(string(data))), &e)
		// Confirm filename date matches event date.
		base := filepath.Base(path)
		if !strings.HasPrefix(base, "rotation-"+e.Timestamp.UTC().Format("20060102")) {
			t.Errorf("event date mismatch: file=%s, event.ts=%v", base, e.Timestamp)
		}
	}
}

func TestEventLogger_AutoTimestampWhenZero(t *testing.T) {
	dir := t.TempDir()
	l := NewEventLogger(dir, nil)
	defer l.Close()

	l.Emit(Event{Type: EventKeyAcquired}) // no Timestamp set

	files, _ := filepath.Glob(filepath.Join(dir, "rotation-*.log"))
	if len(files) != 1 {
		t.Fatalf("expected 1 file, got %d", len(files))
	}
	data, _ := os.ReadFile(files[0])
	var e Event
	_ = json.Unmarshal(data[:len(data)-1], &e) // strip trailing newline
	if e.Timestamp.IsZero() {
		t.Errorf("zero Timestamp should be auto-set by Emit")
	}
}

func TestEventLogger_DirAutoCreated(t *testing.T) {
	parent := t.TempDir()
	dir := filepath.Join(parent, "deep", "nested", "logs")
	// dir does not yet exist
	l := NewEventLogger(dir, nil)
	defer l.Close()

	l.Emit(Event{Type: EventKeyAcquired})

	if _, err := os.Stat(dir); err != nil {
		t.Errorf("dir should be auto-created: %v", err)
	}
}

func TestEventLogger_ConcurrentEmits(t *testing.T) {
	dir := t.TempDir()
	l := NewEventLogger(dir, nil)
	defer l.Close()

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				l.Emit(Event{
					Type:    EventKeyAcquired,
					Account: "p" + string(rune('a'+(n%26))),
				})
			}
		}(i)
	}
	wg.Wait()

	// All 1000 events should be valid JSON lines.
	files, _ := filepath.Glob(filepath.Join(dir, "rotation-*.log"))
	if len(files) != 1 {
		t.Fatalf("expected 1 file, got %d", len(files))
	}
	f, _ := os.Open(files[0])
	defer f.Close()

	scanner := bufio.NewScanner(f)
	count := 0
	for scanner.Scan() {
		var e Event
		if err := json.Unmarshal(scanner.Bytes(), &e); err != nil {
			t.Errorf("malformed JSON line under concurrency: %s err=%v", scanner.Text(), err)
		}
		count++
	}
	if count != 1000 {
		t.Errorf("expected 1000 events, got %d (some writes interleaved or dropped)", count)
	}
}

func TestEventLogger_CurrentPath(t *testing.T) {
	dir := t.TempDir()
	l := NewEventLogger(dir, nil)

	// Before any Emit: path is computed for today.
	path1 := l.CurrentPath()
	if !strings.Contains(path1, "rotation-") || !strings.HasSuffix(path1, ".log") {
		t.Errorf("CurrentPath shape unexpected: %s", path1)
	}

	l.Emit(Event{Type: EventKeyAcquired})
	defer l.Close()

	path2 := l.CurrentPath()
	if path1 != path2 {
		t.Errorf("path drifted unexpectedly: %s vs %s", path1, path2)
	}
}
