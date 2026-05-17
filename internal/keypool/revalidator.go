package keypool

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"time"
)

// Revalidator periodically probes exhausted keys against the upstream and
// clears the exhaustion state if the probe succeeds. This auto-heals false
// positives from the 429 classifier — particularly burst-throttle 429s that
// look identical to the no-signal "no header, no body" case.
//
// Background: on 2026-05-17 the classifier was found to hard-exhaust healthy
// keys for 30 days when OpenCode returned an unsignalled 429. Phase 1 of the
// fix flipped the classifier to Transient for that case. This revalidator is
// the second line of defense — if any future upstream shape still produces a
// false positive, the system self-heals within one tick.
type Revalidator struct {
	pool       *KeyPool
	probeURL   string // OpenCode Go base_url for /chat/completions
	probeModel string // model to use for the 10-token probe
	interval   time.Duration
	httpClient *http.Client
	logger     *slog.Logger
}

const (
	defaultRevalidationInterval = 6 * time.Hour
	defaultProbeModel           = "deepseek-v4-flash"
	probeTimeoutSeconds         = 10
)

// NewRevalidator returns a Revalidator with the production default interval
// (6h). The probe URL is the OpenCode Go chat/completions endpoint configured
// in the proxy. logger may be nil; defaults to slog.Default().
func NewRevalidator(pool *KeyPool, probeURL string, logger *slog.Logger) *Revalidator {
	return NewRevalidatorWithInterval(pool, probeURL, defaultRevalidationInterval, logger)
}

// NewRevalidatorWithInterval is the test-only seam allowing a shorter tick
// for integration tests that need to observe revival within seconds rather
// than hours.
func NewRevalidatorWithInterval(pool *KeyPool, probeURL string, interval time.Duration, logger *slog.Logger) *Revalidator {
	if logger == nil {
		logger = slog.Default()
	}
	return &Revalidator{
		pool:       pool,
		probeURL:   probeURL,
		probeModel: defaultProbeModel,
		interval:   interval,
		httpClient: &http.Client{Timeout: probeTimeoutSeconds * time.Second},
		logger:     logger,
	}
}

// Run drives the revalidation ticker loop until ctx is cancelled. Intended
// to be launched in a goroutine by the proxy startup path. Safe to call
// once per pool — multiple Run calls on the same Revalidator are not
// guarded and will produce concurrent probe traffic.
//
// Loop semantics:
//   - One immediate tick on start (so freshly-rebooted proxies probe
//     instantly rather than waiting a full interval)
//   - Then every interval, runs probeOnce against all exhausted keys
//   - Returns cleanly when ctx is Done
func (r *Revalidator) Run(ctx context.Context) {
	r.logger.Info("revalidator starting", "interval", r.interval, "probe_url", r.probeURL)

	r.probeOnce(ctx)

	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			r.logger.Info("revalidator shutting down")
			return
		case <-ticker.C:
			r.probeOnce(ctx)
		}
	}
}

// probeOnce iterates the current snapshot and probes every exhausted key.
// Probes happen sequentially so we never burst-load the upstream during a
// tick. A failing probe is logged but does not stop the iteration.
func (r *Revalidator) probeOnce(ctx context.Context) {
	for _, k := range r.pool.Snapshot() {
		if k.IsActive() {
			continue
		}
		if ctx.Err() != nil {
			return
		}
		r.probeKey(ctx, k)
	}
}

// probeKey sends a minimal 10-token request and acts on the response:
//   - HTTP 200 → pool.ClearExhausted (key revived, emits key_revived event)
//   - HTTP 401/429 / network failure → log debug, leave exhausted (no spam)
func (r *Revalidator) probeKey(ctx context.Context, k Key) {
	body := map[string]any{
		"model":      r.probeModel,
		"max_tokens": 5,
		"messages":   []map[string]string{{"role": "user", "content": "ok"}},
	}
	data, err := json.Marshal(body)
	if err != nil {
		r.logger.Error("revalidator marshal failed", "err", err)
		return
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.probeURL, bytes.NewReader(data))
	if err != nil {
		r.logger.Error("revalidator build request failed", "err", err, "account", k.Account)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+k.Token)

	resp, err := r.httpClient.Do(req)
	if err != nil {
		r.logger.Debug("revalidator probe network error", "account", k.Account, "err", err)
		return
	}
	defer resp.Body.Close()
	// Drain a small amount to allow connection reuse; ignore body.
	_, _ = io.CopyN(io.Discard, resp.Body, 1024)

	if resp.StatusCode == http.StatusOK {
		if err := r.pool.ClearExhausted(k.Token, "revalidation_succeeded"); err != nil {
			r.logger.Error("revalidator clear-exhausted failed", "account", k.Account, "err", err)
			return
		}
		r.logger.Info("revalidator revived key", "account", k.Account)
		return
	}

	// Not 200 — leave the key exhausted. Emit a low-noise event for
	// operators inspecting the log; debug-level only.
	r.logger.Debug(
		"revalidator probe still failing",
		"account", k.Account,
		"status", resp.StatusCode,
	)
}
