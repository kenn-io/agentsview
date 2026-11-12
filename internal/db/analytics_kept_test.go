//go:build fts5

package db

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAnalyticsKeptPreservesMalformedToolName(t *testing.T) {
	d := testDB(t)
	insertSession(t, d, "session", "project")
	message := asstMsgAt("session", 0, "read", "2024-06-01T09:00:00Z")
	message.ToolCalls = []ToolCall{{SessionID: "session", ToolName: "bad\xfftool", Category: "Other"}}
	insertMessages(t, d, message)
	for range 2 {
		result, err := d.GetAnalyticsTools(t.Context(), AnalyticsFilter{})
		require.NoError(t, err)
		assert.Equal(t, 1, result.TotalCalls)
		require.Len(t, result.ByTool, 1)
		assert.Equal(t, "bad\xfftool", result.ByTool[0].ToolName)
	}
}

func TestAnalyticsKeptCopiesNestedValues(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()
	filter := AnalyticsFilter{}
	tools := ToolsAnalyticsResponse{
		ByCategory: []ToolCategoryCount{{Category: "Read"}},
		ByAgent:    []ToolAgentBreakdown{{Categories: []ToolCategoryCount{{Category: "Read"}}}},
		ByTool:     []ToolUsageAnalysis{{EmptyRate: new(0.1), RepeatRate: new(0.2), RecoveryRate: new(0.3)}},
		Trend:      []ToolTrendEntry{{ByCat: map[string]int{"Read": 1}}},
	}
	loadTools := func(context.Context, AnalyticsFilter) (ToolsAnalyticsResponse, error) { return tools, nil }
	gotTools, err := keptAnalytics(ctx, d, "tools", filter, loadTools)
	require.NoError(t, err)
	gotTools.ByCategory[0].Category = "changed"
	gotTools.ByAgent[0].Categories[0].Category = "changed"
	*gotTools.ByTool[0].EmptyRate = 9
	*gotTools.ByTool[0].RepeatRate = 9
	*gotTools.ByTool[0].RecoveryRate = 9
	gotTools.Trend[0].ByCat["Read"] = 9
	againTools, err := keptAnalytics(ctx, d, "tools", filter, loadTools)
	require.NoError(t, err)
	assert.Equal(t, "Read", againTools.ByCategory[0].Category)
	assert.Equal(t, "Read", againTools.ByAgent[0].Categories[0].Category)
	assert.Equal(t, 0.1, *againTools.ByTool[0].EmptyRate)
	assert.Equal(t, 0.2, *againTools.ByTool[0].RepeatRate)
	assert.Equal(t, 0.3, *againTools.ByTool[0].RecoveryRate)
	assert.Equal(t, 1, againTools.Trend[0].ByCat["Read"])

	velocity := VelocityResponse{ByAgent: []VelocityBreakdown{{Label: "agent"}}, ByComplexity: []VelocityBreakdown{{Label: "simple"}}}
	loadVelocity := func(context.Context, AnalyticsFilter) (VelocityResponse, error) { return velocity, nil }
	gotVelocity, err := keptAnalytics(ctx, d, "velocity", filter, loadVelocity)
	require.NoError(t, err)
	gotVelocity.ByAgent[0].Label = "changed"
	gotVelocity.ByComplexity[0].Label = "changed"
	againVelocity, err := keptAnalytics(ctx, d, "velocity", filter, loadVelocity)
	require.NoError(t, err)
	assert.Equal(t, "agent", againVelocity.ByAgent[0].Label)
	assert.Equal(t, "simple", againVelocity.ByComplexity[0].Label)

	signals := SignalsAnalyticsResponse{
		GradeDistribution: map[string]int{"A": 1}, OutcomeDistribution: map[string]int{"done": 1}, OutcomeConfidenceDistribution: map[string]int{"high": 1},
		AvgHealthScore: new(1.0), ContextHealth: SignalsContextHealth{AvgContextPressure: new(2.0)},
		Trend: []SignalsTrendBucket{{AvgHealthScore: new(3.0)}}, ByAgent: []SignalsAgentRow{{AvgHealthScore: new(4.0)}}, ByProject: []SignalsProjectRow{{AvgHealthScore: new(5.0)}},
		Calibration: map[string]SignalCalibration{"signal": {IncompleteLift: new(6.0), AvgScoreDelta: new(7.0)}},
	}
	loadSignals := func(context.Context, AnalyticsFilter) (SignalsAnalyticsResponse, error) { return signals, nil }
	gotSignals, err := keptAnalytics(ctx, d, "signals", filter, loadSignals)
	require.NoError(t, err)
	gotSignals.GradeDistribution["A"] = 9
	gotSignals.OutcomeDistribution["done"] = 9
	gotSignals.OutcomeConfidenceDistribution["high"] = 9
	*gotSignals.AvgHealthScore = 9
	*gotSignals.ContextHealth.AvgContextPressure = 9
	*gotSignals.Trend[0].AvgHealthScore = 9
	*gotSignals.ByAgent[0].AvgHealthScore = 9
	*gotSignals.ByProject[0].AvgHealthScore = 9
	calibration := gotSignals.Calibration["signal"]
	*calibration.IncompleteLift = 9
	*calibration.AvgScoreDelta = 9
	gotSignals.Calibration["other"] = calibration
	againSignals, err := keptAnalytics(ctx, d, "signals", filter, loadSignals)
	require.NoError(t, err)
	assert.Equal(t, 1, againSignals.GradeDistribution["A"])
	assert.Equal(t, 1, againSignals.OutcomeDistribution["done"])
	assert.Equal(t, 1, againSignals.OutcomeConfidenceDistribution["high"])
	assert.Equal(t, 1.0, *againSignals.AvgHealthScore)
	assert.Equal(t, 2.0, *againSignals.ContextHealth.AvgContextPressure)
	assert.Equal(t, 3.0, *againSignals.Trend[0].AvgHealthScore)
	assert.Equal(t, 4.0, *againSignals.ByAgent[0].AvgHealthScore)
	assert.Equal(t, 5.0, *againSignals.ByProject[0].AvgHealthScore)
	assert.Equal(t, 6.0, *againSignals.Calibration["signal"].IncompleteLift)
	assert.Equal(t, 7.0, *againSignals.Calibration["signal"].AvgScoreDelta)
	assert.NotContains(t, againSignals.Calibration, "other")

	empty, err := keptAnalytics(ctx, d, "empty-tools", filter, func(context.Context, AnalyticsFilter) (ToolsAnalyticsResponse, error) {
		return ToolsAnalyticsResponse{ByAgent: []ToolAgentBreakdown{}, Trend: []ToolTrendEntry{{ByCat: map[string]int{}}}}, nil
	})
	require.NoError(t, err)
	assert.Nil(t, empty.ByCategory)
	assert.NotNil(t, empty.ByAgent)
	assert.Empty(t, empty.ByAgent)
	assert.NotNil(t, empty.Trend[0].ByCat)
	assert.Empty(t, empty.Trend[0].ByCat)
}

