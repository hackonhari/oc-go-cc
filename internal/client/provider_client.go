package client

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"

	"oc-go-cc/internal/config"
	"oc-go-cc/internal/keypool"
)

// ProviderClient sends requests to a single upstream provider.
// It handles both simple API-key forwarding and key-pool rotation
// (when EnableKeyPool is set on the provider config).
type ProviderClient struct {
	name                string
	baseURL             string
	protocol            string // "openai" or "anthropic"
	apiKey              string // simple forward key (when no key pool)
	pool                *keypool.KeyPool
	classifier          keypool.RotationSignal
	httpClient          *http.Client
	disableThinking     bool
	disableReasoningEffort bool
}

// NewProviderClient creates a provider client. When the provider has
// EnableKeyPool + APIKeys, a keypool is created for rotation. Otherwise
// the first API key is used as a simple static key.
func NewProviderClient(
	provider config.ProviderConfig,
	pool *keypool.KeyPool,
	classifier keypool.RotationSignal,
	httpClient *http.Client,
) *ProviderClient {
	pc := &ProviderClient{
		name:                provider.Name,
		baseURL:             provider.BaseURL,
		protocol:            provider.Protocol,
		pool:                pool,
		classifier:          classifier,
		httpClient:          httpClient,
		disableThinking:     provider.DisableThinking,
		disableReasoningEffort: provider.DisableReasoningEffort,
	}
	if !provider.EnableKeyPool && len(provider.APIKeys) > 0 {
		pc.apiKey = provider.APIKeys[0].Token
	}
	return pc
}

// Name returns the provider name.
func (pc *ProviderClient) Name() string { return pc.name }

// Protocol returns the provider protocol ("openai" or "anthropic").
func (pc *ProviderClient) Protocol() string { return pc.protocol }

// BaseURL returns the provider's base URL.
func (pc *ProviderClient) BaseURL() string { return pc.baseURL }

// HasKeyPool returns true when this provider uses key-pool rotation.
func (pc *ProviderClient) HasKeyPool() bool { return pc.pool != nil }

// DisableThinking returns true when thinking params should be stripped.
func (pc *ProviderClient) DisableThinking() bool { return pc.disableThinking }

// DisableReasoningEffort returns true when reasoning_effort should be stripped.
func (pc *ProviderClient) DisableReasoningEffort() bool { return pc.disableReasoningEffort }

// Do sends a request with simple API-key forwarding (no rotation).
func (pc *ProviderClient) Do(ctx context.Context, method, url string, body []byte, headers map[string]string) (*http.Response, error) {
	httpReq, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+pc.apiKey)
	for k, v := range headers {
		httpReq.Header.Set(k, v)
	}
	return pc.httpClient.Do(httpReq)
}

// DoWithRotation sends a request with key-pool rotation. Uses the same
// rotation loop as the current OpenCodeClient — acquire key, send, classify
// 401/429, retry or rotate. Returns ErrAllKeysExhausted when pool is dry.
func (pc *ProviderClient) DoWithRotation(
	ctx context.Context,
	method, url string,
	bodyBuilder func() []byte,
	headers map[string]string,
) (*http.Response, error) {
	return doRotation(ctx, method, url, bodyBuilder, headers, pc.pool, pc.classifier, pc.httpClient)
}

// GetStreamingBody returns a streaming response body.
// Uses simple forward when no key pool is configured, otherwise uses rotation.
func (pc *ProviderClient) GetStreamingBody(ctx context.Context, url string, body []byte) (io.ReadCloser, error) {
	headers := map[string]string{"Accept": "text/event-stream"}
	var resp *http.Response
	var err error
	if pc.HasKeyPool() {
		resp, err = pc.DoWithRotation(ctx, http.MethodPost, url,
			func() []byte { return body },
			headers,
		)
	} else {
		resp, err = pc.Do(ctx, http.MethodPost, url, body, headers)
	}
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

// Pool returns the key pool (nil when HasKeyPool is false).
func (pc *ProviderClient) Pool() *keypool.KeyPool { return pc.pool }
