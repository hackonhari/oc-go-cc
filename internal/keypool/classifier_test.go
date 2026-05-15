package keypool

import (
	"net/http"
	"strconv"
	"testing"
	"time"
)

// mkResp builds an *http.Response for table-driven tests without going
// through a real HTTP server.
func mkResp(status int, headers map[string]string) *http.Response {
	h := http.Header{}
	for k, v := range headers {
		h.Set(k, v)
	}
	return &http.Response{
		StatusCode: status,
		Header:     h,
	}
}

func TestClassifier_TableDriven(t *testing.T) {
	c := NewClassifier()

	// realCreditsErrorBody is the verbatim body captured from a live
	// exhausted ocgo-hk key against opencode.ai/zen/go/v1 on 2026-05-15.
	const realCreditsErrorBody = `{"type":"error","error":{"type":"CreditsError","message":"Insufficient balance. Manage your billing here: https://opencode.ai/workspace/wrk_01KQY6Q9P5XTZBDB44T00ZDPKX/billing"}}`

	cases := []struct {
		name      string
		status    int
		headers   map[string]string
		body      string
		wantClass DecisionClass
		// wantReason is a substring assertion against decision.Reason.
		wantReason string
		// wantRetryAfter only checked for Transient class
		wantRetryAfter time.Duration
	}{
		{
			name:       "real OpenCode 401 CreditsError (monthly cap)",
			status:     401,
			body:       realCreditsErrorBody,
			wantClass:  DecisionHard,
			wantReason: "credits_exhausted",
		},
		{
			name:       "401 with no body still classified hard",
			status:     401,
			body:       "",
			wantClass:  DecisionHard,
			wantReason: "401_credits_or_auth",
		},
		{
			name:       "429 with monthly_limit body, no header",
			status:     429,
			body:       `{"error":"monthly_limit_exceeded"}`,
			wantClass:  DecisionHard,
			wantReason: "monthly_cap",
		},
		{
			name:       "429 with weekly_limit body",
			status:     429,
			body:       `{"error":"weekly_limit_exceeded"}`,
			wantClass:  DecisionHard,
			wantReason: "weekly_cap",
		},
		{
			name:           "429 with Retry-After: 5, no quota body → transient",
			status:         429,
			headers:        map[string]string{"Retry-After": "5"},
			body:           `{"error":"rate_limited_burst"}`,
			wantClass:      DecisionTransient,
			wantReason:     "429_transient_throttle",
			wantRetryAfter: 5 * time.Second,
		},
		{
			name:    "429 with Retry-After: 3600 (long) → hard",
			status:  429,
			headers: map[string]string{"Retry-After": "3600"},
			body:    `{"error":"slow_down"}`,
			// 3600 > 60s threshold AND no quota keyword in body → hard via long-retry path
			wantClass:  DecisionHard,
			wantReason: "429_retry_after_long",
		},
		{
			name:           "429 with Retry-After: 60 (exactly threshold) → transient",
			status:         429,
			headers:        map[string]string{"Retry-After": "60"},
			body:           "",
			wantClass:      DecisionTransient,
			wantReason:     "429_transient_throttle",
			wantRetryAfter: 60 * time.Second,
		},
		{
			name:       "429 with no header, no body → ambiguous → hard",
			status:     429,
			body:       "",
			wantClass:  DecisionAmbiguous,
			wantReason: "429_ambiguous_default_hard",
		},
		{
			name:       "429 with Retry-After + quota body — body wins (hard)",
			status:     429,
			headers:    map[string]string{"Retry-After": "10"},
			body:       `{"error":"quota exceeded"}`,
			wantClass:  DecisionHard,
			wantReason: "429_quota",
		},
		{
			name:       "429 with malformed Retry-After (non-numeric) → treated as no header",
			status:     429,
			headers:    map[string]string{"Retry-After": "nonsense"},
			body:       "",
			wantClass:  DecisionAmbiguous,
			wantReason: "429_ambiguous_default_hard",
		},
		{
			name:       "429 with negative Retry-After → treated as no header",
			status:     429,
			headers:    map[string]string{"Retry-After": "-5"},
			body:       "",
			wantClass:  DecisionAmbiguous,
			wantReason: "429_ambiguous_default_hard",
		},
		{
			name:       "401 with quota in body (hypothetical) — still hard",
			status:     401,
			body:       `{"error":"quota"}`,
			wantClass:  DecisionHard,
			wantReason: "401_credits_or_auth",
		},
		{
			name:       "unexpected 500 routed to classifier → safety hard",
			status:     500,
			body:       "internal error",
			wantClass:  DecisionHard,
			wantReason: "unexpected_status_500",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := mkResp(tc.status, tc.headers)
			dec := c.OnUpstreamError(resp, []byte(tc.body))

			if dec.Class != tc.wantClass {
				t.Errorf("class = %v, want %v", dec.Class, tc.wantClass)
			}
			if tc.wantReason != "" && !contains(dec.Reason, tc.wantReason) {
				t.Errorf("reason = %q, want substring %q", dec.Reason, tc.wantReason)
			}
			if tc.wantClass == DecisionTransient {
				if dec.RetryAfter != tc.wantRetryAfter {
					t.Errorf("retryAfter = %v, want %v", dec.RetryAfter, tc.wantRetryAfter)
				}
			}
			if tc.wantClass == DecisionHard || tc.wantClass == DecisionAmbiguous {
				if dec.ResetDate.IsZero() {
					t.Errorf("hard/ambiguous decision should set ResetDate")
				}
			}
		})
	}
}

func TestClassifier_RetryAfterHTTPDate(t *testing.T) {
	c := NewClassifier()
	// Retry-After as an HTTP-date 30 seconds in the future.
	future := time.Now().Add(30 * time.Second).UTC()
	headers := map[string]string{
		"Retry-After": future.Format(http.TimeFormat),
	}
	resp := mkResp(429, headers)
	dec := c.OnUpstreamError(resp, []byte(""))

	if dec.Class != DecisionTransient {
		t.Errorf("HTTP-date Retry-After 30s should be transient, got %v", dec.Class)
	}
	// Allow 5s tolerance for clock skew.
	if dec.RetryAfter < 25*time.Second || dec.RetryAfter > 35*time.Second {
		t.Errorf("RetryAfter = %v, want ~30s", dec.RetryAfter)
	}
}

func TestClassifier_ResetDateFromRetryAfter(t *testing.T) {
	c := NewClassifier()
	resp := mkResp(429, map[string]string{"Retry-After": "7200"}) // 2 hours
	body := `{"error":"monthly_limit_exceeded"}`
	dec := c.OnUpstreamError(resp, []byte(body))

	if dec.Class != DecisionHard {
		t.Fatalf("should be hard")
	}
	// ResetDate ~ now + 2h
	expected := time.Now().Add(2 * time.Hour)
	delta := dec.ResetDate.Sub(expected)
	if delta < -2*time.Second || delta > 2*time.Second {
		t.Errorf("ResetDate = %v, want ~%v (delta %v)", dec.ResetDate, expected, delta)
	}
}

// contains is a small helper for substring assertions on classifier reasons.
func contains(s, substr string) bool {
	if len(s) < len(substr) {
		return false
	}
	for i := 0; i+len(substr) <= len(s); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

// strconvOK silences unused-import in case we drop the helper later.
var _ = strconv.Itoa
