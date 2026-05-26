// Package handlers contains HTTP request handlers for API endpoints.
package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"oc-go-cc/internal/client"
	"oc-go-cc/internal/config"
	"oc-go-cc/internal/metrics"
	"oc-go-cc/internal/middleware"
	"oc-go-cc/internal/router"
	"oc-go-cc/internal/token"
	"oc-go-cc/internal/transformer"
	"oc-go-cc/pkg/types"
)

// MessagesHandler handles /v1/messages requests.
type MessagesHandler struct {
	config              *config.Config
	client              *client.OpenCodeClient  // legacy client (backward compat)
	universalClient     *client.UniversalClient // multi-provider dispatch (new)
	modelRouter         *router.ModelRouter
	fallbackHandler     *router.FallbackHandler
	requestTransformer  *transformer.RequestTransformer
	responseTransformer *transformer.ResponseTransformer
	streamHandler       *transformer.StreamHandler
	tokenCounter        *token.Counter
	logger              *slog.Logger
	rateLimiter         *middleware.RateLimiter
	requestIDGen        *middleware.RequestIDGenerator
	metrics             *metrics.Metrics
}

// responseWriter wraps http.ResponseWriter to track if headers were written
// and to serialize concurrent writes (http.ResponseWriter is not safe for
// concurrent use, but handleStreaming runs a heartbeat goroutine alongside
// the SSE writer — without the mutex, interleaved bytes corrupt the stream).
type responseWriter struct {
	http.ResponseWriter
	wroteHeader bool
	mu          sync.Mutex
}

func (w *responseWriter) WriteHeader(code int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.wroteHeader {
		w.wroteHeader = true
		w.ResponseWriter.WriteHeader(code)
	}
}

func (w *responseWriter) Write(b []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.wroteHeader {
		w.wroteHeader = true
		w.ResponseWriter.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(b)
}

// Flush implements http.Flusher for SSE streaming support.
func (w *responseWriter) Flush() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// streamHeartbeat sends SSE keepalive comments every 3 seconds until done
// is closed OR ctx is cancelled. Used by handleStreaming to prevent
// Claude Code's 6-second client-side timeout while we wait for upstream
// data to arrive.
//
// LIFECYCLE CONTRACT — required of all callers:
//
//   - The caller MUST wait for this method to return before tearing down rw.
//     Otherwise a tick can fire after the HTTP server has invalidated the
//     response writer, and rw.Flush() panics on a nil bufio.Writer.
//
//   - Recommended pattern (used by handleStreaming):
//     done := make(chan struct{})
//     var wg sync.WaitGroup
//     wg.Add(1)
//     go func() { defer wg.Done(); h.streamHeartbeat(ctx, rw, done) }()
//     defer func() { close(done); wg.Wait() }()
//
// The defer recover() is defense-in-depth: any future code change that
// introduces a new race against rw will be caught here rather than
// killing the entire proxy process. Without recover(), one panicking
// request goroutine takes down every in-flight request AND the listener.
func (h *MessagesHandler) streamHeartbeat(ctx context.Context, rw *responseWriter, done <-chan struct{}) {
	defer func() {
		if r := recover(); r != nil {
			h.logger.Error("heartbeat goroutine panic recovered",
				"panic", r,
				"stack", string(debug.Stack()))
		}
	}()

	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			// Send SSE comment (ignored by client but keeps connection alive).
			// Must write through rw (not the raw w) so the mutex serializes
			// against concurrent SSE event writes — otherwise bytes interleave
			// and Claude Code sees InvalidHTTPResponse.
			_, _ = fmt.Fprintf(rw, ":keepalive\n\n")
			rw.Flush()
		case <-done:
			return
		case <-ctx.Done():
			return
		}
	}
}

