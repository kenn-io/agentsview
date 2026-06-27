//go:build pgtest

package postgres

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
)

// TestStoreGetAnalyticsSignals exercises the PG implementation
// of GetAnalyticsSignals end to end: seed signals on local
// rows, push to PG, then read them back through the Store and
// confirm the aggregated response matches what the SQLite
// implementation would have produced over the same data.
func TestStoreGetAnalyticsSignals(t *testing.T) {
	pgURL := testPGURL(t)
	cleanPGSchema(t, pgURL)
	t.Cleanup(func() { cleanPGSchema(t, pgURL) })

	local := testDB(t)
	ps, err := New(
		pgURL, "agentsview", local,
		"signals-test-machine", true,
		SyncOptions{},
	)
	require.NoError(t, err, "creating sync")
	defer ps.Close()

	ctx := context.Background()
	require.NoError(t, ps.EnsureSchema(ctx), "ensure schema")

	score := 90
	grade := "A"
	pressure := 0.42
	started := time.Now().UTC().Add(-1 * time.Hour).
		Format(time.RFC3339)
	first := "hi"

	for _, id := range []string{"sig-1", "sig-2"} {
		sess := db.Session{
			ID:           id,
			Project:      "proj",
			Machine:      "local",
			Agent:        "claude",
			FirstMessage: &first,
			StartedAt:    &started,
			MessageCount: 4,
		}
		require.NoError(t, local.UpsertSession(sess),
			"upsert %s", id)
		require.NoError(t, local.UpdateSessionSignals(
			id,
			db.SessionSignalUpdate{
				Outcome:                "completed",
				OutcomeConfidence:      "high",
				EndedWithRole:          "assistant",
				HasToolCalls:           true,
				HasContextData:         true,
				ToolFailureSignalCount: 1,
				CompactionCount:        1,
				MidTaskCompactionCount: 1,
				ContextPressureMax:     &pressure,
				HealthScore:            &score,
				HealthGrade:            &grade,
			},
		), "UpdateSessionSignals %s", id)
	}

	_, err = ps.Push(ctx, false, nil)
	require.NoError(t, err, "push")

	store, err := NewStore(pgURL, "agentsview", true)
	require.NoError(t, err, "NewStore")
	defer store.Close()

	// Empty AnalyticsFilter must be accepted -- exercises the
	// sentinel-bound path in analyticsUTCRange that earlier
	// produced "T00:00:00Z" and tripped PG's TIMESTAMPTZ cast.
	resp, err := store.GetAnalyticsSignals(
		ctx, db.AnalyticsFilter{},
	)
	require.NoError(t, err, "GetAnalyticsSignals")

	assert.Equal(t, 2, resp.ScoredSessions)
	assert.Equal(t, 2, resp.GradeDistribution["A"])
	assert.Equal(t, 2, resp.OutcomeDistribution["completed"])
	require.NotNil(t, resp.AvgHealthScore)
	assert.Equal(t, 90.0, *resp.AvgHealthScore)
	assert.Equal(t, 2, resp.ContextHealth.MidTaskCompactionCount)
	assert.Equal(t, 2, resp.ToolHealth.TotalFailureSignals)
	require.Len(t, resp.ByAgent, 1)
	assert.Equal(t, "claude", resp.ByAgent[0].Agent)
	assert.Equal(t, 2, resp.ByAgent[0].SessionCount)
	require.Len(t, resp.ByProject, 1)
	assert.Equal(t, "proj", resp.ByProject[0].Project)
	assert.Equal(t, 2, resp.ByProject[0].SessionCount)
}

func TestStoreGetAnalyticsSignalSessionsModelFilterUsesMatchingMessages(
	t *testing.T,
) {
	pgURL := testPGURL(t)
	cleanPGSchema(t, pgURL)
	t.Cleanup(func() { cleanPGSchema(t, pgURL) })

	local := testDB(t)
	ps, err := New(
		pgURL, "agentsview", local,
		"signals-model-filter-machine", true,
		SyncOptions{},
	)
	require.NoError(t, err, "creating sync")
	defer ps.Close()

	ctx := context.Background()
	require.NoError(t, ps.EnsureSchema(ctx), "ensure schema")

	started := "2024-06-01T09:00:00Z"
	first := "tool evidence"
	require.NoError(t, local.UpsertSession(db.Session{
		ID:           "signal-mixed",
		Project:      "proj",
		Machine:      "local",
		Agent:        "claude",
		FirstMessage: &first,
		StartedAt:    &started,
		MessageCount: 2,
	}), "upsert session")
	require.NoError(t, local.InsertMessages([]db.Message{
		{
			SessionID: "signal-mixed", Ordinal: 0, Role: "assistant",
			Content: "claude tool evidence", ContentLength: 20,
			Timestamp:  "2024-06-01T09:05:00Z",
			Model:      "claude-3-5-sonnet",
			HasToolUse: true,
		},
		{
			SessionID: "signal-mixed", Ordinal: 1, Role: "assistant",
			Content: "gpt tool evidence", ContentLength: 17,
			Timestamp:  "2024-06-01T09:06:00Z",
			Model:      "gpt-4o",
			HasToolUse: true,
		},
	}), "insert messages")
	require.NoError(t, local.UpdateSessionSignals(
		"signal-mixed",
		db.SessionSignalUpdate{ToolFailureSignalCount: 1},
	), "update session signals")

	_, err = ps.Push(ctx, false, nil)
	require.NoError(t, err, "push")

	store, err := NewStore(pgURL, "agentsview", true)
	require.NoError(t, err, "NewStore")
	defer store.Close()

	resp, err := store.GetAnalyticsSignalSessions(
		ctx,
		db.AnalyticsFilter{
			From:     "2024-06-01",
			To:       "2024-06-01",
			Timezone: "UTC",
			Model:    "gpt-4o",
		},
		"tool_failure_signals",
		10,
	)
	require.NoError(t, err, "GetAnalyticsSignalSessions")
	require.Len(t, resp.Sessions, 1, "len(Sessions)")
	assert.Equal(t, "gpt tool evidence", resp.Sessions[0].Excerpt)
	require.NotNil(t, resp.Sessions[0].MessageOrdinal)
	assert.Equal(t, 1, *resp.Sessions[0].MessageOrdinal)
}
