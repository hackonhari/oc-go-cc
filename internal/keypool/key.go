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
// Day-resolution semantics: a resetDate of 2026-06-04 becomes due at any
// time on 2026-06-04 (inclusive).
func (k *Key) IsResetDue(now time.Time) bool {
	if k.ResetDate.IsZero() {
		return false
	}
	// Compare date components only; ignore time-of-day.
	resetDay := k.ResetDate.Truncate(24 * time.Hour)
	today := now.Truncate(24 * time.Hour)
	return !resetDay.After(today)
}
