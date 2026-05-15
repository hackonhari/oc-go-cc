// Package freepool resolves requests to OpenCode Zen free-tier models when
// the paid pool is exhausted (or when the request explicitly opted into
// free-only mode via the /free/v1/* path prefix).
//
// Free-tier characteristics (empirically verified 2026-05-15):
//   - Endpoint: https://opencode.ai/zen/v1 (note: NO /go path segment)
//   - Authentication: anonymous (no Authorization header)
//   - Model IDs use the `-free` suffix (e.g. deepseek-v4-flash-free)
//   - Rate limits: not signaled via headers; 50 concurrent reqs sustained 200
//   - Format: same OpenAI-compatible /chat/completions OR Anthropic /messages
//
// The resolver iterates a configured model list in order; on 4xx/5xx from
// one model it falls to the next; ErrAllFreeFailed when none succeed.
package freepool

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"oc-go-cc/internal/keypool"
)

// ErrAllFreeFailed is returned when every free model in the fallback chain
// has been tried and none returned a successful response. Handlers receiving
// this should serve the final structured 502 to the client.
var ErrAllFreeFailed = errors.New("freepool: all free models failed")

// Resolver dispatches requests to the configured free-tier models in
// fallback order.
type Resolver struct {
	baseURL    string
	models     []string
	httpClient *http.Client
	emitter    keypool.EventEmitter
}

// New constructs a Resolver. emitter may be nil (no event logging).
func New(baseURL string, models []string, emitter keypool.EventEmitter) *Resolver {
	transport := &http.Transport{
		DialContext:           (&net.Dialer{Timeout: 30 * time.Second}).DialContext,
		TLSHandshakeTimeout:   30 * time.Second,
		ResponseHeaderTimeout: 60 * time.Second,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   20,
		IdleConnTimeout:       90 * time.Second,
		MaxConnsPerHost:       50,
	}
	r := &Resolver{
		baseURL:    strings.TrimRight(baseURL, "/"),
		models:     append([]string(nil), models...),
		httpClient: &http.Client{Transport: transport},
		emitter:    emitter,
	}
	if r.emitter == nil {
		r.emitter = noopEmitter{}
	}
	return r
}

// Models returns a copy of the configured fallback model list.
func (r *Resolver) Models() []string {
	out := make([]string, len(r.models))
	copy(out, r.models)
	return out
}

// ResolveOpenAI sends an OpenAI-format chat completion request to free
// models in order. The original request body is rewritten with each free
// model in turn. Returns the response from the first model that returns
// a non-error status. mode is "forced" (free-only path) or "fallback"
// (paid pool exhausted).
//
// Model selection priority: if the incoming request specifies a known
// free-tier model (e.g. via --model qwen3.6-plus-free), that model is
// tried FIRST, then the configured fallback chain (deduped). Otherwise
// the configured chain is used in order.
//
// On success, the caller owns the returned response and must close its
// body. On ErrAllFreeFailed, no response is returned.
func (r *Resolver) ResolveOpenAI(
	ctx context.Context,
	originalBody []byte,
	stream bool,
	mode string,
) (*http.Response, error) {
	r.emitter.Emit(keypool.Event{
		Timestamp: time.Now(),
		Type:      keypool.EventFreeFallbackEngaged,
		Mode:      mode,
	})

	for _, model := range r.modelChain(originalBody) {
		body, err := rewriteModelOpenAI(originalBody, model)
		if err != nil {
			return nil, fmt.Errorf("rewrite model %s: %w", model, err)
		}

		url, anthropic := r.endpointForModel(model)
		resp, status, err := r.send(ctx, url, body, stream, anthropic)
		if err != nil {
			r.emitFailure(model, fmt.Sprintf("transport_error: %v", err))
			continue
		}
		if status < 400 {
			return resp, nil
		}
		// On any 4xx/5xx, close body and try next model.
		if resp != nil {
			_ = resp.Body.Close()
		}
		r.emitFailure(model, fmt.Sprintf("status_%d", status))
	}

	return nil, ErrAllFreeFailed
}

// ResolveAnthropic forwards an Anthropic-format raw body to free models
// in order. Same fallback semantics as ResolveOpenAI.
func (r *Resolver) ResolveAnthropic(
	ctx context.Context,
	originalBody []byte,
	stream bool,
	mode string,
) (*http.Response, error) {
	r.emitter.Emit(keypool.Event{
		Timestamp: time.Now(),
		Type:      keypool.EventFreeFallbackEngaged,
		Mode:      mode,
	})

	for _, model := range r.modelChain(originalBody) {
		body, err := rewriteModelAnthropic(originalBody, model)
		if err != nil {
			return nil, fmt.Errorf("rewrite model %s: %w", model, err)
		}

		url, _ := r.endpointForModel(model)
		// For Anthropic path we always hit /messages regardless of model class.
		anthropicURL := r.baseURL + "/messages"
		_ = url // unused — Anthropic path uses fixed endpoint per upstream design
		resp, status, err := r.send(ctx, anthropicURL, body, stream, true)
		if err != nil {
			r.emitFailure(model, fmt.Sprintf("transport_error: %v", err))
			continue
		}
		if status < 400 {
			return resp, nil
		}
		if resp != nil {
			_ = resp.Body.Close()
		}
		r.emitFailure(model, fmt.Sprintf("status_%d", status))
	}

	return nil, ErrAllFreeFailed
}

