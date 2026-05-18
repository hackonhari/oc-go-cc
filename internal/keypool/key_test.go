package keypool

import (
	"testing"
	"time"
)

// TestIsResetDue covers the timestamp-based reset semantics. Prior
// implementation used day-resolution Truncate(24h), incompatible with
// the sub-day TTLs introduced by transient-retry escalation (2026-05-18).
//
// The function must work uniformly for sub-day, exactly-now, and
// multi-day TTLs without special cases.
func TestIsResetDue(t *testing.T) {
	now := time.Now()

	cases := []struct {
		name      string
		resetDate time.Time
		want      bool
	}{
		{"zero ResetDate → never due", time.Time{}, false},
		{"reset 1 minute ago → due", now.Add(-1 * time.Minute), true},
		{"reset 5 minutes in future → not yet due", now.Add(5 * time.Minute), false},
		{"reset 15 minutes in future → not yet due (transient escalation TTL)", now.Add(15 * time.Minute), false},
		{"reset 24 hours ago → due", now.Add(-24 * time.Hour), true},
		{"reset exactly now → due (boundary case)", now, true},
		{"reset 1 second from now → not yet due", now.Add(1 * time.Second), false},
		{"reset 30 days in future → not yet due (real exhaustion)", now.Add(30 * 24 * time.Hour), false},
		{"reset 30 days ago → due (long-overdue reset)", now.Add(-30 * 24 * time.Hour), true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			k := &Key{Token: "sk-test", ResetDate: tc.resetDate}
			if got := k.IsResetDue(now); got != tc.want {
				t.Errorf("IsResetDue() = %v, want %v (resetDate=%v)", got, tc.want, tc.resetDate)
			}
		})
	}
}

// TestIsResetDue_AutoClearAfterTransientTTL confirms the end-to-end
// scenario from the 2026-05-18 fix: a key marked with 15-min TTL
// should auto-clear via Acquire's reset check once 15min has elapsed.
//
// We simulate this by setting ResetDate slightly in the past.
func TestIsResetDue_AutoClearAfterTransientTTL(t *testing.T) {
	now := time.Now()
	// Simulate 15min escalation TTL that has just elapsed.
	resetDate := now.Add(-1 * time.Second)

	k := &Key{Token: "sk-test", ResetDate: resetDate}
	if !k.IsResetDue(now) {
		t.Errorf("a key with ResetDate 1s in the past should be reset-due, got false")
	}
}
