//go:build fts5

package db

import (
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAnalyticsKeptTracksWritesAndCopies(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()
	insertSession(t, d, "session", "project", func(s *Session) {
		s.StartedAt = new("2024-06-01T09:00:00Z")
		s.MessageCount = 2
	})
	message := asstMsgAt("session", 0, "hello", "2024-06-01T09:00:00Z")
	message.ToolCalls = []ToolCall{{SessionID: "session", ToolName: "Read", Category: "Read"}}
	insertMessages(t, d, message)
	updateSignals(t, d, "session", SessionSignalUpdate{ToolRetryCount: 1, QualitySignals: QualitySignals{Version: CurrentQualitySignalVersion}})

	tools, err := d.GetAnalyticsTools(ctx, AnalyticsFilter{})
	require.NoError(t, err)
	assert.Equal(t, 1, tools.TotalCalls)
	require.Len(t, tools.ByCategory, 1)
	tools.ByCategory[0].Count = 99
	again, err := d.GetAnalyticsTools(ctx, AnalyticsFilter{})
	require.NoError(t, err)
	assert.Equal(t, 1, again.ByCategory[0].Count)

	signals, err := d.GetAnalyticsSignals(ctx, AnalyticsFilter{})
	require.NoError(t, err)
	assert.Equal(t, 1, signals.ToolHealth.TotalRetries)
	updateSignals(t, d, "session", SessionSignalUpdate{ToolRetryCount: 3, QualitySignals: QualitySignals{Version: CurrentQualitySignalVersion}})
	signals, err = d.GetAnalyticsSignals(ctx, AnalyticsFilter{})
	require.NoError(t, err)
	assert.Equal(t, 3, signals.ToolHealth.TotalRetries)

	_, err = d.getWriter().ExecContext(ctx, `UPDATE tool_calls SET tool_name = 'Bash', category = 'Bash' WHERE session_id = 'session'`)
	require.NoError(t, err)
	again, err = d.GetAnalyticsTools(ctx, AnalyticsFilter{})
	require.NoError(t, err)
	require.Len(t, again.ByCategory, 1)
	assert.Equal(t, "Bash", again.ByCategory[0].Category)

	require.NoError(t, d.ReplaceSessionMessages(ctx, "session", nil))
	again, err = d.GetAnalyticsTools(ctx, AnalyticsFilter{})
	require.NoError(t, err)
	assert.Zero(t, again.TotalCalls)
}

func TestAnalyticsKeptSharesLoads(t *testing.T) {
	d := testDB(t)
	insertSession(t, d, "session", "project")
	message := asstMsgAt("session", 0, "read", "2024-06-01T09:00:00Z")
	message.ToolCalls = []ToolCall{{SessionID: "session", ToolName: "Read", Category: "Read"}}
	insertMessages(t, d, message)
	var calls atomic.Int32
	started, release := make(chan struct{}), make(chan struct{})
	previous := analyticsQueryObserver
	analyticsQueryObserver = func(query string) {
		if strings.Contains(query, "FROM tool_calls") && calls.Add(1) == 1 {
			close(started)
			<-release
		}
	}
	t.Cleanup(func() { analyticsQueryObserver = previous })
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			result, err := d.GetAnalyticsTools(t.Context(), AnalyticsFilter{})
			assert.NoError(t, err)
			assert.Equal(t, 1, result.TotalCalls)
		})
	}
	<-started
	close(release)
	wg.Wait()
	_, err := d.GetAnalyticsTools(t.Context(), AnalyticsFilter{})
	require.NoError(t, err)
	assert.EqualValues(t, 1, calls.Load())
}

func TestAnalyticsKeptContentLengthRepair(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()
	insertSession(t, d, "session", "project", func(s *Session) {
		s.StartedAt = new("2024-06-01T09:00:00Z")
		s.MessageCount = 2
	})
	insertMessages(t, d,
		Message{SessionID: "session", Ordinal: 0, Role: "user", Content: "hello", Timestamp: "2024-06-01T09:00:00Z"},
		Message{SessionID: "session", Ordinal: 1, Role: "assistant", Content: "1234567890", ContentLength: 10, Timestamp: "2024-06-01T09:01:00Z"},
	)
	before, err := d.GetAnalyticsVelocity(ctx, AnalyticsFilter{})
	require.NoError(t, err)
	assert.Equal(t, float64(10), before.Overall.CharsPerActiveMin)
	_, err = d.getWriter().ExecContext(ctx, `UPDATE messages SET content_length = 20 WHERE session_id = 'session' AND ordinal = 1`)
	require.NoError(t, err)
	after, err := d.GetAnalyticsVelocity(ctx, AnalyticsFilter{})
	require.NoError(t, err)
	assert.Equal(t, float64(20), after.Overall.CharsPerActiveMin)
}

