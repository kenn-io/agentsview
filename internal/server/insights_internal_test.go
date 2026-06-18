package server

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
)

// TestActivityRangeSummaryUsesRequestTimezone confirms the insight activity
// summary resolves its window in the request timezone, so a non-UTC viewer's
// summary covers the same local-day window as the activity dashboard the dates
// were derived from. A session whose only instant is 2026-06-16T02:00:00Z is
// the local day 2026-06-15 in America/New_York (UTC-4 in June) but the UTC day
// 2026-06-16, so the June-15 summary must include it under New York and
// exclude it under UTC. Before the fix the window was always UTC, so the New
// York request would have wrongly excluded the session.
func TestActivityRangeSummaryUsesRequestTimezone(t *testing.T) {
	srv := testServer(t, 0)
	ctx := context.Background()
	ts := "2026-06-16T02:00:00Z"
	require.NoError(t, srv.db.UpsertSession(db.Session{
		ID: "x", Project: "proj", Machine: "test", Agent: "claude",
		StartedAt: &ts, EndedAt: &ts, MessageCount: 1,
		RelationshipType: "root", DataVersion: 1,
	}))
	require.NoError(t, srv.db.ReplaceSessionMessages("x", []db.Message{{
		SessionID: "x", Ordinal: 0, Role: "assistant", Content: "x",
		Timestamp: ts, Model: "m1",
	}}))

	ny, err := srv.activityRangeSummary(ctx, generateInsightRequest{
		Type: "daily_activity", DateFrom: "2026-06-15", DateTo: "2026-06-15",
		Timezone: "America/New_York",
	})
	require.NoError(t, err)
	require.NotNil(t, ny)
	assert.Equal(t, 1, ny.Sessions,
		"New York June-15 window covers the 02:00Z instant (22:00 local)")

	utc, err := srv.activityRangeSummary(ctx, generateInsightRequest{
		Type: "daily_activity", DateFrom: "2026-06-15", DateTo: "2026-06-15",
		Timezone: "UTC",
	})
	require.NoError(t, err)
	require.NotNil(t, utc)
	assert.Equal(t, 0, utc.Sessions,
		"UTC June-15 window ends at June 16 00:00Z, before the instant")
}
