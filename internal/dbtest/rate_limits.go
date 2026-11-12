package dbtest

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
)

func rateLimitWindow(used float64, minutes int64, resets string) *db.RateLimitWindow {
	return &db.RateLimitWindow{UsedPercent: used, WindowMinutes: &minutes, ResetsAt: resets}
}

// SeedRateLimitSnapshots writes rate-limit reports that exercise the latest
// selection: two accounts, an older report from a later-sorting session, a
// report with only a secondary window, and absent window lengths and reset
// times. Every backend runs AssertLatestRateLimits over them.
func SeedRateLimitSnapshots(tb testing.TB, local *db.DB) {
	tb.Helper()
	require.NoError(tb, local.InsertRateLimitSnapshots(tb.Context(), []db.RateLimitSnapshot{
		{Agent: "codex", AccountKey: "acct-a", LimitID: "codex", PlanType: "pro", SessionID: "codex:s1",
			ObservedAt: "2026-11-10T09:00:00.000Z",
			Primary:    rateLimitWindow(10, 10080, "2026-11-16T06:00:00.000Z")},
		{Agent: "codex", AccountKey: "acct-a", LimitID: "codex", PlanType: "pro", SessionID: "codex:s9",
			ObservedAt: "2026-11-10T08:00:00.000Z",
			Primary:    rateLimitWindow(9, 10080, "2026-11-16T06:00:00.000Z")},
		{Agent: "codex", AccountKey: "acct-a", LimitID: "codex", PlanType: "pro", SessionID: "codex:s2",
			ObservedAt: "2026-11-11T09:30:00.250Z",
			Primary:    rateLimitWindow(42.5, 300, "2026-11-11T12:00:00.000Z"),
			Secondary:  rateLimitWindow(12, 10080, "2026-11-16T06:00:00.000Z")},
		{Agent: "codex", AccountKey: "acct-b", LimitID: "codex", SessionID: "codex:s3",
			ObservedAt: "2026-11-09T09:00:00.000Z",
			Secondary:  &db.RateLimitWindow{UsedPercent: 3}},
	}), "seed rate limit snapshots")
}

// AssertLatestRateLimits checks that store returns the latest report for
// each account and limit seeded by SeedRateLimitSnapshots, newest first, with
// every reported value intact.
func AssertLatestRateLimits(tb testing.TB, store db.Store) {
	tb.Helper()
	got, err := store.GetLatestRateLimits(tb.Context())
	require.NoError(tb, err, "GetLatestRateLimits")
	assert.Equal(tb, []db.RateLimitSnapshot{
		{Agent: "codex", AccountKey: "acct-a", LimitID: "codex", PlanType: "pro", SessionID: "codex:s2",
			ObservedAt: "2026-11-11T09:30:00.250Z",
			Primary:    rateLimitWindow(42.5, 300, "2026-11-11T12:00:00.000Z"),
			Secondary:  rateLimitWindow(12, 10080, "2026-11-16T06:00:00.000Z")},
		{Agent: "codex", AccountKey: "acct-b", LimitID: "codex", SessionID: "codex:s3",
			ObservedAt: "2026-11-09T09:00:00.000Z",
			Secondary:  &db.RateLimitWindow{UsedPercent: 3}},
	}, got)
}