func TestAnalyticsKeptVelocityTracksMessageAndToolChanges(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()
	insertSession(t, d, "session", "project", func(s *Session) {
		s.StartedAt = new("2024-06-01T09:00:00Z")
		s.MessageCount = 2
	})
	user := Message{SessionID: "session", Ordinal: 0, Role: "user", Content: "hello", Timestamp: "2024-06-01T09:00:00Z"}
	assistant := Message{SessionID: "session", Ordinal: 1, Role: "assistant", Content: "reply", Timestamp: "2024-06-01T09:01:00Z"}
	insertMessages(t, d, user, assistant)
	before, err := d.GetAnalyticsVelocity(ctx, AnalyticsFilter{})
	require.NoError(t, err)
	assert.Equal(t, float64(60), before.Overall.FirstResponseSec.P50)
	assert.Zero(t, before.Overall.ToolCallsPerActiveMin)

	assistant.Timestamp = "2024-06-01T09:02:00Z"
	assistant.ToolCalls = []ToolCall{{SessionID: "session", ToolName: "Read", Category: "Read"}}
	require.NoError(t, d.ReplaceSessionMessages(ctx, "session", []Message{user, assistant}))
	after, err := d.GetAnalyticsVelocity(ctx, AnalyticsFilter{})
	require.NoError(t, err)
	assert.Equal(t, float64(120), after.Overall.FirstResponseSec.P50)
	assert.Equal(t, 0.5, after.Overall.ToolCallsPerActiveMin)
}

func TestAnalyticsKeptToolFactsAfterDialogueRepair(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()
	insertSession(t, d, "session", "project")
	message := asstMsgAt("session", 0, "read", "2024-06-01T09:00:00Z")
	message.ToolCalls = []ToolCall{{SessionID: "session", ToolName: "Read", Category: "Read"}}
	insertMessages(t, d, message)
	updateSignals(t, d, "session", SessionSignalUpdate{ToolObservations: []ToolObservation{{Outcome: "empty", Repeat: "none"}}, QualitySignals: QualitySignals{Version: CurrentQualitySignalVersion}})
	before, err := d.GetAnalyticsTools(ctx, AnalyticsFilter{})
	require.NoError(t, err)
	require.Len(t, before.ByTool, 1)
	assert.Equal(t, 1, before.ByTool[0].AnalyzedCalls)

	session, err := d.GetSession(ctx, "session")
	require.NoError(t, err)
	require.NotNil(t, session)
	messages, err := d.GetAllMessages(ctx, "session")
	require.NoError(t, err)
	require.Len(t, messages, 1)
	messages[0].DialogueText = new("repaired dialogue")
	messages[0].ToolCalls[0].ObservedOutcome = nil
	messages[0].ToolCalls[0].ObservedRepeat = nil
	result, err := d.WriteSessionBatchContext(ctx, []SessionBatchWrite{{Session: *session, Messages: messages, ReplaceMessages: true, SkipSignalUpdates: true}})
	require.NoError(t, err)
	require.Zero(t, result.FailedSessions)
	after, err := d.GetAnalyticsTools(ctx, AnalyticsFilter{})
	require.NoError(t, err)
	require.Len(t, after.ByTool, 1)
	assert.Equal(t, 1, after.ByTool[0].MissingCalls)
}

func TestAnalyticsKeptSelectionAndArchiveIdentity(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()
	insertSession(t, d, "session", "project", func(s *Session) { s.StartedAt = new("2024-06-01T09:00:00Z") })
	message := asstMsgAt("session", 0, "read", "2024-06-01T09:00:00Z")
	message.ToolCalls = []ToolCall{{SessionID: "session", ToolName: "Read", Category: "Read"}}
	insertMessages(t, d, message)
	var calls atomic.Int32
	previous := analyticsQueryObserver
	analyticsQueryObserver = func(query string) {
		if strings.Contains(query, "FROM tool_calls") {
			calls.Add(1)
		}
	}
	t.Cleanup(func() { analyticsQueryObserver = previous })
	filters := []AnalyticsFilter{
		{Timezone: "UTC"}, {Timezone: "America/New_York"},
		{Timezone: "UTC", Hour: new(9)}, {Timezone: "UTC", DayOfWeek: new(5)},
		{Timezone: "UTC", Model: "model-a"},
	}
	for _, filter := range filters {
		_, err := d.GetAnalyticsTools(ctx, filter)
		require.NoError(t, err)
		_, err = d.GetAnalyticsTools(ctx, filter)
		require.NoError(t, err)
	}
	// The model filter has no matching sessions and does not issue a tool query.
	assert.EqualValues(t, 4, calls.Load())
	_, err := d.getWriter().ExecContext(ctx, `UPDATE archive_metadata SET value = 'another-generation' WHERE key = ?`, archiveMetadataDatabaseIDKey)
	require.NoError(t, err)
	_, err = d.GetAnalyticsTools(ctx, filters[0])
	require.NoError(t, err)
	assert.EqualValues(t, 5, calls.Load())
	_, err = d.getWriter().ExecContext(ctx, `DELETE FROM archive_metadata WHERE key = ?`, archiveMetadataDatabaseIDKey)
	require.NoError(t, err)
	for range 2 {
		_, err = d.GetAnalyticsTools(ctx, filters[0])
		require.NoError(t, err)
	}
	assert.EqualValues(t, 7, calls.Load())
}