// NewMessagesHandler creates a new messages handler.
func NewMessagesHandler(
	cfg *config.Config,
	openCodeClient *client.OpenCodeClient,
	universalClient *client.UniversalClient,
	modelRouter *router.ModelRouter,
	fallbackHandler *router.FallbackHandler,
	tokenCounter *token.Counter,
	metrics *metrics.Metrics,
) *MessagesHandler {
	return &MessagesHandler{
		config:              cfg,
		client:              openCodeClient,
		universalClient:     universalClient,
		modelRouter:         modelRouter,
		fallbackHandler:     fallbackHandler,
		requestTransformer:  transformer.NewRequestTransformer(),
		responseTransformer: transformer.NewResponseTransformer(),
		streamHandler:       transformer.NewStreamHandler(),
		tokenCounter:        tokenCounter,
		logger:              slog.Default(),
		rateLimiter:         middleware.NewRateLimiter(100, time.Minute),
		requestIDGen:        middleware.NewRequestIDGenerator(),
		metrics:             metrics,
	}
}

// resolveProviderClient returns the ProviderClient for a model if multi-provider
// is configured. Returns nil when legacy OpenCodeClient should be used.
//
// Resolution order:
//  1. Provider from URL path (set by /<provider>/v1/messages routing) — the
//     authoritative source. When present, routes directly to that provider.
//  2. Exact model match in any provider's model list (e.g. "deepseek-v4-pro" →
//     opencode, "deepseek/deepseek-v4-pro" → commandcode)
//  3. Provider-prefix "provider/model" routing (fallback for bare /v1/messages)
//  4. Returns nil when no provider is configured for this model
func (h *MessagesHandler) resolveProviderClient(r *http.Request, modelID string) (*client.ProviderClient, string) {
	if h.universalClient == nil {
		return nil, modelID
	}
	// Step 1: provider from URL path — the definitive routing signal.
	if provName := middleware.ProviderFromContext(r.Context()); provName != "" {
		pc, err := h.universalClient.Route(provName)
		if err == nil {
			return pc, modelID
		}
	}
	// Step 2: exact model match in provider model lists.
	pc, err := h.universalClient.RouteByModel(modelID)
	if err == nil {
		return pc, modelID
	}
	// Step 3: provider-prefix routing — "provider/model" format.
	if i := strings.Index(modelID, "/"); i > 0 {
		provName := modelID[:i]
		cleanModel := modelID[i+1:]
		if h.universalClient.HasProvider(provName) {
			pc, err := h.universalClient.Route(provName)
			if err == nil {
				return pc, cleanModel
			}
		}
	}
	return nil, modelID
}

