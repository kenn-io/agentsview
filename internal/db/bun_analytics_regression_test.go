package db

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"
)

type replayingAnalyticsBackend struct {
	first, second bun.IDB
	attempts      int
}

func (*replayingAnalyticsBackend) Name() string { return "replaying-analytics" }

func (*replayingAnalyticsBackend) ReadOnly() bool { return true }

func (*replayingAnalyticsBackend) Capabilities() BackendCapabilities {
	return BackendCapabilities{}
}

func (*replayingAnalyticsBackend) SessionQueryDialect() QueryDialect {
	return SQLiteBunSessionQueryDialect()
}

func (*replayingAnalyticsBackend) SessionVersion(
	context.Context, bun.IDB, string,
) (int, int64, error) {
	return 0, 0, sql.ErrNoRows
}

func (b *replayingAnalyticsBackend) View(
	_ context.Context, callback func(bun.IDB) error,
) error {
	return callback(b.second)
}

func (b *replayingAnalyticsBackend) ConsistentView(
	_ context.Context, callback func(bun.IDB) error,
) error {
	b.attempts++
	if err := callback(b.first); err != nil {
		return err
	}
	b.attempts++
	return callback(b.second)
}

func (*replayingAnalyticsBackend) Update(
	context.Context, func(bun.IDB) error,
) error {
	return ErrReadOnly
}

func TestBunAnalyticsTerminationUsesActivityWindows(t *testing.T) {
	database := testDB(t)
	now := time.Now().UTC()
	for _, row := range []struct {
		id, status string
		age        time.Duration
	}{
		{id: "analytics-active-clean", status: "clean", age: 5 * time.Minute},
		{id: "analytics-active-flagged", status: "truncated", age: 5 * time.Minute},
		{id: "analytics-stale", status: "tool_call_pending", age: 30 * time.Minute},
		{id: "analytics-unclean", status: "truncated", age: 2 * time.Hour},
	} {
		ended := now.Add(-row.age).Format(time.RFC3339Nano)
		status := row.status
		require.NoError(t, database.UpsertSession(Session{
			ID: row.id, Project: "termination", Machine: "host", Agent: "codex",
			CreatedAt: ended, StartedAt: &ended, EndedAt: &ended,
			MessageCount: 1, UserMessageCount: 1, TerminationStatus: &status,
		}))
		require.NoError(t, database.InsertMessages([]Message{{
			SessionID: row.id, Ordinal: 0, Role: "assistant",
			Content: "done", ContentLength: 4, Timestamp: ended,
		}}))
	}

	for _, test := range []struct {
		filter string
		want   int
	}{
		{filter: "active", want: 2},
		{filter: "stale", want: 1},
		{filter: "unclean", want: 1},
	} {
		t.Run(test.filter, func(t *testing.T) {
			summary, err := database.GetAnalyticsSummary(t.Context(), AnalyticsFilter{
				Project: "termination", Termination: test.filter,
			})
			require.NoError(t, err)
			assert.Equal(t, test.want, summary.TotalSessions)
		})
	}
}

func TestBunAnalyticsToolsFallsBackToSessionTime(t *testing.T) {
	database := testDB(t)
	started := "2026-08-04T10:00:00Z"
	file := "fallback.go"
	require.NoError(t, database.UpsertSession(Session{
		ID: "analytics-tool-fallback", Project: "fallback", Machine: "host",
		Agent: "codex", CreatedAt: started, StartedAt: &started,
		MessageCount: 1, UserMessageCount: 1,
	}))
	require.NoError(t, database.InsertMessages([]Message{{
		SessionID: "analytics-tool-fallback", Ordinal: 0, Role: "assistant",
		Content: "edit", ContentLength: 4, HasToolUse: true,
		ToolCalls: []ToolCall{{
			ToolName: "Edit", Category: "Edit", FilePath: file,
		}},
	}}))
	hour := 10
	result, err := database.GetAnalyticsTools(t.Context(), AnalyticsFilter{
		Project: "fallback", Timezone: "UTC", Hour: &hour,
	})
	require.NoError(t, err)
	assert.Equal(t, 1, result.TotalCalls)
}

func TestBunAnalyticsSummaryReportsOnlyPresentRequestedModels(t *testing.T) {
	database := testDB(t)
	started := "2026-08-04T11:00:00Z"
	require.NoError(t, database.UpsertSession(Session{
		ID: "analytics-model-list", Project: "models", Machine: "host",
		Agent: "codex", CreatedAt: started, StartedAt: &started,
		MessageCount: 1, UserMessageCount: 1,
	}))
	require.NoError(t, database.InsertMessages([]Message{{
		SessionID: "analytics-model-list", Ordinal: 0, Role: "assistant",
		Content: "model", ContentLength: 5, Timestamp: started, Model: "model-a",
	}}))

	result, err := database.GetAnalyticsSummary(t.Context(), AnalyticsFilter{
		Project: "models", Model: "model-a,missing-model",
	})
	require.NoError(t, err)
	assert.Equal(t, []string{"model-a"}, result.Models)
}

