package client

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"oc-go-cc/internal/config"
	"oc-go-cc/internal/keypool"
)

// UniversalClient routes requests to the correct ProviderClient based on
// provider name or model ID. It replaces the single-provider OpenCodeClient
// with a multi-provider dispatch layer.
type UniversalClient struct {
	providers   map[string]*ProviderClient
	modelToProv map[string]string // model_id → provider name
	httpClient  *http.Client
}

// NewUniversalClient creates a universal client from the provider registry.
// For each provider with EnableKeyPool, a dedicated keypool is initialized.
// The httpClient is shared across all providers.
func NewUniversalClient(
	providerConfigs map[string]config.ProviderConfig,
	pools map[string]*keypool.KeyPool,
	classifier keypool.RotationSignal,
	httpClient *http.Client,
) *UniversalClient {
	uc := &UniversalClient{
		providers:   make(map[string]*ProviderClient),
		modelToProv: make(map[string]string),
		httpClient:  httpClient,
	}

	for name, pc := range providerConfigs {
		pool := pools[name] // nil for simple-forward providers
		client := NewProviderClient(pc, pool, classifier, httpClient)
		uc.providers[name] = client

		for _, m := range pc.Models {
			uc.modelToProv[m] = name
		}
	}
	return uc
}

// Route returns the ProviderClient for a given provider name.
func (uc *UniversalClient) Route(providerName string) (*ProviderClient, error) {
	pc, ok := uc.providers[providerName]
	if !ok {
		return nil, fmt.Errorf("unknown provider: %q", providerName)
	}
	return pc, nil
}

// RouteByModel returns the ProviderClient that owns the given model ID.
func (uc *UniversalClient) RouteByModel(modelID string) (*ProviderClient, error) {
	name, ok := uc.modelToProv[modelID]
	if !ok {
		return nil, fmt.Errorf("no provider configured for model: %q", modelID)
	}
	return uc.Route(name)
}

// ProviderNames returns all configured provider names.
func (uc *UniversalClient) ProviderNames() []string {
	names := make([]string, 0, len(uc.providers))
	for n := range uc.providers {
		names = append(names, n)
	}
	return names
}

// HasProvider returns true if the named provider is configured.
func (uc *UniversalClient) HasProvider(name string) bool {
	_, ok := uc.providers[name]
	return ok
}

// doRotation is the shared key-pool rotation loop used by ProviderClient
// when EnableKeyPool is true. Acquire a key, send the request, classify
// 401/429 responses, retry same key (transient) or rotate (hard).
func doRotation(
	ctx context.Context,
	method, url string,
	bodyBuilder func() []byte,
	headers map[string]string,
	pool *keypool.KeyPool,
	classifier keypool.RotationSignal,
	httpClient *http.Client,
) (*http.Response, error) {
	for {
		key, err := pool.Acquire()
		if err != nil {
			if errors.Is(err, keypool.ErrAllExhausted) {
				return nil, ErrAllKeysExhausted
			}
			return nil, fmt.Errorf("acquire key: %w", err)
		}

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

			resp, err := httpClient.Do(httpReq)
			if err != nil {
				return nil, fmt.Errorf("request failed: %w", err)
			}

			if resp.StatusCode < 400 {
				return resp, nil
			}

			if resp.StatusCode >= 500 {
				return resp, nil
			}

			if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusTooManyRequests {
				respBody, _ := io.ReadAll(resp.Body)
				_ = resp.Body.Close()

				dec := classifier.OnUpstreamError(&http.Response{
					StatusCode: resp.StatusCode,
					Header:     resp.Header,
				}, respBody)

				switch dec.Class {
				case keypool.DecisionTransient:
					transientAttempts++
					if transientAttempts > maxTransientRetries {
						_ = pool.MarkExhausted(
							key.Token,
							time.Now().Add(transientEscalationTTL),
							"transient_retries_exhausted",
						)
						break
					}
					pool.MarkTransient(key.Token, dec.RetryAfter, dec.Reason)
					if dec.RetryAfter > 0 {
						select {
						case <-time.After(dec.RetryAfter):
						case <-ctx.Done():
							return nil, ctx.Err()
						}
					}
					continue

				case keypool.DecisionHard:
					_ = pool.MarkExhausted(key.Token, dec.ResetDate, dec.Reason)
					break

				default:
					return resp, fmt.Errorf("unknown decision class: %v", dec.Class)
				}
				break
			}

			// Other 4xx — not a key issue.
			return resp, nil
		}
	}
}