// HandleMessages handles POST /v1/messages.
func (h *MessagesHandler) HandleMessages(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Generate or get request ID for correlation
	requestID := r.Header.Get("X-Request-ID")
	if requestID == "" {
		requestID = h.requestIDGen.Generate()
	}
	w.Header().Set("X-Request-ID", requestID)

	// Rate limiting
	clientIP := middleware.GetClientIP(r)
	if !h.rateLimiter.Allow(clientIP) {
		h.metrics.RecordRateLimited()
		h.logger.Warn("rate limited", "client", clientIP, "request_id", requestID)
		http.Error(w, "rate limited", http.StatusTooManyRequests)
		return
	}

	// Read the raw request body for debug logging
	var rawBody json.RawMessage
	if err := json.NewDecoder(r.Body).Decode(&rawBody); err != nil {
		h.sendError(w, http.StatusBadRequest, "invalid request body", err)
		return
	}

	// Parse into Anthropic request
	var anthropicReq types.MessageRequest
	if err := json.Unmarshal(rawBody, &anthropicReq); err != nil {
		h.sendError(w, http.StatusBadRequest, "invalid request body", err)
		return
	}

	// Strip Claude Code's [1m] extended-context suffix from the model name.
	// The router already does this internally, but the raw model name is also used
	// for provider resolution (modelToProv lookup) and upstream body replacement,
	// both of which need the clean model name.
	if i := strings.LastIndex(strings.ToLower(anthropicReq.Model), "[1m]"); i >= 0 && i+4 == len(anthropicReq.Model) {
		anthropicReq.Model = anthropicReq.Model[:i]
	}

	// Validate request
	if err := anthropicReq.Validate(); err != nil {
		h.sendError(w, http.StatusBadRequest, err.Error(), nil)
		return
	}

	// Record metrics
	isStreaming := anthropicReq.Stream != nil && *anthropicReq.Stream
	h.metrics.RecordRequest(isStreaming)

	h.logger.Info("received request",
		"model", anthropicReq.Model,
		"streaming", isStreaming,
		"messages", len(anthropicReq.Messages),
		"tools", len(anthropicReq.Tools),
		"max_tokens", anthropicReq.MaxTokens,
	)

	// Build message content for routing and token counting.
	var routerMessages []router.MessageContent
	var tokenMessages []token.MessageContent
	systemText := anthropicReq.SystemText()

	for _, msg := range anthropicReq.Messages {
		blocks := msg.ContentBlocks()
		content := extractTextFromBlocks(blocks)
		mc := router.MessageContent{
			Role:    msg.Role,
			Content: content,
		}
		routerMessages = append(routerMessages, mc)
		tokenMessages = append(tokenMessages, token.MessageContent{
			Role:    msg.Role,
			Content: content,
		})
	}

	// Count tokens.
	tokenCount, err := h.tokenCounter.CountMessages(systemText, tokenMessages)
	if err != nil {
		h.logger.Warn("failed to count tokens", "error", err)
		tokenCount = 0
	}

	// Route to appropriate model.
	// Pass the requested model name so the router can honor explicit selection
	// (via /model command, ANTHROPIC_MODEL env var, or claude-* model names).
	// For streaming, use faster models to minimize TTFT when no explicit model is set.
	var routeResult router.RouteResult
	if isStreaming {
		routeResult = h.modelRouter.RouteForStreaming(routerMessages, tokenCount, anthropicReq.Model)
	} else {
		var err error
		routeResult, err = h.modelRouter.Route(routerMessages, tokenCount, anthropicReq.Model)
		if err != nil {
			h.sendError(w, http.StatusInternalServerError, "routing failed", err)
			return
		}
	}

	h.logger.Info("routing request",
		"requested_model", anthropicReq.Model,
		"scenario", routeResult.Scenario,
		"resolved_model", routeResult.Primary.ModelID,
		"tokens", tokenCount,
	)

	// Build fallback chain.
	modelChain := routeResult.GetModelChain()

	if isStreaming {
		h.handleStreaming(w, r, &anthropicReq, modelChain, rawBody, anthropicReq.Model)
	} else {
		h.handleNonStreaming(w, r, &anthropicReq, modelChain, rawBody, anthropicReq.Model)
	}
}

