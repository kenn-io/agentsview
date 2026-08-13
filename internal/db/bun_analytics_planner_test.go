package db

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"
	"go.kenn.io/agentsview/internal/activity"
)

func TestBunTopSessionsPlannerRanksActiveDurationAndLimitsInDatabase(t *testing.T) {
	database := testDB(t)
	base := time.Date(2026, 8, 4, 10, 0, 0, 0, time.UTC)
	for index := range 12 {
		id := fmt.Sprintf("ranked-%02d", index)
		start := base.Add(time.Duration(index) * time.Hour)
		end := start.Add(30 * time.Minute)
		require.NoError(t, database.UpsertSession(Session{
			ID: id, Project: "ranked", Machine: "host", Agent: "codex",
			CreatedAt: start.Format(time.RFC3339Nano),
			StartedAt: Ptr(start.Format(time.RFC3339Nano)),
			EndedAt:   Ptr(end.Format(time.RFC3339Nano)), MessageCount: 2,
		}))
		require.NoError(t, database.InsertMessages([]Message{
			{SessionID: id, Ordinal: 0, Role: "user", Timestamp: start.Format(time.RFC3339Nano)},
			{SessionID: id, Ordinal: 1, Role: "assistant", Timestamp: start.Add(time.Duration(index+1) * time.Second).Format(time.RFC3339Nano)},
		}))
	}

	got, err := database.GetAnalyticsTopSessions(
		t.Context(), AnalyticsFilter{Project: "ranked"}, "duration",
	)
	require.NoError(t, err)
	require.Len(t, got.Sessions, 10)
	assert.Equal(t, "ranked-11", got.Sessions[0].ID)
	assert.Equal(t, "ranked-02", got.Sessions[9].ID)
	assert.InDelta(t, 12.0/60.0, got.Sessions[0].ActiveDurationMin, 0.0001)
}

func TestBunActivityReportPlannerScopesExactOverlapAndEvents(t *testing.T) {
	database := testDB(t)
	for _, session := range []Session{
		{
			ID: "inside", Project: "activity-scope", Machine: "host", Agent: "codex",
			CreatedAt: "2026-08-04T10:00:00Z", StartedAt: Ptr("2026-08-04T10:00:00Z"),
			EndedAt: Ptr("2026-08-04T10:05:00Z"), MessageCount: 1,
		},
		{
			ID: "outside", Project: "activity-scope", Machine: "host", Agent: "codex",
			CreatedAt: "2026-08-03T10:00:00Z", StartedAt: Ptr("2026-08-03T10:00:00Z"),
			EndedAt: Ptr("2026-08-03T10:05:00Z"), MessageCount: 1,
		},
	} {
		require.NoError(t, database.UpsertSession(session))
	}
	require.NoError(t, database.InsertMessages([]Message{
		{SessionID: "inside", Ordinal: 0, Role: "assistant", Timestamp: "2026-08-04T10:01:00Z"},
		{SessionID: "outside", Ordinal: 0, Role: "assistant", Timestamp: "2026-08-03T10:01:00Z"},
	}))

	q := dayQuery(t, "2026-08-04", "UTC")
	var sessions []activity.SessionMeta
	var events []activity.ActivityEvent
	err := database.consistentView(t.Context(), func(store bun.IDB) error {
		var queryErr error
		sessions, events, queryErr = database.bunActivityReportScopeFrom(
			t.Context(), store, AnalyticsFilter{Project: "activity-scope"}, q,
		)
		return queryErr
	})
	require.NoError(t, err)
	require.Len(t, sessions, 1)
	assert.Equal(t, "inside", sessions[0].SessionID)
	require.Len(t, events, 1)
	assert.Equal(t, "inside", events[0].SessionID)
}
