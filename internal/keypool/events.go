package keypool

import "time"

// Event type constants used in the rotation event log.
// The actual JSONL logger lives in this package's log.go; this file
// defines the wire format consumed by other packages.
const (
	EventKeyAcquired                = "key_acquired"
	EventKeyExhaustedHard           = "key_exhausted_hard"
	EventKeyThrottleTransient       = "key_throttle_transient"
	EventKeyResetAutoclear          = "key_reset_autoclear"
	EventKeyRevived                 = "key_revived"
	EventKeyRevalidationStillFailed = "key_revalidation_still_exhausted"
	EventFreeFallbackEngaged        = "free_fallback_engaged"
	EventFreeFallbackModelFail      = "free_fallback_model_failed"
	EventAllExhausted502            = "all_exhausted_502"
)

// Event is a structured rotation event written to the JSONL daily log.
type Event struct {
	Timestamp   time.Time `json:"ts"`
	Type        string    `json:"event"`
	Account     string    `json:"account,omitempty"`
	Model       string    `json:"model,omitempty"`
	RetryAfter  *int      `json:"retry_after,omitempty"`
	BodyExcerpt string    `json:"body_excerpt,omitempty"`
	RequestID   string    `json:"request_id,omitempty"`
	Reason      string    `json:"reason,omitempty"`
	Mode        string    `json:"mode,omitempty"` // "forced" | "fallback" for free_fallback_engaged
}

// EventEmitter is the abstraction for the rotation event log.
// Implemented by *EventLogger (added in Phase 7); kept as interface so
// tests can inject no-op and so the dependency direction stays inward.
type EventEmitter interface {
	Emit(event Event)
}

// noopEmitter is the default emitter when none has been wired.
type noopEmitter struct{}

func (noopEmitter) Emit(Event) {}