// handleStreaming handles a streaming request with real-time SSE proxying.
// originalModel is the model name from the request (before router resolution),
// used for multi-provider dispatch to ensure the correct provider is selected
// regardless of the model router's fallback chain.
func (h *MessagesHandler) handleStreaming(
	w http.ResponseWriter,
	r *http.Request,
	anthropicReq *types.MessageRequest,
	modelChain []config.ModelConfig,
	rawBody json.RawMessage,
	originalModel string,
) {
	// Each fallback attempt needs its own context with timeout.
	// Don't share r.Context() across fallbacks - when Claude Code retries,
	// the original context gets canceled and kills all fallbacks.
	clientCtx := r.Context()

	rw := &responseWriter{ResponseWriter: w}

	// Set SSE headers immediately so Claude Code knows the stream is alive.
	// This prevents client-side timeouts before we even start sending data.
	rw.Header().Set("Content-Type", "text/event-stream")
	rw.Header().Set("Cache-Control", "no-cache")
	rw.Header().Set("Connection", "keep-alive")
	rw.Header().Set("X-Accel-Buffering", "no")
	rw.WriteHeader(http.StatusOK)
	rw.Flush()

	// Start heartbeat to keep connection alive while waiting for upstream.
	// Claude Code times out after ~6 seconds of no data, so we send pings every 3 seconds
	// (frequent enough to prevent timeout, not so frequent as to cause overhead).
	//
	// Lifecycle (fixed 2026-05-20): the goroutine MUST exit before this
	// handler returns. Otherwise a tick can fire AFTER the HTTP server
	// has torn down the underlying response writer, and rw.Flush() panics
	// on a nil bufio.Writer. The WaitGroup makes the deferred cleanup
	// block until the goroutine has actually exited — not merely been
	// signaled to exit. See streamHeartbeat docstring for the lifecycle
	// contract.
	heartbeatDone := make(chan struct{})
	var heartbeatWG sync.WaitGroup
	heartbeatWG.Add(1)
	go func() {
		defer heartbeatWG.Done()
		h.streamHeartbeat(clientCtx, rw, heartbeatDone)
	}()
	// Stop heartbeat AND wait for it to fully exit before handler returns.
	// Order matters: close signals the goroutine, Wait blocks until it has
	// actually exited the select loop. Without Wait, the goroutine could
	// still be mid-flush when the response writer is torn down.
	defer func() {
		close(heartbeatDone)
		heartbeatWG.Wait()
	}()

	streamStart := time.Now()

	for _, model := range modelChain {
		// Check if client already disconnected before trying this model
		select {
		case <-clientCtx.Done():
			h.logger.Info("client disconnected, stopping streaming fallbacks")
			return
		default:
		}

		h.logger.Info("attempting streaming model", "model", model.ModelID)

		// Create a fresh context for THIS attempt only — no timeout.
		// Streaming requests can run many minutes (DeepSeek V4 Pro reasoning).
		// Transport-level timeouts handle connection hangs; clientCtx handles disconnects.
		ctx, cancel := context.WithCancel(context.Background())

		// Resolve provider client using the ORIGINAL requested model name.
		// The model router may have resolved to a different model (fallback),
		// but provider routing needs the original name to find the correct
		// upstream.
		pc, cleanModel := h.resolveProviderClient(r, originalModel)
		if pc != nil {
			originalModel = cleanModel
		}

		// Check if this is an Anthropic-native model (MiniMax, native endpoint providers).
		// Use originalModel for provider dispatch, but keep model.ModelID for
		// the anthropic-native path (MiniMax still uses its own model IDs).
		if client.IsAnthropicModel(model.ModelID) || (pc != nil && effectiveProtocol(pc, originalModel) == "anthropic") {
			// Use the original model name when routing via multi-provider,
			// otherwise use the model chain's model ID (legacy path).
			effectiveModel := model.ModelID
			if pc != nil {
				effectiveModel = originalModel
			}
			modelBody := replaceModelInRawBody(rawBody, effectiveModel)
			if err := h.handleAnthropicStreaming(ctx, rw, modelBody, model.ModelID, pc); err != nil {
				cancel()
				// Check if this was a client disconnect
				if clientCtx.Err() == context.Canceled {
					h.logger.Info("client disconnected during anthropic stream")
					return
				}
				h.logger.Warn("anthropic streaming failed", "model", model.ModelID, "error", err)
				continue
			}
			cancel()
			latency := time.Since(streamStart)
			h.metrics.RecordSuccess(model.ModelID, latency)
			h.logger.Info("streaming completed", "model", model.ModelID, "latency", latency)
			return
		}

		// For OpenAI-compatible models, apply per-provider flags then transform.
		// Both executeViaProvider (non-streaming) and this path must agree on
		// which fields to strip — disable_thinking and disable_reasoning_effort.
		effectiveModel := model
		if pc != nil {
			if pc.DisableThinking() {
				effectiveModel.Thinking = nil
			}
			if pc.DisableReasoningEffort() {
				effectiveModel.ReasoningEffort = ""
			}
		}
		openaiReq, err := h.requestTransformer.TransformRequest(anthropicReq, effectiveModel)
		if err != nil {
			cancel()
			h.logger.Warn("request transform failed", "model", model.ModelID, "error", err)
			continue
		}

		// Get streaming body — use ProviderClient if available, else legacy client
		streamBody, err := h.getStreamingBody(ctx, pc, model.ModelID, openaiReq)
		if err != nil {
			cancel()
			// Check if this was a client disconnect (context canceled)
			if clientCtx.Err() == context.Canceled {
				h.logger.Info("client disconnected during upstream request")
				return
			}
			h.logger.Warn("streaming request failed", "model", model.ModelID, "error", err)
			continue
		}

		// Proxy the stream: transform OpenAI SSE → Anthropic SSE in real-time
		if err := h.streamHandler.ProxyStream(rw, streamBody, model.ModelID, clientCtx); err != nil {
			_ = streamBody.Close()
			cancel()
			if err == transformer.ErrClientDisconnected {
				h.logger.Info("client disconnected during stream")
				return
			}
			// Check if this was a client disconnect
			if clientCtx.Err() == context.Canceled {
				h.logger.Info("client disconnected during stream (context canceled)")
				return
			}
			h.logger.Warn("stream proxy failed", "model", model.ModelID, "error", err)
			continue
		}

		_ = streamBody.Close()
		cancel()
		latency := time.Since(streamStart)
		h.metrics.RecordSuccess(model.ModelID, latency)
		h.logger.Info("streaming completed", "model", model.ModelID, "latency", latency)
		return
	}

	// All models failed
	h.metrics.RecordFailure()
	if !rw.wroteHeader {
		h.sendError(w, http.StatusBadGateway, "all streaming models failed", nil)
	} else {
		// Headers already sent - send error as SSE event
		h.sendStreamError(rw, "all upstream models failed")
	}
}

