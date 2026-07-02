//go:build !(windows && arm64)

package duckdb

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
)

// newEmptyDuckStore returns a DuckDB store with the schema initialized but
// no session rows, so tests can seed precise session states (including
// soft-deleted and zero-message sessions) via direct inserts.
func newEmptyDuckStore(t *testing.T) *Store {
	t.Helper()
	return newDuckAnalyticsStore(t, nil)
}

// insertDuckCountSession seeds one session row for CountSessionsForUsage
// tests. Empty ended/deleted map to SQL NULL.
func insertDuckCountSession(
	t *testing.T, store *Store, id, project, agent, started, ended string,
	messageCount int, deleted string,
) {
	t.Helper()
	ctx := context.Background()
	endedExpr := "NULL"
	if ended != "" {
		endedExpr = "CAST('" + ended + "' AS TIMESTAMP)"
	}
	deletedExpr := "NULL"
	if deleted != "" {
		deletedExpr = "CAST('" + deleted + "' AS TIMESTAMP)"
	}
	_, err := store.DB().ExecContext(ctx, `
		INSERT INTO sessions (
			id, machine, project, agent, started_at, ended_at,
			message_count, user_message_count, deleted_at
		) VALUES (
			?, 'test-machine', ?, ?, CAST(? AS TIMESTAMP), `+endedExpr+`,
			?, 1, `+deletedExpr+`
		)`, id, project, agent, started, messageCount)
	require.NoError(t, err, "insert duck session %s", id)
}

func TestDuckCountSessionsForUsage_NoTokenSessionCounted(t *testing.T) {
	store := newEmptyDuckStore(t)
	ctx := context.Background()

	insertDuckCountSession(t, store, "cop-1", "proj-a", "copilot",
		"2024-06-15T10:00:00Z", "2024-06-15T10:05:00Z", 1, "")

	f := db.UsageFilter{From: "2024-06-01", To: "2024-06-30", Timezone: "UTC"}
	n, err := store.CountSessionsForUsage(ctx, f)
	require.NoError(t, err, "CountSessionsForUsage")
	assert.Equal(t, 1, n, "no-token session must be counted")

	counts, err := store.GetUsageSessionCounts(ctx, f)
	require.NoError(t, err, "GetUsageSessionCounts")
	assert.Equal(t, 0, counts.Total, "usage-row count misses no-token session")
}

func TestDuckCountSessionsForUsage_ExcludesDeletedAndEmpty(t *testing.T) {
	store := newEmptyDuckStore(t)
	ctx := context.Background()

	insertDuckCountSession(t, store, "live", "proj-a", "copilot",
		"2024-06-15T10:00:00Z", "", 1, "")
	insertDuckCountSession(t, store, "deleted", "proj-a", "copilot",
		"2024-06-15T10:00:00Z", "", 1, "2024-06-16T00:00:00Z")
	insertDuckCountSession(t, store, "empty", "proj-a", "copilot",
		"2024-06-15T10:00:00Z", "", 0, "")

	n, err := store.CountSessionsForUsage(ctx, db.UsageFilter{
		From: "2024-06-01", To: "2024-06-30", Timezone: "UTC",
	})
	require.NoError(t, err, "CountSessionsForUsage")
	assert.Equal(t, 1, n, "only the live non-empty session counts")
}

func TestDuckCountSessionsForUsage_FilterPredicates(t *testing.T) {
	store := newEmptyDuckStore(t)
	ctx := context.Background()

	insertDuckCountSession(t, store, "cop-a", "proj-a", "copilot",
		"2024-06-15T10:00:00Z", "", 1, "")
	insertDuckCountSession(t, store, "vscop-a", "proj-a", "vscode-copilot",
		"2024-06-15T10:00:00Z", "", 1, "")
	insertDuckCountSession(t, store, "cop-b", "proj-b", "copilot",
		"2024-06-15T10:00:00Z", "", 1, "")
	insertDuckCountSession(t, store, "claude-a", "proj-a", "claude",
		"2024-06-15T10:00:00Z", "", 1, "")

	base := db.UsageFilter{From: "2024-06-01", To: "2024-06-30", Timezone: "UTC"}

	f := base
	f.Agent = "copilot,vscode-copilot"
	n, err := store.CountSessionsForUsage(ctx, f)
	require.NoError(t, err, "CSV agent filter")
	assert.Equal(t, 3, n, "three Copilot-family sessions, claude excluded")

	f.ExcludeProject = "proj-b"
	n, err = store.CountSessionsForUsage(ctx, f)
	require.NoError(t, err, "CSV agent + exclude_project")
	assert.Equal(t, 2, n, "proj-b Copilot session excluded")
}

func TestDuckCountSessionsForUsage_TimezoneDayBoundary(t *testing.T) {
	store := newEmptyDuckStore(t)
	ctx := context.Background()

	// America/New_York is UTC-4 in June (EDT).
	// local 2024-06-15 23:30 == UTC 2024-06-16 03:30 -> in-window.
	insertDuckCountSession(t, store, "late", "proj-a", "copilot",
		"2024-06-16T03:30:00Z", "2024-06-16T03:35:00Z", 1, "")
	// local 2024-06-16 00:15 == UTC 2024-06-16 04:15 -> next local day.
	insertDuckCountSession(t, store, "past-midnight", "proj-a", "copilot",
		"2024-06-16T04:15:00Z", "2024-06-16T04:20:00Z", 1, "")

	n, err := store.CountSessionsForUsage(ctx, db.UsageFilter{
		From: "2024-06-15", To: "2024-06-15", Timezone: "America/New_York",
	})
	require.NoError(t, err, "CountSessionsForUsage")
	assert.Equal(t, 1, n, "only the 23:30-local session is in the local day")
}

func TestDuckCountSessionsForUsage_MultiDayOverlap(t *testing.T) {
	store := newEmptyDuckStore(t)
	ctx := context.Background()

	insertDuckCountSession(t, store, "multi-day", "proj-a", "copilot",
		"2024-06-10T10:00:00Z", "2024-06-20T10:00:00Z", 1, "")
	// Null ended_at: upper endpoint falls back to started_at (in-window).
	insertDuckCountSession(t, store, "no-end", "proj-a", "copilot",
		"2024-06-15T10:00:00Z", "", 1, "")
	insertDuckCountSession(t, store, "before", "proj-a", "copilot",
		"2024-06-01T10:00:00Z", "2024-06-02T10:00:00Z", 1, "")

	n, err := store.CountSessionsForUsage(ctx, db.UsageFilter{
		From: "2024-06-15", To: "2024-06-15", Timezone: "UTC",
	})
	require.NoError(t, err, "CountSessionsForUsage")
	assert.Equal(t, 2, n, "multi-day overlap and no-end fallback both count")
}
