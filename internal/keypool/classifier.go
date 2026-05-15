package keypool

import (
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Classifier inspects upstream error responses and decides whether the
// current key should be rotated (hard exhaustion) or retried after a
// short backoff (transient throttle).
//
// Real-world OpenCode Go responses observed (2026-05-15):
//
//	HTTP 401 + {"type":"error","error":{"type":"CreditsError","message":"Insufficient balance..."}}
//	  → Monthly/weekly cap hit. Hard exhaustion.
//
//	HTTP 429 + body containing "rate limit" or quota language
//	  → Either burst throttle (Retry-After short) or weekly cap (Retry-After long).
//
// This classifier handles both shapes; other status codes (200, 5xx) are
// not the classifier's concern — the handler routes them directly.
type Classifier struct {
	// transientRetryAfterMax is the upper bound on Retry-After (seconds)
	// for a 429 to be classified as transient. Defaults to 60s.
	transientRetryAfterMax time.Duration

	// fallbackResetDuration is added to now() when the upstream signals
	// hard exhaustion but provides no Retry-After. Defaults to 30 days.
	fallbackResetDuration time.Duration
}

// NewClassifier returns a Classifier with default thresholds.
func NewClassifier() *Classifier {
	return &Classifier{
		transientRetryAfterMax: 60 * time.Second,
		fallbackResetDuration:  30 * 24 * time.Hour,
	}
}

// quotaKeywords are case-insensitive substrings that, when present in
// a 4xx/5xx response body, indicate quota/cap exhaustion (hard).
var quotaKeywords = []string{
	"creditserror",           // OpenCode primary signal (401 body type field)
	"insufficient balance",   // OpenCode CreditsError message
	"monthly_limit_exceeded", // hypothetical 429 body code
	"weekly_limit_exceeded",  // hypothetical 429 body code
	"quota",
	"rate limit reached",
	"usage limit",
}

// OnUpstreamError implements RotationSignal.
func (c *Classifier) OnUpstreamError(resp *http.Response, body []byte) RotationDecision {
	now := time.Now()

	switch resp.StatusCode {
	case http.StatusUnauthorized:
		// 401 from OpenCode == CreditsError (monthly cap). Always hard.
		// Other 401 causes (truly invalid token) also benefit from rotation
		// — a broken key shouldn't keep retrying.
		return RotationDecision{
			Class:     DecisionHard,
			ResetDate: now.Add(c.fallbackResetDuration),
			Reason:    classifyReason(body, "401_credits_or_auth"),
		}

	case http.StatusTooManyRequests:
		retryAfter := parseRetryAfter(resp.Header.Get("Retry-After"), now)
		hasQuotaBody := containsAny(string(body), quotaKeywords)

		// Hard: quota-language body OR long Retry-After (>60s).
		if hasQuotaBody {
			reset := now.Add(c.fallbackResetDuration)
			if retryAfter > 0 {
				reset = now.Add(retryAfter)
			}
			return RotationDecision{
				Class:     DecisionHard,
				ResetDate: reset,
				Reason:    classifyReason(body, "429_quota"),
			}
		}
		if retryAfter > c.transientRetryAfterMax {
			return RotationDecision{
				Class:     DecisionHard,
				ResetDate: now.Add(retryAfter),
				Reason:    "429_retry_after_long",
			}
		}

		// Transient: short Retry-After AND no quota body.
		if retryAfter > 0 {
			return RotationDecision{
				Class:      DecisionTransient,
				RetryAfter: retryAfter,
				Reason:     "429_transient_throttle",
			}
		}

		// Ambiguous: 429 with no header and no body code. Treat as Hard
		// for safety (better to over-rotate than spin on a dead key).
		return RotationDecision{
			Class:     DecisionAmbiguous,
			ResetDate: now.Add(c.fallbackResetDuration),
			Reason:    "429_ambiguous_default_hard",
		}
	}

	// Other status codes are not classifier business — caller should not
	// route them here. Return Hard with explanatory reason as a safety net.
	return RotationDecision{
		Class:     DecisionHard,
		ResetDate: now.Add(c.fallbackResetDuration),
		Reason:    "unexpected_status_" + strconv.Itoa(resp.StatusCode),
	}
}

// containsAny reports whether s (case-insensitive) contains any of the
// needles.
func containsAny(s string, needles []string) bool {
	lower := strings.ToLower(s)
	for _, n := range needles {
		if strings.Contains(lower, n) {
			return true
		}
	}
	return false
}

// parseRetryAfter parses the Retry-After header as either seconds or
// an HTTP-date. Returns 0 if the header is absent or invalid.
func parseRetryAfter(header string, now time.Time) time.Duration {
	if header == "" {
		return 0
	}
	if secs, err := strconv.Atoi(strings.TrimSpace(header)); err == nil {
		if secs < 0 {
			return 0
		}
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(header); err == nil {
		d := t.Sub(now)
		if d < 0 {
			return 0
		}
		return d
	}
	return 0
}

// classifyReason produces a compact log label including the body's
// extracted error type code when present.
func classifyReason(body []byte, fallback string) string {
	s := strings.ToLower(string(body))
	if strings.Contains(s, "creditserror") {
		return "credits_exhausted"
	}
	if strings.Contains(s, "insufficient balance") {
		return "credits_exhausted"
	}
	if strings.Contains(s, "monthly_limit_exceeded") {
		return "monthly_cap"
	}
	if strings.Contains(s, "weekly_limit_exceeded") {
		return "weekly_cap"
	}
	if strings.Contains(s, "rate limit reached") {
		return "rate_limited"
	}
	return fallback
}
