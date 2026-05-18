package keypool

import "time"

// Key represents a single upstream API key with its lifecycle state.
// Persisted in ~/.config/oc-go-cc/key-states.json as part of api_keys[].
type Key struct {
	Token       string     `json:"token"`
	Account     string     `json:"account"`
	ExhaustedAt *time.Time `json:"exhaustedAt,omitempty"`
	ResetDate   time.Time  `json:"resetDate"`
	LastUsed    *time.Time `json:"lastUsed,omitempty"`

	// Reserved for Phase 1.5 proactive usage scraper. Always nil in v1 —
	// schema hooks so we don't need a state file migration later.
	WeeklyUsagePercent  *int `json:"weeklyUsagePercent,omitempty"`
	MonthlyUsagePercent *int `json:"monthlyUsagePercent,omitempty"`
}

// IsActive reports whether the key is currently usable (not exhausted).
func (k *Key) IsActive() bool {
	return k.ExhaustedAt == nil
}

// IsResetDue reports whether the key's reset date has passed.
//
// Direct timestamp comparison so sub-day TTLs (used by the transient
// retry escalation, 15-min default) auto-clear on the request hot path.
// Multi-day TTLs (monthly cap, 30d default) still work the same — a
// resetDate of 2026-06-04 23:59 becomes due exactly then, not at midnight.
//
// Zero ResetDate → never due (no-op). The revalidator goroutine is the
// fallback safety net for keys mistakenly marked with zero ResetDate.
func (k *Key) IsResetDue(now time.Time) bool {
	if k.ResetDate.IsZero() {
		return false
	}
	return !k.ResetDate.After(now)
}
