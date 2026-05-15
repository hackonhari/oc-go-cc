package keypool

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// DefaultLogDir is the conventional location for rotation event logs.
// Mirrors the Apify pattern (~/.cache/headsupai/apify-YYYYMMDD.log).
const DefaultLogDir = "~/.cache/oc-go-cc"

// EventLogger implements EventEmitter by appending JSONL records to a
// daily-rotated file at <dir>/rotation-YYYYMMDD.log. Rotation is by
// filename (no in-process rotation logic) — each day's events land in
// a fresh file, simplifying log analysis with grep/jq.
//
// Thread-safe: concurrent Emit() calls are serialized by an internal
// mutex. The mutex is held only for the duration of a single Write,
// not across the network or any blocking I/O.
type EventLogger struct {
	dir string

	mu          sync.Mutex
	currentFile *os.File
	currentDate string // "20060102" of the file currently held open
	logger      *slog.Logger
}

// NewEventLogger constructs an EventLogger writing to <dir>/rotation-YYYYMMDD.log.
// The dir is created on first Emit if it doesn't exist. logger may be nil
// (defaults to slog.Default()) — it's used only to surface internal write
// failures (we never want logging to break the proxy hot path).
func NewEventLogger(dir string, logger *slog.Logger) *EventLogger {
	if logger == nil {
		logger = slog.Default()
	}
	return &EventLogger{
		dir:    dir,
		logger: logger,
	}
}

// Emit serializes the event to JSON and appends it as a single line to
// today's log file. Errors are logged via slog but never returned —
// rotation events must be best-effort so a write failure never breaks
// an active request path.
func (l *EventLogger) Emit(e Event) {
	if e.Timestamp.IsZero() {
		e.Timestamp = time.Now()
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	today := e.Timestamp.UTC().Format("20060102")
	if l.currentDate != today {
		if err := l.rotateLocked(today); err != nil {
			l.logger.Warn("rotation log: cannot open today's file", "err", err)
			return
		}
	}

	data, err := json.Marshal(e)
	if err != nil {
		l.logger.Warn("rotation log: marshal failed", "err", err, "event_type", e.Type)
		return
	}
	if _, err := l.currentFile.Write(append(data, '\n')); err != nil {
		l.logger.Warn("rotation log: write failed", "err", err)
	}
}

// Close releases the currently-held file handle. Safe to call multiple
// times. Always call this from the server's shutdown path so the trailing
// fsync is guaranteed.
func (l *EventLogger) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.currentFile != nil {
		err := l.currentFile.Close()
		l.currentFile = nil
		l.currentDate = ""
		return err
	}
	return nil
}

// CurrentPath returns the absolute path of today's log file (if open) or
// the path that WOULD be opened on next Emit. Useful for keys-status --tail.
func (l *EventLogger) CurrentPath() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.currentFile != nil {
		return l.currentFile.Name()
	}
	today := time.Now().UTC().Format("20060102")
	return filepath.Join(l.dir, "rotation-"+today+".log")
}

// rotateLocked closes the current file (if any) and opens a fresh one
// for the given date. Caller MUST hold the write lock.
func (l *EventLogger) rotateLocked(date string) error {
	if l.currentFile != nil {
		_ = l.currentFile.Close()
		l.currentFile = nil
	}

	if err := os.MkdirAll(l.dir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", l.dir, err)
	}

	path := filepath.Join(l.dir, "rotation-"+date+".log")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	l.currentFile = f
	l.currentDate = date
	return nil
}
