package keypool

import (
	"net/http"
	"time"
)

// DecisionClass enumerates the three classifier outcomes for an upstream 429.
type DecisionClass int

const (
	// DecisionTransient means the same key should be retried after a short backoff.
	DecisionTransient DecisionClass = iota
	// DecisionHard means the key is genuinely exhausted; rotate to the next key.
	DecisionHard
	// DecisionAmbiguous is treated as Hard for safety, but is logged distinctly.
	DecisionAmbiguous
)

// String renders the class for log output.
func (d DecisionClass) String() string {
	switch d {
	case DecisionTransient:
		return "transient"
	case DecisionHard:
		return "hard"
	case DecisionAmbiguous:
		return "ambiguous"
	default:
		return "unknown"
	}
}

// RotationDecision captures the outcome of inspecting an upstream 429 response.
type RotationDecision struct {
	Class DecisionClass

	// RetryAfter is the suggested wait time before retrying the SAME key
	// (Transient only). Zero when not applicable.
	RetryAfter time.Duration

	// ResetDate is the computed reactivation date (Hard only). Zero when
	// not applicable.
	ResetDate time.Time

	// Reason is a short human-readable label for logging.
	Reason string
}

// RotationSignal is the abstraction that turns an upstream error response
// into a rotation decision. The "error" here covers OpenCode's two
// observed exhaustion shapes:
//   - HTTP 401 with body `{"error":{"type":"CreditsError",...}}` (monthly cap hit)
//   - HTTP 429 with quota-language body (rate-limit / weekly cap)
//
// v1 ships with a single implementation in classifier.go. Future
// Phase 1.5 work may add proactive usage poll signals via additional
// interface methods.
type RotationSignal interface {
	OnUpstreamError(resp *http.Response, body []byte) RotationDecision
}
