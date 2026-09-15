//go:build !(windows && arm64)

// ABOUTME: Store-contract parity for duplicate-role usage suppression: the
// ABOUTME: DuckDB mirror must not double-count migrated duplicate copies in
// ABOUTME: daily/top/counts aggregates, exactly like the SQLite archive.
package duckdb

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
)

// duplicateSuppressionWrites seeds the migration shape from
// internal/db/duplicate_suppression_test.go: the old-store canonical carries
// token-bearing usage, the migrated duplicate carries its own usage_event so
// a naive aggregate would double-count it.
func duplicateSuppressionWrites() []db.SessionBatchWrite {
	started := "2026-08-10T08:00:00Z"
	first := "I need you to research and create a comprehensive plan."
	session := func(id, agent string, msgs int) db.Session {
		return db.Session{
			ID: id, Project: "project", Machine: "local", Agent: agent,
			StartedAt:    &started,
			FirstMessage: &first,
			MessageCount: msgs,
		}
	}
	return []db.SessionBatchWrite{
		{
			Session: session("goose:old", "goose", 5),
			Messages: []db.Message{{
				SessionID: "goose:old", Ordinal: 0, Role: "assistant",
				Timestamp: "2026-08-10T09:00:00Z", Model: "ossington-5",
				TokenUsage:      json.RawMessage(`{"input_tokens":2,"output_tokens":3067}`),
				ClaudeMessageID: "m-old", ClaudeRequestID: "r-old",
				SourceUUID: "u-old",
			}},
			DataVersion: 1, ReplaceMessages: true,
		},
		{
			Session: session("augure-desktop:new", "augure-desktop", 4),
			UsageEvents: []db.UsageEvent{{
				Source: "session", Model: "ossington-5", InputTokens: 7,
				OutputTokens: 9, OccurredAt: "2026-08-10T09:00:05Z",
				DedupKey: "dup-1",
			}},
			DataVersion: 1,
		},
	}
}

func TestDuckAggregateUsageSuppressesDuplicateLikeSQLite(t *testing.T) {
	ctx := context.Background()
	local := newLocalDB(t)
	_, err := local.WriteSessionBatchAtomic(duplicateSuppressionWrites())
	require.NoError(t, err)
	syncer := newInMemoryTestSync(t, local, SyncOptions{})
	require.NoError(t, createSchema(ctx, syncer.DB()))

	// Push before grouping: the mirror starts with no membership rows,
	// like a mirror that lags the archive.
	_, err = syncer.pushEverything(ctx, nil)
	require.NoError(t, err)

	// Group the twins and push again; the rebuild bumps
	// local_modified_at on changed sessions, so the re-push carries the
	// new membership into duplicate_group_members.
	result, err := local.RebuildDuplicateGroups(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, result.Groups)
	_, err = syncer.pushEverything(ctx, nil)
	require.NoError(t, err)
	duck := NewStoreFromDB(syncer.DB())

	filter := db.UsageFilter{}

	// Daily usage: only the canonical's tokens may appear.
	daily, err := duck.GetDailyUsage(ctx, filter)
	require.NoError(t, err, "GetDailyUsage")
	assert.Equal(t, 3067, daily.Totals.OutputTokens,
		"daily usage suppresses the duplicate's usage_event copy")

	// Top sessions: the duplicate must not appear as its own entry.
	top, err := duck.GetTopSessionsByCost(ctx, filter, 10)
	require.NoError(t, err, "GetTopSessionsByCost")
	require.Len(t, top, 1)
	assert.Equal(t, "goose:old", top[0].SessionID,
		"top sessions list only the canonical")

	// Session counts count the canonical, not the duplicate copy.
	counts, err := duck.GetUsageSessionCounts(ctx, filter)
	require.NoError(t, err, "GetUsageSessionCounts")
	assert.Equal(t, 1, counts.Total,
		"usage session counts suppress duplicate copies")

	// Per-session usage is never suppressed: the duplicate's own detail
	// view keeps its numbers.
	sessUsage, err := duck.GetSessionUsage(ctx, "augure-desktop:new", true)
	require.NoError(t, err, "GetSessionUsage")
	require.NotNil(t, sessUsage)
	require.NotEmpty(t, sessUsage.Breakdown,
		"per-session usage keeps the duplicate's own rows")
	total := 0
	for _, row := range sessUsage.Breakdown {
		total += row.OutputTokens
	}
	assert.Equal(t, 9, total,
		"per-session usage keeps the duplicate's own numbers")

	// Matching-session counts mirror SQLite's activity path: unsuppressed.
	matching, err := duck.GetUsageMatchingSessionCount(ctx, filter)
	require.NoError(t, err, "GetUsageMatchingSessionCount")
	assert.Equal(t, 2, matching,
		"matching-session counts stay unsuppressed")
}