func TestAnalyticsKeptReadOnlyBypass(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()
	insertSession(t, d, "session", "project")
	message := asstMsgAt("session", 0, "read", "2024-06-01T09:00:00Z")
	message.ToolCalls = []ToolCall{{SessionID: "session", ToolName: "Read", Category: "Read"}}
	insertMessages(t, d, message)
	ro, err := OpenReadOnly(ctx, d.Path())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, ro.Close()) })
	before, err := ro.GetAnalyticsTools(ctx, AnalyticsFilter{})
	require.NoError(t, err)
	assert.Equal(t, "Read", before.ByCategory[0].Category)
	// Model an older writer that cannot maintain the new triggers.
	_, err = d.getWriter().ExecContext(ctx, `DROP TRIGGER trg_tool_calls_analytics_change_update`)
	require.NoError(t, err)
	_, err = d.getWriter().ExecContext(ctx, `UPDATE tool_calls SET tool_name = 'Bash', category = 'Bash' WHERE session_id = 'session'`)
	require.NoError(t, err)
	after, err := ro.GetAnalyticsTools(ctx, AnalyticsFilter{})
	require.NoError(t, err)
	assert.Equal(t, "Bash", after.ByCategory[0].Category)
}

func TestAnalyticsKeptTriggerUpgrade(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()
	insertSession(t, d, "session", "project")
	message := asstMsgAt("session", 0, "read", "2024-06-01T09:00:00Z")
	message.ToolCalls = []ToolCall{{SessionID: "session", ToolName: "Read", Category: "Read"}}
	insertMessages(t, d, message)
	_, err := d.getWriter().ExecContext(ctx, `DROP TRIGGER trg_tool_calls_analytics_change_update;
		CREATE TRIGGER trg_tool_calls_analytics_change_update AFTER UPDATE ON tool_calls
		BEGIN UPDATE archive_metadata SET value = CAST(value AS INTEGER) + 1 WHERE key = 'usage_change_revision'; END`)
	require.NoError(t, err)
	upgraded, err := OpenIsolated(ctx, d.Path())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, upgraded.Close()) })
	_, err = upgraded.GetAnalyticsTools(ctx, AnalyticsFilter{})
	require.NoError(t, err)
	usageBefore, err := upgraded.UsageChangeRevision(ctx)
	require.NoError(t, err)
	_, err = upgraded.getWriter().ExecContext(ctx, `UPDATE tool_calls SET tool_name = 'Bash', category = 'Bash' WHERE session_id = 'session'`)
	require.NoError(t, err)
	usageAfter, err := upgraded.UsageChangeRevision(ctx)
	require.NoError(t, err)
	assert.Equal(t, usageBefore, usageAfter)
	result, err := upgraded.GetAnalyticsTools(ctx, AnalyticsFilter{})
	require.NoError(t, err)
	require.Len(t, result.ByCategory, 1)
	assert.Equal(t, "Bash", result.ByCategory[0].Category)
}

func TestAnalyticsKeptTerminationClock(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		d := testDB(t)
		ctx := t.Context()
		now := time.Now().UTC().Format(time.RFC3339)
		insertSession(t, d, "session", "project", func(s *Session) {
			s.StartedAt = &now
			s.EndedAt = &now
		})
		message := asstMsgAt("session", 0, "read", now)
		message.ToolCalls = []ToolCall{{SessionID: "session", ToolName: "Read", Category: "Read"}}
		insertMessages(t, d, message)
		filter := AnalyticsFilter{Termination: "active"}
		before, err := d.GetAnalyticsTools(ctx, filter)
		require.NoError(t, err)
		assert.Equal(t, 1, before.TotalCalls)
		time.Sleep(11 * time.Minute)
		after, err := d.GetAnalyticsTools(ctx, filter)
		require.NoError(t, err)
		assert.Zero(t, after.TotalCalls)
	})
}
