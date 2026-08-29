//go:build fts5

package db

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCaptureUsageQueryBoundedCandidatesAndMetadata(t *testing.T) {
	database := usageCandidateFixture(t)
	filter := UsageFilter{
		From: "2026-08-10", To: "2026-08-10", Timezone: "UTC",
		Project: "keep", Model: "model-x",
	}

	snapshot, err := database.captureUsageQuery(
		context.Background(), filter, usageQueryKindToken,
	)
	require.NoError(t, err)
	require.NoError(t, snapshot.Close())

	assert.NotEmpty(t, snapshot.DatabaseID)
	assert.Equal(t, int64(1), snapshot.CursorHighWater)
	assert.Equal(t, []usageQueryInterval{{
		FromMillis: 1786320000000, ToMillis: 1786406400000,
		FromLocalDate: "2026-08-10", ToLocalDate: "2026-08-10",
	}}, snapshot.Intervals)

	assert.Equal(t, []string{
		"blank-message", "event", "filtered-competitor", "inside-message",
		"model-filtered",
	}, usageQuerySessionIDs(snapshot.Sessions))
	byID := make(map[string]usageQuerySession, len(snapshot.Sessions))
	for _, session := range snapshot.Sessions {
		byID[session.ID] = session
	}
	assert.Equal(t, usageQuerySession{
		ID: "inside-message", Project: "keep", Machine: "machine-a",
		Agent: "claude", GitBranch: "main",
		CreatedAt:   "2026-08-01T00:00:00.000Z",
		StartedAt:   "2026-08-01T00:00:00Z",
		DisplayName: "Inside", StartedAtMillis: new(int64(1785542400000)),
		StartedAtNanos:   new(int64(0)),
		UserMessageCount: 2, PassesFilter: true,
	}, byID["inside-message"])
	assert.False(t, byID["filtered-competitor"].PassesFilter,
		"session filters must be recorded, not applied to candidate discovery")
	assert.True(t, byID["model-filtered"].PassesFilter,
		"model filters apply to facts after ranking, not session metadata")

	assert.Equal(t, usageQuerySessionIDs(snapshot.Sessions),
		usageSourceVersionIDs(snapshot.Versions))
	versionByID := make(map[string]usageSourceVersion, len(snapshot.Versions))
	for _, version := range snapshot.Versions {
		versionByID[version.SessionID] = version
		assert.NotEmpty(t, version.SyncMarker)
		assert.NotEmpty(t, version.TranscriptRevision)
	}
	assert.Equal(t,
		"false|0|7:session|7:model-x|0:|1|2|0|0|0|false|0|0:|0:|20:2026-08-10T12:00:00Z|2:e1;",
		versionByID["event"].UsageEventFingerprint)
	assert.Empty(t, versionByID["inside-message"].UsageEventFingerprint)

	// The returned value owns no live archive transaction.
	require.NoError(t, database.RenameSession("inside-message", new("renamed")))
}

func TestCaptureUsageQueryRelaxedAndAllHistoryCandidates(t *testing.T) {
	database := usageCandidateFixture(t)
	filter := UsageFilter{
		From: "2026-08-10", To: "2026-08-10", Timezone: "UTC",
	}

	relaxed, err := database.captureUsageQuery(
		context.Background(), filter, usageQueryKindActivity,
	)
	require.NoError(t, err)
	assert.Equal(t, []string{
		"blank-message", "event", "filtered-competitor", "inside-message",
		"model-filtered", "relaxed",
	}, usageQuerySessionIDs(relaxed.Sessions))

	allHistory, err := database.captureUsageQuery(
		context.Background(), UsageFilter{}, usageQueryKindToken,
	)
	require.NoError(t, err)
	assert.Equal(t, []string{
		"all-empty", "blank-message", "event", "filtered-competitor",
		"inside-message", "model-filtered", "outside", "relaxed", "user-only",
	}, usageQuerySessionIDs(allHistory.Sessions))
	assert.Empty(t, allHistory.Intervals)
}

func TestUsageCandidateDiscoveryQueryPlans(t *testing.T) {
	database := usageCandidateFixture(t)
	bounds := usageBoundsForFilter(UsageFilter{
		From: "2026-08-10", To: "2026-08-10", Timezone: "UTC",
	})

	tokenSQL, tokenArgs := usageCandidateDiscoverySQL(bounds, usageQueryKindToken)
	tokenPlan := explainUsageCandidatePlan(t, database, tokenSQL, tokenArgs)
	assert.Contains(t, tokenPlan, "idx_messages_usage_timestamp")

	activitySQL, activityArgs := usageCandidateDiscoverySQL(
		bounds, usageQueryKindActivity,
	)
	activityPlan := explainUsageCandidatePlan(t, database, activitySQL, activityArgs)
	assert.Contains(t, activityPlan,
		"SEARCH m USING COVERING INDEX idx_messages_activity_timestamp (timestamp>")
}