func TestAnalyticsKeptTracksWrites(t *testing.T) {
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

	signals, err := d.GetAnalyticsSignals(ctx, AnalyticsFilter{})
	require.NoError(t, err)
	assert.Equal(t, 1, signals.ToolHealth.TotalRetries)
	updateSignals(t, d, "session", SessionSignalUpdate{ToolRetryCount: 3, QualitySignals: QualitySignals{Version: CurrentQualitySignalVersion}})
	signals, err = d.GetAnalyticsSignals(ctx, AnalyticsFilter{})
	require.NoError(t, err)
	assert.Equal(t, 3, signals.ToolHealth.TotalRetries)

	_, err = d.getWriter().ExecContext(ctx, `UPDATE tool_calls SET tool_name = 'Bash', category = 'Bash' WHERE session_id = 'session'`)
	require.NoError(t, err)
	again, err := d.GetAnalyticsTools(ctx, AnalyticsFilter{})
	require.NoError(t, err)
	require.Len(t, again.ByCategory, 1)
	assert.Equal(t, "Bash", again.ByCategory[0].Category)
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
	message.Model = "model-a"
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
	assert.EqualValues(t, 5, calls.Load())
	_, err := d.getWriter().ExecContext(ctx, `UPDATE archive_metadata SET value = 'another-generation' WHERE key = ?`, archiveMetadataDatabaseIDKey)
	require.NoError(t, err)
	_, err = d.GetAnalyticsTools(ctx, filters[0])
	require.NoError(t, err)
	assert.EqualValues(t, 6, calls.Load())
	_, err = d.getWriter().ExecContext(ctx, `DELETE FROM archive_metadata WHERE key = ?`, archiveMetadataDatabaseIDKey)
	require.NoError(t, err)
	for range 2 {
		_, err = d.GetAnalyticsTools(ctx, filters[0])
		require.NoError(t, err)
	}
	assert.EqualValues(t, 8, calls.Load())
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
		DELETE FROM archive_metadata WHERE key = 'analytics_change_revision'`)
	require.NoError(t, err)
	upgraded, err := OpenIsolated(ctx, d.Path())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, upgraded.Close()) })
	_, err = upgraded.GetAnalyticsTools(ctx, AnalyticsFilter{})
	require.NoError(t, err)
	_, err = upgraded.getWriter().ExecContext(ctx, `UPDATE tool_calls SET tool_name = 'Bash', category = 'Bash' WHERE session_id = 'session'`)
	require.NoError(t, err)
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