// replaceModelInRawBody replaces the model field in raw JSON body with the actual model ID.
// This is needed for Anthropic endpoint which validates the model name.
func replaceModelInRawBody(rawBody json.RawMessage, modelID string) json.RawMessage {
	// Simple string replacement - find "model":"..." and replace with "model":"actual-model"
	bodyStr := string(rawBody)

	// Try to find and replace the model field
	// Pattern: "model":"claude-..." or "model":"any-model-name"
	if idx := strings.Index(bodyStr, `"model":"`); idx != -1 {
		start := idx + len(`"model":"`)
		if end := strings.Index(bodyStr[start:], `"`); end != -1 {
			oldModel := bodyStr[start : start+end]
			// Replace the model value
			newBody := bodyStr[:start] + modelID + bodyStr[start+end:]
			slog.Debug("replaced model in request body",
				"old_model", oldModel,
				"new_model", modelID,
				"success", true)
			return json.RawMessage(newBody)
		}
	}

	slog.Warn("could not find model field in request body, using original",
		"body_preview", bodyStr[:min(len(bodyStr), 200)])
	// If we couldn't parse, return original (will likely fail upstream but that's ok)
	return rawBody
}

// getStreamingBody returns a streaming response body from either the
// ProviderClient (multi-provider path) or the legacy OpenCodeClient.
func (h *MessagesHandler) getStreamingBody(
	ctx context.Context,
	pc *client.ProviderClient,
	modelID string,
	req *types.ChatCompletionRequest,
) (io.ReadCloser, error) {
	if pc != nil {
		body, err := json.Marshal(req)
		if err != nil {
			return nil, fmt.Errorf("marshal request: %w", err)
		}
		return pc.GetStreamingBody(ctx, pc.BaseURL()+"/chat/completions", body)
	}
	return h.client.GetStreamingBody(ctx, modelID, req)
}