func TestUsageCandidateDiscoveryCoversOppositeDateLineOffsets(t *testing.T) {
	database := testDB(t)
	started := "2026-08-08T23:00:00-12:00"
	insertSession(t, database, "date-line", "project", func(session *Session) {
		session.StartedAt = &started
	})
	require.NoError(t, database.InsertMessages([]Message{{
		SessionID: "date-line", Ordinal: 0, Role: "assistant",
		Timestamp: started, Model: "model",
		TokenUsage: json.RawMessage(`{"input_tokens":1}`),
	}}))
	snapshot, err := database.captureUsageQuery(context.Background(), UsageFilter{
		From: "2026-08-10", To: "2026-08-10", Timezone: "Pacific/Kiritimati",
	}, usageQueryKindToken)
	require.NoError(t, err)
	assert.Equal(t, []string{"date-line"}, usageQuerySessionIDs(snapshot.Sessions))
}

func usageCandidateFixture(t *testing.T) *DB {
	t.Helper()
	database := testDB(t)
	seedSession := func(
		id, project, started string, userMessages int, name string,
	) {
		t.Helper()
		insertSession(t, database, id, project, func(session *Session) {
			session.Machine = "machine-a"
			session.Agent = "claude"
			session.GitBranch = "main"
			session.StartedAt = &started
			session.UserMessageCount = userMessages
			session.SessionName = &name
		})
		_, err := database.getWriter().Exec(
			`UPDATE sessions SET created_at = '2026-08-01T00:00:00.000Z' WHERE id = ?`, id)
		require.NoError(t, err)
	}
	tokenMessage := func(id, timestamp, model string) Message {
		return Message{
			SessionID: id, Ordinal: 0, Role: "assistant", Timestamp: timestamp,
			Model: model, TokenUsage: json.RawMessage(`{"input_tokens":1}`),
			Content: "answer", ContentLength: 6,
		}
	}

	seedSession("inside-message", "keep", "2026-08-01T00:00:00Z", 2, "Inside")
	insertMessages(t, database,
		tokenMessage("inside-message", "2026-08-10T10:00:00Z", "model-x"))
	seedSession("blank-message", "keep", "2026-08-10T09:00:00Z", 2, "Blank")
	insertMessages(t, database, tokenMessage("blank-message", "", "model-x"))
	seedSession("event", "keep", "2026-08-01T00:00:00Z", 2, "Event")
	require.NoError(t, database.ReplaceSessionUsageEvents("event", []UsageEvent{{
		Source: "session", Model: "model-x", InputTokens: 1, OutputTokens: 2,
		OccurredAt: "2026-08-10T12:00:00Z", DedupKey: "e1",
	}}))
	seedSession("filtered-competitor", "drop", "2026-08-01T00:00:00Z", 2, "Drop")
	insertMessages(t, database,
		tokenMessage("filtered-competitor", "2026-08-10T11:00:00Z", "other-model"))
	seedSession("model-filtered", "keep", "2026-08-01T00:00:00Z", 2, "Model")
	insertMessages(t, database,
		tokenMessage("model-filtered", "2026-08-10T11:30:00Z", "other-model"))
	seedSession("outside", "keep", "2026-08-01T00:00:00Z", 2, "Outside")
	insertMessages(t, database,
		tokenMessage("outside", "2026-08-20T10:00:00Z", "model-x"))
	seedSession("relaxed", "keep", "2026-08-01T00:00:00Z", 2, "Relaxed")
	insertMessages(t, database, Message{
		SessionID: "relaxed", Ordinal: 0, Role: "assistant",
		Timestamp: "2026-08-10T13:00:00Z", Content: "answer", ContentLength: 6,
	})
	seedSession("user-only", "keep", "2026-08-10T08:00:00Z", 1, "User")
	insertMessages(t, database, Message{
		SessionID: "user-only", Ordinal: 0, Role: "user",
		Timestamp: "2026-08-10T08:00:00Z", Content: "question", ContentLength: 8,
	})
	seedSession("all-empty", "keep", "2026-08-10T07:00:00Z", 0, "Empty")
	seedSession("deleted", "keep", "2026-08-10T06:00:00Z", 2, "Deleted")
	insertMessages(t, database,
		tokenMessage("deleted", "2026-08-10T06:00:00Z", "model-x"))
	_, err := database.getWriter().Exec(
		`UPDATE sessions SET deleted_at = '2026-08-10T14:00:00Z' WHERE id = 'deleted'`)
	require.NoError(t, err)

	require.NoError(t, database.InsertCursorUsageEvents([]CursorUsageEvent{{
		OccurredAt: "2026-08-10T15:00:00Z", Model: "cursor-model", DedupKey: "cursor-1",
	}}))
	return database
}

func usageQuerySessionIDs(sessions []usageQuerySession) []string {
	ids := make([]string, len(sessions))
	for i, session := range sessions {
		ids[i] = session.ID
	}
	return ids
}

func usageSourceVersionIDs(versions []usageSourceVersion) []string {
	ids := make([]string, len(versions))
	for i, version := range versions {
		ids[i] = version.SessionID
	}
	return ids
}

func explainUsageCandidatePlan(
	t *testing.T, database *DB, query string, args []any,
) string {
	t.Helper()
	rows, err := database.getReader().Query("EXPLAIN QUERY PLAN "+query, args...)
	require.NoError(t, err)
	defer rows.Close()
	var details []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		require.NoError(t, rows.Scan(&id, &parent, &unused, &detail))
		details = append(details, detail)
	}
	require.NoError(t, rows.Err())
	sort.Strings(details)
	return strings.Join(details, "\n")
}
