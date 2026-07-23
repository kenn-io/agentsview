package artifact

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
)

func TestExportIncludesUsageOnlySession(t *testing.T) {
	ctx := context.Background()
	store := newTestArtifactStore(t)
	origin := "laptop-a1b2c3"
	database := testDB(t)

	// A usage-only session: owned, zero messages, with a usage event. The
	// sidebar list filter (message_count > 0) hides it, so export must not rely
	// on that path.
	require.NoError(t, database.UpsertSession(db.Session{
		ID:        "usage-1",
		Project:   "alpha",
		Machine:   "local",
		Agent:     "claude",
		CreatedAt: "2026-06-14T01:02:03Z",
	}))
	require.NoError(t, database.ReplaceSessionUsageEvents("usage-1", []db.UsageEvent{{
		SessionID:    "usage-1",
		Source:       "assistant",
		Model:        "claude-opus-4-8",
		InputTokens:  10,
		OutputTokens: 20,
		OccurredAt:   "2026-06-14T01:02:30Z",
		DedupKey:     "usage-1:0",
	}}))

	exportResult, err := ExportToStore(ctx, database, store, ExportOptions{Origin: origin, Full: true})
	require.NoError(t, err)
	assert.Equal(t, 1, exportResult.ExportedSessions)

	gid := origin + "~usage-1"
	_, cp, err := latestStoreCheckpointSummary(ctx, store, origin)
	require.NoError(t, err)
	require.NotNil(t, cp)
	assert.Contains(t, cp.Sessions, gid, "usage-only session must be in the checkpoint")

	// It imports into a peer as a real session with its usage events intact.
	importDB := testDB(t)
	res, err := importResultFromTestStore(ctx, importDB, store, "desktop-d4e5f6")
	require.NoError(t, err)
	assert.Equal(t, 1, res.Sessions)

	got, err := importDB.GetSessionFull(ctx, gid)
	require.NoError(t, err)
	require.NotNil(t, got)
	events, err := importDB.GetUsageEvents(ctx, gid)
	require.NoError(t, err)
	require.Len(t, events, 1)
	assert.Equal(t, 10, events[0].InputTokens)
	assert.Equal(t, 20, events[0].OutputTokens)
}

func TestExportSkipsDeletedAndForeignSessions(t *testing.T) {
	ctx := context.Background()
	store := newTestArtifactStore(t)
	origin := "laptop-a1b2c3"
	database := testDB(t)

	seedSession(t, database, "owned", "alpha")
	// A soft-deleted owned session and a foreign-owned session are both excluded.
	seedSession(t, database, "trashed", "alpha")
	require.NoError(t, database.SoftDeleteSession("trashed"))
	require.NoError(t, database.UpsertSession(db.Session{
		ID:        "foreign",
		Project:   "alpha",
		Machine:   "desktop-d4e5f6",
		Agent:     "claude",
		CreatedAt: "2026-06-14T01:02:03Z",
	}))

	_, err := ExportToStore(ctx, database, store, ExportOptions{Origin: origin, Full: true})
	require.NoError(t, err)

	_, cp, err := latestStoreCheckpointSummary(ctx, store, origin)
	require.NoError(t, err)
	require.NotNil(t, cp)
	assert.Contains(t, cp.Sessions, origin+"~owned")
	assert.NotContains(t, cp.Sessions, origin+"~trashed")
	assert.NotContains(t, cp.Sessions, origin+"~foreign")
}
