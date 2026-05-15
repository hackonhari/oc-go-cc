// Package client manages upstream API client connections with built-in
// key rotation and upstream-error classification.
//
// The client owns a *keypool.KeyPool and a keypool.RotationSignal
// classifier. On every outbound request, a key is Acquire()'d, the
// request is sent, and any 401/429 response is fed through the classifier:
//   - DecisionTransient → backoff + retry the SAME key (up to maxTransientRetries)
//   - DecisionHard      → MarkExhausted + Acquire next key + retry
//   - DecisionAmbiguous → treated as Hard
//
// 5xx upstream errors are NOT the key's fault; they propagate to the
// caller without touching pool state. ErrAllExhausted bubbles up when no
// active key remains — the handler converts that to a free-fallback path.
//
// Streaming compatibility: rotation happens before the first SSE byte
// is read. Once a 200 response is returned to the caller and streaming
// begins, mid-stream errors are caller-handled, not rotation triggers.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"

	"oc-go-cc/internal/config"
	"oc-go-cc/internal/freepool"
	"oc-go-cc/internal/keypool"
	"oc-go-cc/pkg/types"
)

// maxTransientRetries is the number of retries on the SAME key when the
// classifier returns DecisionTransient. After hitting this ceiling, the
// classifier's transient becomes effectively hard (we rotate).
const maxTransientRetries = 2

// ErrAllKeysExhausted is the typed signal the handler layer watches for
// to engage free-fallback. It wraps keypool.ErrAllExhausted with extra
// context (e.g. the earliest reset date) for the 502 response body.
var ErrAllKeysExhausted = errors.New("client: all paid keys exhausted")

// freeOnlyContextKey is the unexported context key used by WithFreeOnly /
// isFreeOnlyContext to mark a request as bypassing the paid pool.
type freeOnlyContextKey struct{}

// WithFreeOnly returns a child context that signals the client to skip
// the paid pool entirely and route directly to the free resolver.
// Used by the /free/v1/* HTTP routes (Phase 6 dual-endpoint).
func WithFreeOnly(ctx context.Context) context.Context {
	return context.WithValue(ctx, freeOnlyContextKey{}, true)
}

// isFreeOnlyContext reports whether the context was tagged WithFreeOnly.
func isFreeOnlyContext(ctx context.Context) bool {
	v, _ := ctx.Value(freeOnlyContextKey{}).(bool)
	return v
}

// OpenCodeClient handles communication with OpenCode Go API, using a
// keypool for rotation and a classifier for 401/429 decisions. When the
// paid pool is exhausted (ErrAllExhausted), the client automatically
// falls through to the freepool resolver (Zen free models, anonymous).
type OpenCodeClient struct {
	openAIURL    string
	anthropicURL string
	pool         *keypool.KeyPool
	classifier   keypool.RotationSignal
	freeResolver *freepool.Resolver
	httpClient   *http.Client
}

// NewOpenCodeClient creates a new OpenCode Go client. The pool MUST be
// pre-seeded (Load or SeedKeys called) before any request is sent.
// freeResolver may be nil — in that case, paid pool exhaustion bubbles
// up as ErrAllKeysExhausted instead of being silently rescued by free.
func NewOpenCodeClient(
	cfg config.OpenCodeGoConfig,
	pool *keypool.KeyPool,
	classifier keypool.RotationSignal,
	freeResolver *freepool.Resolver,
) *OpenCodeClient {
	// Transport-level timeouts only — no client-level Timeout.
	// A blanket client.Timeout covers body reads too, which kills
	// long-running streaming requests (DeepSeek V4 Pro reasoning
	// can take 10+ minutes). Time out connection establishment
	// phases individually and let streaming body reads run as long
	// as the client stays connected.
	transport := &http.Transport{
		DialContext:           (&net.Dialer{Timeout: 30 * time.Second}).DialContext,
		TLSHandshakeTimeout:   30 * time.Second,
		ResponseHeaderTimeout: 60 * time.Second,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   20,
		IdleConnTimeout:       90 * time.Second,
		MaxConnsPerHost:       50,
		DisableKeepAlives:     false,
	}

	return &OpenCodeClient{
		openAIURL:    cfg.BaseURL,
		anthropicURL: cfg.AnthropicBaseURL,
		pool:         pool,
		classifier:   classifier,
		freeResolver: freeResolver,
		httpClient: &http.Client{
			Transport: transport,
		},
	}
}

// IsAnthropicModel returns true if the model requires the Anthropic endpoint.
func IsAnthropicModel(modelID string) bool {
	switch modelID {
	case "minimax-m2.5", "minimax-m2.7":
		return true
	default:
		return false
	}
}

