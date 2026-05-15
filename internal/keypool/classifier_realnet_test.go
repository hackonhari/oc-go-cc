//go:build integration

package keypool

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
)

// TestClassifier_RealOpenCodeExhaustedKey hits live OpenCode Go upstream
// with a token known to be quota-exhausted and verifies the classifier
// returns DecisionHard with a credits-related reason.
//
// Run via:
//
//	OC_GO_CC_TEST_EXHAUSTED_TOKEN=sk-...your-exhausted-token... \
//	  go test -tags=integration ./internal/keypool/ -run RealOpenCode -v
//
// Skipped automatically when the env var is unset, so CI without the
// secret stays green.
func TestClassifier_RealOpenCodeExhaustedKey(t *testing.T) {
	token := os.Getenv("OC_GO_CC_TEST_EXHAUSTED_TOKEN")
	if token == "" {
		t.Skip("OC_GO_CC_TEST_EXHAUSTED_TOKEN not set; skipping real-network test")
	}

	body := map[string]any{
		"model":      "deepseek-v4-pro",
		"messages":   []map[string]string{{"role": "user", "content": "hi"}},
		"max_tokens": 3,
	}
	buf, _ := json.Marshal(body)

	req, err := http.NewRequest("POST",
		"https://opencode.ai/zen/go/v1/chat/completions",
		bytes.NewReader(buf))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("HTTP request: %v", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)

	t.Logf("upstream returned: status=%d body=%s", resp.StatusCode, string(respBody))

	// Sanity: token should genuinely be exhausted. If we get 200, the
	// token has recovered and this test cannot prove what we claim.
	if resp.StatusCode == 200 {
		t.Skip("token returned 200 — no longer exhausted, cannot prove classifier behavior")
	}

	c := NewClassifier()
	dec := c.OnUpstreamError(resp, respBody)

	if dec.Class != DecisionHard {
		t.Errorf("expected DecisionHard for exhausted token, got %v (reason=%q)", dec.Class, dec.Reason)
	}
	// Reason should mention credits or a quota-related label.
	if !strings.Contains(dec.Reason, "credits") && !strings.Contains(dec.Reason, "401") && !strings.Contains(dec.Reason, "quota") {
		t.Errorf("reason = %q, want substring 'credits' or 'quota' or '401'", dec.Reason)
	}
	if dec.ResetDate.IsZero() {
		t.Errorf("hard decision should set ResetDate")
	}
}