// handleAnthropicStreaming sends a raw Anthropic request to the Anthropic endpoint.
// When pc is non-nil (multi-provider path), uses the ProviderClient with its
// configured AnthropicBaseURL; otherwise falls back to the legacy client.
func (h *MessagesHandler) handleAnthropicStreaming(
	ctx context.Context,
	w http.ResponseWriter,
	rawBody json.RawMessage,
	modelID string,
	pc *client.ProviderClient,
) error {
	// Debug: Log what we're sending
	h.logger.Debug("sending anthropic streaming request",
		"model_id", modelID,
		"body_preview", string(rawBody)[:min(len(rawBody), 200)])

	// Use ProviderClient for multi-provider path.
	if pc != nil {
		url := pc.BaseURL() + "/messages"
		resp, err := pc.Do(ctx, http.MethodPost, url, rawBody, nil)
		if err != nil {
			return err
		}
		defer func() { _ = resp.Body.Close() }()
		_, err = io.Copy(w, resp.Body)
		if err != nil {
			if ctx.Err() == context.Canceled {
				return transformer.ErrClientDisconnected
			}
			return fmt.Errorf("failed to copy response: %w", err)
		}
		return nil
	}

	// Send raw Anthropic request to Anthropic endpoint via legacy client
	// Use ctx so cancellation propagates when client disconnects
	resp, err := h.client.SendAnthropicRequest(ctx, rawBody, true)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	// Copy the response directly (already in Anthropic format)
	// SSE headers already set by handleStreaming
	// Use io.Copy which handles streaming efficiently
	_, err = io.Copy(w, resp.Body)
	if err != nil {
		// Check if this was a client disconnect
		if ctx.Err() == context.Canceled {
			return transformer.ErrClientDisconnected
		}
		return fmt.Errorf("failed to copy response: %w", err)
	}

	return nil
}

