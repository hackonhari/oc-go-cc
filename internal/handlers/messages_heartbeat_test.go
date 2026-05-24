package handlers

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// panicFlusher is an http.ResponseWriter whose Flush() always panics —
// simulates the 2026-05-19 SIGSEGV from bufio.Writer.Flush on a nil buf
// when the underlying response writer has been torn down. Used to verify
// L2's defer recover() actually catches panics from rw.Flush().
type panicFlusher struct {
	http.ResponseWriter
}

func (p *panicFlusher) Flush() {
	panic("simulated: bufio.Writer.Flush on nil")
}

// newTestHandler builds a minimal MessagesHandler with just a logger.
// The streamHeartbeat method only touches h.logger, so the other fields
// (router, client, transformer) can stay nil for this unit test.
func newTestHandler(logBuf *bytes.Buffer) *MessagesHandler {
	logger := slog.New(slog.NewTextHandler(logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return &MessagesHandler{logger: logger}
}

// TestStreamHeartbeat_ExitsCleanlyOnDoneClose guards AC1 — once the done
// channel is closed, streamHeartbeat must return promptly so the caller's
// WaitGroup.Wait() doesn't block forever. This is the lifecycle contract
// that prevents the goroutine from outliving the handler.
func TestStreamHeartbeat_ExitsCleanlyOnDoneClose(t *testing.T) {
	h := newTestHandler(&bytes.Buffer{})
	rw := &responseWriter{ResponseWriter: httptest.NewRecorder()}
	done := make(chan struct{})

	finished := make(chan struct{})
	go func() {
		h.streamHeartbeat(context.Background(), rw, done)
		close(finished)
	}()

	// Give heartbeat a moment to enter select loop.
	time.Sleep(50 * time.Millisecond)
	close(done)

	select {
	case <-finished:
		// Expected — clean exit
	case <-time.After(1 * time.Second):
		t.Fatal("streamHeartbeat did not exit within 1s of done close")
	}
}

// TestStreamHeartbeat_ExitsCleanlyOnContextCancel — same lifecycle
// contract but via the ctx path (used when Claude Code disconnects).
func TestStreamHeartbeat_ExitsCleanlyOnContextCancel(t *testing.T) {
	h := newTestHandler(&bytes.Buffer{})
	rw := &responseWriter{ResponseWriter: httptest.NewRecorder()}
	done := make(chan struct{})
	defer close(done) // cleanup so goroutine doesn't leak if ctx path fails

	ctx, cancel := context.WithCancel(context.Background())
	finished := make(chan struct{})
	go func() {
		h.streamHeartbeat(ctx, rw, done)
		close(finished)
	}()

	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case <-finished:
		// Expected
	case <-time.After(1 * time.Second):
		t.Fatal("streamHeartbeat did not exit within 1s of ctx cancel")
	}
}

// TestStreamHeartbeat_RecoversFromFlushPanic guards AC2 — this is the
// regression test for the 2026-05-19 production crash. With the panicking
// Flush, the heartbeat goroutine MUST NOT propagate the panic up to its
// caller (which would crash the proxy process). It must:
//   - Recover internally
//   - Log the panic via slog at ERROR level
//   - Return cleanly from streamHeartbeat
//
// We use a fast ticker workaround: instead of waiting 3 seconds for the
// real ticker, we wait long enough for at least one tick (3.2 seconds).
// The alternative (parameterizing the ticker) is left for a future
// refactor — this test deliberately exercises the production timing.
func TestStreamHeartbeat_RecoversFromFlushPanic(t *testing.T) {
	if testing.Short() {
		t.Skip("waits for the 3-second real ticker; ~3.2s total")
	}

	logBuf := &bytes.Buffer{}
	h := newTestHandler(logBuf)

	rw := &responseWriter{ResponseWriter: &panicFlusher{
		ResponseWriter: httptest.NewRecorder(),
	}}
	done := make(chan struct{})

	finished := make(chan struct{})
	go func() {
		// If recover() fails, this goroutine panics and the runtime
		// terminates the test binary. Reaching close(finished) means
		// the recover() in streamHeartbeat caught the panic.
		h.streamHeartbeat(context.Background(), rw, done)
		close(finished)
	}()

	// Wait for at least one tick (3 seconds) so Flush() fires and panics.
	time.Sleep(3500 * time.Millisecond)
	close(done)

	select {
	case <-finished:
		// Expected — recover caught the panic, function returned normally
	case <-time.After(1 * time.Second):
		t.Fatal("streamHeartbeat hung after panic — recover() may have failed")
	}

	// Verify the panic was logged so operators can see it in production.
	logged := logBuf.String()
	if !strings.Contains(logged, "heartbeat goroutine panic recovered") {
		t.Errorf("expected recovery log entry, got log buffer:\n%s", logged)
	}
	if !strings.Contains(logged, "simulated: bufio.Writer.Flush on nil") {
		t.Errorf("expected panic message in log, got:\n%s", logged)
	}
}

// TestStreamHeartbeat_WaitGroupContract is a structural check of the
// lifecycle pattern: when the caller does the recommended sync.WaitGroup
// wrap, Wait() must NOT return until streamHeartbeat has finished.
//
// This is the test that would have caught the 2026-05-19 bug if it had
// existed earlier — the old code only used `close(done)` without Wait().
func TestStreamHeartbeat_WaitGroupContract(t *testing.T) {
	h := newTestHandler(&bytes.Buffer{})
	rw := &responseWriter{ResponseWriter: httptest.NewRecorder()}
	done := make(chan struct{})

	// Simulate the caller's pattern verbatim from handleStreaming.
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		h.streamHeartbeat(context.Background(), rw, done)
	}()

	// Caller's cleanup: close(done) then Wait().
	close(done)

	// Wait must return promptly because the goroutine has exited.
	doneWaiting := make(chan struct{})
	go func() {
		wg.Wait()
		close(doneWaiting)
	}()

	select {
	case <-doneWaiting:
		// Expected — Wait returned because streamHeartbeat exited
	case <-time.After(1 * time.Second):
		t.Fatal("WaitGroup.Wait() did not return within 1s — lifecycle contract violated")
	}
}

// _ = io.Discard ensures unused-import doesn't trip if a future test
// drops its rw assertion. Kept tiny + explicit.
var _ = io.Discard
