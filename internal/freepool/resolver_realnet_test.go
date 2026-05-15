//go:build integration

package freepool

import (
	"context"
	"encoding/json"
	"io"
	"testing"
)

// TestResolveOpenAI_RealZenEndpoint hits the live Zen free-tier endpoint
// anonymously and asserts deepseek-v4-flash-free returns 200 with cost=0.
//
// Run via:
//
//	go test -tags=integration ./internal/freepool/ -run RealZenEndpoint -v
//
// No env vars needed — Zen free is anonymous.
func TestResolveOpenAI_RealZenEndpoint(t *testing.T) {
	r := New("https://opencode.ai/zen/v1",
		[]string{"deepseek-v4-flash-free", "qwen3.6-plus-free", "minimax-m2.5-free"},
		nil,
	)

	body := []byte(`{"model":"any","messages":[{"role":"user","content":"reply OK"}],"max_tokens":5}`)
	resp, err := r.ResolveOpenAI(context.Background(), body, false, "fallback")
	if err != nil {
		t.Fatalf("real free fallback failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		respBody, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d body = %s", resp.StatusCode, respBody)
	}

	respBody, _ := io.ReadAll(resp.Body)
	var parsed map[string]any
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		t.Fatalf("parse response: %v", err)
	}

	cost, _ := parsed["cost"].(string)
	if cost != "0" {
		t.Errorf("free model should have cost=0, got %q (body: %s)", cost, respBody)
	}
	t.Logf("real free model returned cost=%s id=%v", cost, parsed["id"])
}

// TestResolveAnthropic_RealZenEndpoint hits the live Anthropic-format
// free endpoint with minimax-m2.5-free.
func TestResolveAnthropic_RealZenEndpoint(t *testing.T) {
	r := New("https://opencode.ai/zen/v1",
		[]string{"minimax-m2.5-free"},
		nil,
	)

	body := []byte(`{"model":"x","max_tokens":5,"messages":[{"role":"user","content":"hi"}]}`)
	resp, err := r.ResolveAnthropic(context.Background(), body, false, "fallback")
	if err != nil {
		t.Fatalf("anthropic real free fallback failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		respBody, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d body = %s", resp.StatusCode, respBody)
	}

	respBody, _ := io.ReadAll(resp.Body)
	t.Logf("real anthropic free response: %s", respBody)
}