func TestBunAnalyticsProjectsPublishesOnlyAcceptedReplay(t *testing.T) {
	first := testDB(t)
	second := testDB(t)
	for database, project := range map[*DB]string{first: "rejected", second: "accepted"} {
		started := "2026-08-04T12:00:00Z"
		id := "analytics-" + project
		require.NoError(t, database.UpsertSession(Session{
			ID: id, Project: project, Machine: "host", Agent: "codex",
			CreatedAt: started, StartedAt: &started,
			MessageCount: 1, UserMessageCount: 1,
		}))
		require.NoError(t, database.InsertMessages([]Message{{
			SessionID: id, Ordinal: 0, Role: "assistant",
			Content: project, ContentLength: len(project), Timestamp: started,
		}}))
	}
	backend := &replayingAnalyticsBackend{
		first: first.bunReader, second: second.bunReader,
	}
	result, err := NewBunStore(backend).GetAnalyticsProjects(
		t.Context(), AnalyticsFilter{},
	)
	require.NoError(t, err)
	assert.Equal(t, 2, backend.attempts)
	require.Len(t, result.Projects, 1)
	assert.Equal(t, "accepted", result.Projects[0].Name)
}

func TestBunRecentEditsHydratesOnlyRequestedGroups(t *testing.T) {
	database := testDB(t)
	started := "2026-08-04T13:00:00Z"
	require.NoError(t, database.UpsertSession(Session{
		ID: "bounded-edits", Project: "edits", Machine: "host", Agent: "codex",
		CreatedAt: started, StartedAt: &started,
		MessageCount: 1, UserMessageCount: 1,
	}))
	calls := make([]ToolCall, 30)
	for index := range calls {
		file := fmt.Sprintf("file-%02d.go", index)
		calls[index] = ToolCall{
			CallIndex: index, ToolName: "Edit", Category: "Edit", FilePath: file,
		}
	}
	require.NoError(t, database.InsertMessages([]Message{{
		SessionID: "bounded-edits", Ordinal: 0, Role: "assistant",
		Content: "edits", ContentLength: 5, Timestamp: started,
		HasToolUse: true, ToolCalls: calls,
	}}))

	hook := new(countingQueryHook)
	store := NewBunStore(&sessionContractBackend{
		store: database.bunReader.WithQueryHook(hook),
	})
	result, err := store.RecentEdits(t.Context(), RecentEditsParams{
		Project: "edits", Limit: 1, MaxEditsPerFile: 2,
	})
	require.NoError(t, err)
	require.Len(t, result.Files, 1)
	assert.True(t, result.HasMore)
	assert.Equal(t, 1, hook.selects)
	require.Len(t, hook.queries, 1)
	assert.NotContains(t, hook.queries[0], "input_json")
	assert.NotContains(t, hook.queries[0], "result_content")
	assert.NotContains(t, hook.queries[0], "messages.content")
}

func TestBunContentAnalyticsStreamsAcrossSessionBatches(t *testing.T) {
	database := testDB(t)
	started := "2026-08-04T12:00:00Z"
	messages := make([]Message, 0, 33)
	for index := range 33 {
		id := fmt.Sprintf("content-stream-%02d", index)
		require.NoError(t, database.UpsertSession(Session{
			ID: id, Project: "content-stream", Machine: "host", Agent: "codex",
			CreatedAt: started, StartedAt: &started,
			MessageCount: 1, UserMessageCount: 1,
		}))
		content := "this is broken again seam"
		messages = append(messages, Message{
			SessionID: id, Ordinal: 0, Role: "user", Content: content,
			ContentLength: len(content), Timestamp: started,
		})
	}
	require.NoError(t, database.InsertMessages(messages))
	_, err := database.getWriter().Exec(
		"UPDATE sessions SET quality_signal_version = ?",
		CurrentQualitySignalVersion,
	)
	require.NoError(t, err)

	hook := new(countingQueryHook)
	store := NewBunStore(&sessionContractBackend{
		store: database.bunReader.WithQueryHook(hook),
	})
	terms, err := ParseTrendTerms([]string{"seam"})
	require.NoError(t, err)
	trends, err := store.GetTrendsTerms(t.Context(), AnalyticsFilter{
		Project: "content-stream", From: "2026-08-04", To: "2026-08-04",
		Timezone: "UTC",
	}, terms, "day")
	require.NoError(t, err)
	assert.Equal(t, 33, trends.MessageCount)
	require.Len(t, trends.Series, 1)
	assert.Equal(t, 33, trends.Series[0].Total)
	trendContentQueries := bunContentSelects(hook.queries)
	require.Len(t, trendContentQueries, 2)
	for _, query := range trendContentQueries {
		assert.NotContains(t, query, "has_thinking")
		assert.NotContains(t, query, "has_tool_use")
		assert.NotContains(t, query, "content_length")
		assert.NotContains(t, query, "output_tokens")
		assert.NotContains(t, query, "is_sidechain")
	}

	hook.queries = nil
	signalResult, err := store.GetAnalyticsSignals(t.Context(), AnalyticsFilter{
		Project: "content-stream", From: "2026-08-04", To: "2026-08-04",
		Timezone: "UTC",
	})
	require.NoError(t, err)
	assert.Equal(t, 33, signalResult.QualityHealth.Totals.FrustrationMarkerCount)
	assert.Equal(t, 33,
		signalResult.QualityHealth.SessionsWithSignal.FrustrationMarkerCount)
	signalContentQueries := bunContentSelects(hook.queries)
	require.Len(t, signalContentQueries, 2)
	for _, query := range signalContentQueries {
		assert.NotContains(t, query, "ordinal")
		assert.NotContains(t, query, "model")
		assert.NotContains(t, query, "timestamp")
		assert.NotContains(t, query, "has_tool_use")
	}
}

func bunContentSelects(queries []string) []string {
	var contentQueries []string
	for _, query := range queries {
		normalized := strings.ToLower(query)
		if strings.Contains(normalized, `from "messages"`) &&
			strings.Contains(normalized, `"content"`) {
			contentQueries = append(contentQueries, normalized)
		}
	}
	return contentQueries
}