// urlForModel returns the correct upstream URL for a model.
func (c *OpenCodeClient) urlForModel(modelID string) string {
	if IsAnthropicModel(modelID) {
		return c.anthropicURL
	}
	return c.openAIURL
}

// doWithRotation is the rotation loop used by all public request methods.
// It Acquire()s a key, sends the request, inspects status, and either
// returns the response, retries with the same key (transient), or rotates
// to the next key (hard). Returns ErrAllKeysExhausted when pool is dry.
//
// The body byte slice is read on each retry attempt (must be cheap to
// re-create from the original request — that's why doWithRotation takes
// a bodyBuilder function rather than a fixed slice).
func (c *OpenCodeClient) doWithRotation(
	ctx context.Context,
	method, url string,
	bodyBuilder func() []byte,
	headers map[string]string,
) (*http.Response, error) {
	for {
		key, err := c.pool.Acquire()
		if err != nil {
			if errors.Is(err, keypool.ErrAllExhausted) {
				return nil, ErrAllKeysExhausted
			}
			return nil, fmt.Errorf("acquire key: %w", err)
		}

		// Inner loop handles same-key transient retries.
		transientAttempts := 0
		for {
			body := bodyBuilder()
			httpReq, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(body))
			if err != nil {
				return nil, fmt.Errorf("build request: %w", err)
			}
			httpReq.Header.Set("Content-Type", "application/json")
			httpReq.Header.Set("Authorization", "Bearer "+key.Token)
			for k, v := range headers {
				httpReq.Header.Set(k, v)
			}

			resp, err := c.httpClient.Do(httpReq)
			if err != nil {
				return nil, fmt.Errorf("request failed: %w", err)
			}

			// Success path.
			if resp.StatusCode < 400 {
				return resp, nil
			}

			// 5xx upstream errors: not the key's fault. Return as-is.
			if resp.StatusCode >= 500 {
				return resp, nil
			}

			// 401 / 429 → classifier path.
			if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusTooManyRequests {
				respBody, _ := io.ReadAll(resp.Body)
				_ = resp.Body.Close()

				dec := c.classifier.OnUpstreamError(&http.Response{
					StatusCode: resp.StatusCode,
					Header:     resp.Header,
				}, respBody)

				switch dec.Class {
				case keypool.DecisionTransient:
					transientAttempts++
					if transientAttempts > maxTransientRetries {
						// Escalate to hard rotation.
						_ = c.pool.MarkExhausted(key.Token, dec.ResetDate, "transient_retries_exhausted")
						break // breaks inner loop, outer loop Acquire()s next key
					}
					c.pool.MarkTransient(key.Token, dec.RetryAfter, dec.Reason)
					// Honor Retry-After backoff before retrying same key.
					if dec.RetryAfter > 0 {
						select {
						case <-time.After(dec.RetryAfter):
						case <-ctx.Done():
							return nil, ctx.Err()
						}
					}
					continue // retry same key

				case keypool.DecisionHard, keypool.DecisionAmbiguous:
					_ = c.pool.MarkExhausted(key.Token, dec.ResetDate, dec.Reason)
					break // breaks inner loop, outer loop Acquire()s next key

				default:
					return resp, fmt.Errorf("unknown decision class: %v", dec.Class)
				}
				break // unreachable but quiet linter
			}

			// Other 4xx (400, 403, 404, etc.) — likely bad request shape, not key issue.
			// Return as-is for caller to surface to client.
			return resp, nil
		}
		// Inner loop broke (hard rotation) — fall through to outer loop's next Acquire().
	}
}

// ChatCompletion sends an OpenAI-format chat completion request with
// rotation. On paid-pool exhaustion, falls through to the free resolver.
// Returns the raw HTTP response on success OR ErrAllKeysExhausted when
// neither paid nor free succeeds (or when freeResolver is nil and paid
// is exhausted).
func (c *OpenCodeClient) ChatCompletion(
	ctx context.Context,
	modelID string,
	req *types.ChatCompletionRequest,
) (*http.Response, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}
	stream := req.Stream != nil && *req.Stream

	// Free-only mode (set by /free/v1/* routes): skip paid entirely.
	if isFreeOnlyContext(ctx) {
		if c.freeResolver == nil {
			return nil, errors.New("client: free-only mode requested but free resolver not configured")
		}
		return c.freeResolver.ResolveOpenAI(ctx, body, stream, "forced")
	}

	url := c.urlForModel(modelID)
	headers := map[string]string{}
	if stream {
		headers["Accept"] = "text/event-stream"
	}

	resp, err := c.doWithRotation(ctx, http.MethodPost, url,
		func() []byte { return body },
		headers,
	)
	if errors.Is(err, ErrAllKeysExhausted) && c.freeResolver != nil {
		return c.freeResolver.ResolveOpenAI(ctx, body, stream, "fallback")
	}
	return resp, err
}