// send executes the HTTP request and returns the response + status.
// Anonymous: NO Authorization header is set — free tier requires none
// (verified empirically against opencode.ai/zen/v1).
func (r *Resolver) send(
	ctx context.Context,
	url string,
	body []byte,
	stream bool,
	anthropicFormat bool,
) (*http.Response, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, 0, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if stream {
		req.Header.Set("Accept", "text/event-stream")
	}
	// NO Authorization header — free tier is anonymous.

	resp, err := r.httpClient.Do(req)
	if err != nil {
		return nil, 0, err
	}
	return resp, resp.StatusCode, nil
}

// modelChain returns the ordered list of models to try for this request.
// If the incoming body specifies a model that looks like a free-tier
// model, that model is moved to the front of the chain (de-duped against
// the configured list). Otherwise the configured list is returned as-is.
//
// This lets `--model qwen3.6-plus-free` from Claude Code actually pick
// that model, while leaving the existing fallback semantics intact when
// the user didn't pass an explicit free model.
func (r *Resolver) modelChain(body []byte) []string {
	userModel := extractRequestModel(body)
	if !isFreeTierModel(userModel) {
		return r.models
	}

	out := make([]string, 0, len(r.models)+1)
	out = append(out, userModel)
	for _, m := range r.models {
		if m != userModel {
			out = append(out, m)
		}
	}
	return out
}

// extractRequestModel pulls the "model" field from a request body. Returns
// empty string if the body is not parseable or has no model field.
func extractRequestModel(body []byte) string {
	var generic map[string]any
	if err := json.Unmarshal(body, &generic); err != nil {
		return ""
	}
	if m, ok := generic["model"].(string); ok {
		return m
	}
	return ""
}

// isFreeTierModel reports whether the given model ID is a recognized
// Zen free-tier model. Detection: "-free" suffix OR a known free name
// (currently just "big-pickle" — others all use -free suffix).
//
// Anything else (paid model names, aliases like "sonnet", "deepseek-v4-pro")
// returns false — those should NOT bypass the configured chain.
func isFreeTierModel(modelID string) bool {
	if modelID == "" {
		return false
	}
	if strings.HasSuffix(modelID, "-free") {
		return true
	}
	switch modelID {
	case "big-pickle":
		return true
	}
	return false
}

// endpointForModel returns (url, isAnthropicFormat). MiniMax-prefixed
// free models use the Anthropic-format /messages endpoint; everything
// else uses the OpenAI-compatible /chat/completions endpoint.
func (r *Resolver) endpointForModel(modelID string) (string, bool) {
	if strings.HasPrefix(modelID, "minimax-") {
		return r.baseURL + "/messages", true
	}
	return r.baseURL + "/chat/completions", false
}

func (r *Resolver) emitFailure(model, reason string) {
	r.emitter.Emit(keypool.Event{
		Timestamp: time.Now(),
		Type:      keypool.EventFreeFallbackModelFail,
		Model:     model,
		Reason:    reason,
	})
}

// rewriteModelOpenAI swaps the "model" field in an OpenAI-format request
// body with the given free model ID, preserving all other fields.
func rewriteModelOpenAI(body []byte, newModel string) ([]byte, error) {
	var generic map[string]any
	if err := json.Unmarshal(body, &generic); err != nil {
		return nil, fmt.Errorf("parse body: %w", err)
	}
	generic["model"] = newModel
	return json.Marshal(generic)
}

// rewriteModelAnthropic swaps the "model" field in an Anthropic-format
// request body with the given free model ID.
func rewriteModelAnthropic(body []byte, newModel string) ([]byte, error) {
	// Anthropic format uses the same top-level "model" field name.
	return rewriteModelOpenAI(body, newModel)
}

// noopEmitter is the default when no emitter is wired.
type noopEmitter struct{}

func (noopEmitter) Emit(keypool.Event) {}

// drain is exported to satisfy import-only uses in test setups.
func drain(rc io.ReadCloser) {
	if rc == nil {
		return
	}
	_, _ = io.Copy(io.Discard, rc)
	_ = rc.Close()
}

// silence unused import in some build configurations.
var _ = drain