// sendStreamError sends an error event in the SSE stream.
// Use this when headers have already been written.
func (h *MessagesHandler) sendStreamError(w http.ResponseWriter, message string) {
	h.logger.Error("sending stream error", "message", message)

	errorEvent := map[string]interface{}{
		"type": "error",
		"error": map[string]interface{}{
			"type":    "api_error",
			"message": message,
		},
	}

	data, _ := json.Marshal(errorEvent)
	_, _ = fmt.Fprintf(w, "event: error\ndata: %s\n\n", string(data))

	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

// handleNonStreaming handles a non-streaming request with fallback.
func (h *MessagesHandler) handleNonStreaming(
	w http.ResponseWriter,
	r *http.Request,
	anthropicReq *types.MessageRequest,
	modelChain []config.ModelConfig,
	rawBody json.RawMessage,
	originalModel string,
) {
	ctx := r.Context()
	startTime := time.Now()

	h.logger.Info("routing via provider", "requested_model", originalModel, "resolved_chain", len(modelChain))

	// Resolve provider client from the ORIGINAL model name (not the router-resolved
	// model which may have fallen back to default). Once resolved, the same
	// provider is used for all fallback attempts in the chain.
	// Supports provider-prefix routing: "provider/model" selects provider,
	// and cleanModel strips the prefix for upstream use.
	pc, cleanModel := h.resolveProviderClient(r, originalModel)
	if pc != nil {
		originalModel = cleanModel
		h.logger.Info("multi-provider dispatch", "provider", pc.Name(), "protocol", pc.Protocol())
	}

	// Build effective chain: when multi-provider is active, use only the
	// first model (primary). The fallback chain contains opencode models
	// that shouldn't be tried against other providers.
	effectiveChain := modelChain
	if pc != nil {
		effectiveChain = modelChain[:1]
	}

	result, responseBody, err := h.fallbackHandler.ExecuteWithFallback(
		ctx,
		effectiveChain,
		func(ctx context.Context, model config.ModelConfig) ([]byte, error) {
			// When multi-provider is active, use the resolved provider client
			// with the ORIGINAL model name (preserved from the user request).
			if pc != nil {
				return h.executeViaProvider(ctx, anthropicReq, rawBody, model, pc, originalModel)
			}
			// Legacy path: use model chain's model ID directly
			if client.IsAnthropicModel(model.ModelID) {
				return h.executeAnthropicRequest(ctx, rawBody, model)
			}
			return h.executeOpenAIRequest(ctx, anthropicReq, model)
		},
	)

	if err != nil {
		h.metrics.RecordFailure()
		h.sendError(w, http.StatusBadGateway, "all models failed", err)
		return
	}

	latency := time.Since(startTime)
	h.metrics.RecordSuccess(result.ModelID, latency)

	h.logger.Info("request completed",
		"model", result.ModelID,
		"attempts", result.Attempted,
		"latency", latency,
	)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(responseBody)
}

// effectiveProtocol returns the protocol to use for a model.
// Some providers (commandcode) serve both Anthropic-native models (Claude)
// and OpenAI models (DeepSeek, Qwen etc.) through different endpoints.
// Claude models must use the Anthropic path regardless of provider config.
func effectiveProtocol(pc *client.ProviderClient, modelID string) string {
	if strings.HasPrefix(modelID, "claude-") {
		return "anthropic"
	}
	return pc.Protocol()
}

// executeViaProvider sends a request through the ProviderClient (multi-provider path).
// Uses originalModel (preserved from the user request) as the model ID sent upstream,
// while using model's config for temperature/max_tokens/thinking parameters.
func (h *MessagesHandler) executeViaProvider(
	ctx context.Context,
	anthropicReq *types.MessageRequest,
	rawBody json.RawMessage,
	model config.ModelConfig,
	pc *client.ProviderClient,
	originalModel string,
) ([]byte, error) {
	if effectiveProtocol(pc, originalModel) == "anthropic" {
		// Anthropic-native: pass through with model name replaced
		modifiedBody := replaceModelInRawBody(rawBody, originalModel)
		return h.sendAnthropicViaProvider(ctx, modifiedBody, originalModel, pc)
	}
	// OpenAI-format: translate Anthropic→OpenAI, use originalModel as upstream model ID.
	// Build a model config that inherits temperature/max_tokens/thinking from the
	// router-resolved model but uses the original model name for upstream routing.
	effectiveModel := model
	effectiveModel.ModelID = originalModel
		// Per-provider transformer flags: each flag targets only its namesake field.
		// disable_thinking strips the Anthropic-format "thinking" field.
		// disable_reasoning_effort strips the OpenAI-standard "reasoning_effort" field.
		// They are independent: e.g. Google keeps reasoning_effort but strips thinking.
		if pc.DisableThinking() {
			effectiveModel.Thinking = nil
		}
		if pc.DisableReasoningEffort() {
			effectiveModel.ReasoningEffort = ""
		}
		reqCopy := *anthropicReq
		reqCopy.Model = originalModel
		openaiReq, err := h.requestTransformer.TransformRequest(&reqCopy, effectiveModel)
		// Post-transform: catch defaults the transformer may have added.
		if pc.DisableThinking() {
			openaiReq.Thinking = nil
		}
		if pc.DisableReasoningEffort() {
			openaiReq.ReasoningEffort = nil
		}
	if err != nil {
		return nil, fmt.Errorf("request transform failed: %w", err)
	}
	body, err := json.Marshal(openaiReq)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}
	// Use key-pool rotation when the provider has one (opencode),
	// otherwise use simple API-key forwarding.
	var resp *http.Response
	if pc.HasKeyPool() {
		resp, err = pc.DoWithRotation(ctx, http.MethodPost, pc.BaseURL()+"/chat/completions",
			func() []byte { return body }, nil)
	} else {
		resp, err = pc.Do(ctx, http.MethodPost, pc.BaseURL()+"/chat/completions", body, nil)
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= http.StatusBadRequest {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("API error %d: %s", resp.StatusCode, string(bodyBytes))
	}
	respBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	var openaiResp types.ChatCompletionResponse
	if err := json.Unmarshal(respBytes, &openaiResp); err != nil {
		return nil, fmt.Errorf("unmarshal response: %w", err)
	}
	anthropicResp, err := h.responseTransformer.TransformResponse(&openaiResp, originalModel)
	if err != nil {
		return nil, fmt.Errorf("transform response: %w", err)
	}
	return json.Marshal(anthropicResp)
}

// sendAnthropicViaProvider sends a raw Anthropic request through a ProviderClient.
func (h *MessagesHandler) sendAnthropicViaProvider(
	ctx context.Context,
	rawBody json.RawMessage,
	modelID string,
	pc *client.ProviderClient,
) ([]byte, error) {
	resp, err := pc.Do(ctx, http.MethodPost, pc.BaseURL()+"/messages", rawBody, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= http.StatusBadRequest {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("API error %d: %s", resp.StatusCode, string(bodyBytes))
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	return body, nil
}

// executeAnthropicRequest executes a request to the Anthropic endpoint (for MiniMax models).
func (h *MessagesHandler) executeAnthropicRequest(
	ctx context.Context,
	rawBody json.RawMessage,
	model config.ModelConfig,
) ([]byte, error) {
	// Send raw Anthropic request to Anthropic endpoint
	resp, err := h.client.SendAnthropicRequest(ctx, rawBody, false)
	if err != nil {
		return nil, fmt.Errorf("anthropic request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	// Read the response (already in Anthropic format)
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response: %w", err)
	}

	h.logger.Debug("anthropic response", "body", string(body))

	return body, nil
}

// executeOpenAIRequest executes a request to the OpenAI endpoint with transformation.
func (h *MessagesHandler) executeOpenAIRequest(
	ctx context.Context,
	anthropicReq *types.MessageRequest,
	model config.ModelConfig,
) ([]byte, error) {
	// Transform request to OpenAI format.
	openaiReq, err := h.requestTransformer.TransformRequest(anthropicReq, model)
	if err != nil {
		return nil, fmt.Errorf("request transform failed: %w", err)
	}

	// Handle non-streaming.
	resp, err := h.client.ChatCompletionNonStreaming(ctx, model.ModelID, openaiReq)
	if err != nil {
		return nil, fmt.Errorf("chat completion failed: %w", err)
	}

	// Transform response to Anthropic format.
	anthropicResp, err := h.responseTransformer.TransformResponse(resp, model.ModelID)
	if err != nil {
		return nil, fmt.Errorf("response transform failed: %w", err)
	}

	return json.Marshal(anthropicResp)
}

// extractTextFromBlocks extracts plain text from Anthropic content blocks.
func extractTextFromBlocks(blocks []types.ContentBlock) string {
	var content string
	for _, block := range blocks {
		switch block.Type {
		case "text":
			content += block.Text
		case "tool_use":
			content += fmt.Sprintf("[Tool Use: %s]", block.Name)
		case "tool_result":
			content += block.TextContent()
		case "thinking":
			// Skip thinking blocks for text extraction
		case "image":
			content += "[Image]"
		}
	}
	return content
}

// sendError sends an error response in Anthropic format.
// Safe to call multiple times - subsequent calls are no-ops.
func (h *MessagesHandler) sendError(w http.ResponseWriter, statusCode int, message string, err error) {
	h.logger.Error("request error",
		"status", statusCode,
		"message", message,
		"error", err,
	)

	// Use the wrapped writer if available to prevent duplicate WriteHeader calls
	if rw, ok := w.(*responseWriter); ok && rw.wroteHeader {
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)

	errorResp := transformer.TransformErrorResponse(statusCode, message)
	_ = json.NewEncoder(w).Encode(errorResp)
}