// FreeOnlyChatCompletion bypasses the paid pool entirely and sends the
// request directly to the free resolver. Used by the /free/v1/* handler
// path (Phase 6 dual-endpoint routing) for paid-quota preservation.
func (c *OpenCodeClient) FreeOnlyChatCompletion(
	ctx context.Context,
	req *types.ChatCompletionRequest,
) (*http.Response, error) {
	if c.freeResolver == nil {
		return nil, errors.New("client: free resolver not configured")
	}
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}
	stream := req.Stream != nil && *req.Stream
	return c.freeResolver.ResolveOpenAI(ctx, body, stream, "forced")
}

// ChatCompletionNonStreaming sends a non-streaming request and returns
// the full parsed response. Forces stream=false.
func (c *OpenCodeClient) ChatCompletionNonStreaming(
	ctx context.Context,
	modelID string,
	req *types.ChatCompletionRequest,
) (*types.ChatCompletionResponse, error) {
	streamFalse := false
	req.Stream = &streamFalse

	resp, err := c.ChatCompletion(ctx, modelID, req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	// Non-200 → surface as Go error for backward compat.
	if resp.StatusCode >= http.StatusBadRequest {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("API error %d: %s", resp.StatusCode, string(bodyBytes))
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	var chatResp types.ChatCompletionResponse
	if err := json.Unmarshal(body, &chatResp); err != nil {
		return nil, fmt.Errorf("unmarshal response: %w", err)
	}
	return &chatResp, nil
}

// GetStreamingBody returns the response body for streaming consumption.
// Rotation completes before the first SSE byte; once 200 is returned,
// the caller owns the stream.
func (c *OpenCodeClient) GetStreamingBody(
	ctx context.Context,
	modelID string,
	req *types.ChatCompletionRequest,
) (io.ReadCloser, error) {
	streamTrue := true
	req.Stream = &streamTrue

	resp, err := c.ChatCompletion(ctx, modelID, req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= http.StatusBadRequest {
		bodyBytes, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		return nil, fmt.Errorf("API error %d: %s", resp.StatusCode, string(bodyBytes))
	}
	return resp.Body, nil
}

// SendAnthropicRequest sends a raw Anthropic-format request (for MiniMax models).
// Uses the same rotation path as ChatCompletion, and falls through to
// the free resolver's Anthropic endpoint on paid-pool exhaustion.
func (c *OpenCodeClient) SendAnthropicRequest(
	ctx context.Context,
	body []byte,
	stream bool,
) (*http.Response, error) {
	// Free-only mode: skip paid pool entirely.
	if isFreeOnlyContext(ctx) {
		if c.freeResolver == nil {
			return nil, errors.New("client: free-only mode requested but free resolver not configured")
		}
		return c.freeResolver.ResolveAnthropic(ctx, body, stream, "forced")
	}

	headers := map[string]string{}
	if stream {
		headers["Accept"] = "text/event-stream"
	}

	resp, err := c.doWithRotation(ctx, http.MethodPost, c.anthropicURL,
		func() []byte { return body },
		headers,
	)
	if errors.Is(err, ErrAllKeysExhausted) && c.freeResolver != nil {
		return c.freeResolver.ResolveAnthropic(ctx, body, stream, "fallback")
	}
	if err != nil {
		return nil, err
	}

	// Mimic legacy behavior: surface non-2xx as Go error for handler compat.
	// Rotation already handled 401/429 — anything still non-2xx here is 5xx
	// or other 4xx not triggering rotation.
	if resp.StatusCode >= http.StatusBadRequest {
		bodyBytes, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		return nil, fmt.Errorf("API error %d: %s", resp.StatusCode, string(bodyBytes))
	}
	return resp, nil
}

// FreeOnlySendAnthropic bypasses the paid pool and sends an Anthropic-format
// request directly to the free resolver. Used by the /free/v1/messages
// handler path (Phase 6).
func (c *OpenCodeClient) FreeOnlySendAnthropic(
	ctx context.Context,
	body []byte,
	stream bool,
) (*http.Response, error) {
	if c.freeResolver == nil {
		return nil, errors.New("client: free resolver not configured")
	}
	return c.freeResolver.ResolveAnthropic(ctx, body, stream, "forced")
}

// IsAllExhausted reports whether err signals that the paid pool is dry
// (handler should engage free-fallback).
func IsAllExhausted(err error) bool {
	return errors.Is(err, ErrAllKeysExhausted)
}
