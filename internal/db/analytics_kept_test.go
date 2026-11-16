//go:build fts5

package db

import (
	"context"
	"os"
	"path/filepath"
	"strings"
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

func TestAnalyticsKeptAppendUsesTranscriptRevision(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()
	insertSession(t, d, "session", "project")
	before, err := d.GetAnalyticsTools(ctx, AnalyticsFilter{})
	require.NoError(t, err)
	assert.Zero(t, before.TotalCalls)
	message := asstMsgAt("session", 0, "read", "2024-06-01T09:00:00Z")
	message.ToolCalls = []ToolCall{{SessionID: "session", ToolName: "Read", Category: "Read"}, {SessionID: "session", ToolName: "Bash", Category: "Bash"}}
	insertMessages(t, d, message)
	after, err := d.GetAnalyticsTools(ctx, AnalyticsFilter{})
	require.NoError(t, err)
	assert.Equal(t, 2, after.TotalCalls)
}

func TestAnalyticsKeptTracksWrites(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()
	insertSession(t, d, "session", "project", func(s *Session) {
		s.StartedAt = new("2024-06-01T09:00:00Z")
		s.MessageCount = 1
	})
	message := asstMsgAt("session", 0, "hello", "2024-06-01T09:00:00Z")
	insertMessages(t, d, message)
	updateSignals(t, d, "session", SessionSignalUpdate{ToolRetryCount: 1, QualitySignals: QualitySignals{Version: CurrentQualitySignalVersion}})

	signals, err := d.GetAnalyticsSignals(ctx, AnalyticsFilter{})
	require.NoError(t, err)
	assert.Equal(t, 1, signals.ToolHealth.TotalRetries)
	updateSignals(t, d, "session", SessionSignalUpdate{ToolRetryCount: 3, QualitySignals: QualitySignals{Version: CurrentQualitySignalVersion}})
	signals, err = d.GetAnalyticsSignals(ctx, AnalyticsFilter{})
	require.NoError(t, err)
	assert.Equal(t, 3, signals.ToolHealth.TotalRetries)
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
	assert.InDelta(t, 10, before.Overall.CharsPerActiveMin, 0)
	_, err = d.getWriter().ExecContext(ctx, `UPDATE messages SET content_length = 20 WHERE session_id = 'session' AND ordinal = 1`)
	require.NoError(t, err)
	after, err := d.GetAnalyticsVelocity(ctx, AnalyticsFilter{})
	require.NoError(t, err)
	assert.InDelta(t, 20, after.Overall.CharsPerActiveMin, 0)
}

func TestAnalyticsKeptContentLengthReplacement(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()
	insertSession(t, d, "session", "project", func(s *Session) {
		s.StartedAt = new("2024-06-01T09:00:00Z")
		s.MessageCount = 2
	})
	messages := []Message{
		{SessionID: "session", Ordinal: 0, Role: "user", Content: "hello", ContentLength: 5, Timestamp: "2024-06-01T09:00:00Z"},
		{SessionID: "session", Ordinal: 1, Role: "assistant", Content: "1234567890", ContentLength: 10, Timestamp: "2024-06-01T09:01:00Z"},
	}
	insertMessages(t, d, messages...)
	updateSignals(t, d, "session", SessionSignalUpdate{})
	before, err := d.GetAnalyticsVelocity(ctx, AnalyticsFilter{})
	require.NoError(t, err)
	assert.InDelta(t, 10, before.Overall.CharsPerActiveMin, 0)
	messages[0].ContentLength = 6
	messages[1].ContentLength = 20
	require.NoError(t, d.ReplaceSessionContent(ctx, "session", messages, SessionSignalUpdate{}, nil))
	after, err := d.GetAnalyticsVelocity(ctx, AnalyticsFilter{})
	require.NoError(t, err)
	assert.InDelta(t, 20, after.Overall.CharsPerActiveMin, 0)
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
		{Timezone: "UTC"},
		{Timezone: "America/New_York"},
		{Timezone: "UTC", Hour: new(9)},
		{Timezone: "UTC", DayOfWeek: new(5)},
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

func TestTrendsKeptTracksContentAndSelection(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()
	insertSession(t, d, "session", "project", func(s *Session) { s.StartedAt = new("2024-06-01T00:30:00Z"); s.MessageCount = 1 })
	messages := []Message{{SessionID: "session", Ordinal: 0, Role: "user", Content: "alpha bravo", ContentLength: 11, Timestamp: "2024-06-01T00:30:00Z"}}
	insertMessages(t, d, messages...)
	var sized atomic.Int64
	d.analyticsKept.Size = func(any) int64 { sized.Add(1); return 1 }
	f := AnalyticsFilter{From: "2024-05-31", To: "2024-06-01", Timezone: "UTC", Project: "project"}
	a := TrendTermInput{Term: "alpha", Variants: []string{"alpha"}, Matchers: []string{"alpha"}}
	b := TrendTermInput{Term: "bravo", Variants: []string{"bravo"}, Matchers: []string{"bravo"}}
	terms := []TrendTermInput{a, b}
	read := func() TrendsTermsResponse {
		result, err := d.GetTrendsTerms(ctx, f, terms, "week")
		require.NoError(t, err)
		return result
	}
	first := read()
	require.Equal(t, 1, first.Series[0].Total)
	require.EqualValues(t, 1, sized.Load())
	require.Equal(t, first, read())
	require.EqualValues(t, 1, sized.Load())
	reversed, err := d.GetTrendsTerms(ctx, f, []TrendTermInput{b, a}, "week")
	require.NoError(t, err)
	require.Equal(t, "bravo", reversed.Series[0].Term)
	day, err := d.GetTrendsTerms(ctx, f, terms, "day")
	require.NoError(t, err)
	require.Equal(t, "day", day.Granularity)
	require.Equal(t, 1, day.Series[0].Points[1].Count)
	a.Variants = []string{"different"}
	variant, err := d.GetTrendsTerms(ctx, f, []TrendTermInput{a, b}, "week")
	require.NoError(t, err)
	require.Equal(t, []string{"different"}, variant.Series[0].Variants)
	a.Matchers = []string{"absent"}
	matcher, err := d.GetTrendsTerms(ctx, f, []TrendTermInput{a, b}, "week")
	require.NoError(t, err)
	require.Zero(t, matcher.Series[0].Total)
	messages[0].Content = "bravo bravo"
	require.NoError(t, d.ReplaceSessionContent(ctx, "session", messages, SessionSignalUpdate{}, nil))
	after := read()
	require.Zero(t, after.Series[0].Total)
	require.Equal(t, 1, after.MessageCount)
	fresh, err := d.trendsTerms(ctx, f, terms, "week")
	require.NoError(t, err)
	require.Equal(t, fresh, after)
	session, err := d.GetSession(ctx, "session")
	require.NoError(t, err)
	session.Project = "other"
	require.NoError(t, d.UpsertSession(ctx, *session))
	require.Zero(t, read().MessageCount)
}

func TestAnalyticsKeptScoreOnly(t *testing.T) {
	t.Setenv("CACHE_DIRECTORY", t.TempDir())
	d := testDB(t)
	ctx := t.Context()
	insertSession(t, d, "session", "project", func(s *Session) {
		s.StartedAt = new("2024-06-01T09:00:00Z")
	})
	message := asstMsgAt("session", 0, "read", "2024-06-01T09:00:00Z")
	message.ToolCalls = []ToolCall{{SessionID: "session", ToolName: "Read", Category: "Read"}}
	insertMessages(t, d, message)
	updateSignals(t, d, "session", SessionSignalUpdate{HealthScore: new(30)})
	f := AnalyticsFilter{Timezone: "UTC"}
	read := func(store *DB, counts *[3]int) SignalsAnalyticsResponse {
		tools, err := keptAnalytics(ctx, store, "tools", f, func(ctx context.Context, f AnalyticsFilter) (ToolsAnalyticsResponse, error) {
			counts[0]++
			return store.analyticsTools(ctx, f)
		})
		require.NoError(t, err)
		require.Equal(t, 1, tools.TotalCalls)
		_, err = keptAnalytics(ctx, store, "velocity", f, func(ctx context.Context, f AnalyticsFilter) (VelocityResponse, error) {
			counts[1]++
			return store.analyticsVelocity(ctx, f)
		})
		require.NoError(t, err)
		signals, err := keptAnalytics(ctx, store, "signals", f, func(ctx context.Context, f AnalyticsFilter) (SignalsAnalyticsResponse, error) {
			counts[2]++
			return store.analyticsSignals(ctx, f)
		})
		require.NoError(t, err)
		return signals
	}
	counts := [3]int{}
	first := read(d, &counts)
	require.NotNil(t, first.AvgHealthScore)
	require.InDelta(t, 30, *first.AvgHealthScore, 0)
	require.NoError(t, d.UpdateSessionSignals(ctx, "session", SessionSignalUpdate{HealthScore: new(60)}))
	counts = [3]int{}
	warm := read(d, &counts)
	require.NotNil(t, warm.AvgHealthScore)
	require.InDelta(t, 60, *warm.AvgHealthScore, 0)
	assert.Equal(t, [3]int{0, 0, 1}, counts)
	tools, err := d.GetAnalyticsTools(ctx, f)
	require.NoError(t, err)
	velocity, err := d.GetAnalyticsVelocity(ctx, f)
	require.NoError(t, err)
	saved, err := OpenIsolated(ctx, d.Path())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, saved.Close()) })
	counts = [3]int{}
	got := read(saved, &counts)
	require.Equal(t, warm, got)
	gotTools, err := saved.GetAnalyticsTools(ctx, f)
	require.NoError(t, err)
	require.Equal(t, tools, gotTools)
	gotVelocity, err := saved.GetAnalyticsVelocity(ctx, f)
	require.NoError(t, err)
	require.Equal(t, velocity, gotVelocity)
	require.Equal(t, [3]int{}, counts)
	require.NoError(t, d.UpdateSessionSignals(ctx, "session", SessionSignalUpdate{HealthScore: new(90)}))
	reopened, err := OpenIsolated(ctx, d.Path())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, reopened.Close()) })
	counts = [3]int{}
	after := read(reopened, &counts)
	require.NotNil(t, after.AvgHealthScore)
	require.InDelta(t, 90, *after.AvgHealthScore, 0)
	assert.Equal(t, [3]int{0, 0, 1}, counts)
}

func TestAnalyticsKeptSessionSelectionChanges(t *testing.T) {
	for _, tc := range []struct{ name, query string }{
		{"relationship", `UPDATE sessions SET relationship_type='subagent' WHERE id='session'`},
		{"nullable date", `UPDATE sessions SET started_at=NULL WHERE id='session'`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := testDB(t)
			ctx := t.Context()
			insertSession(t, d, "session", "project", func(s *Session) { s.StartedAt = new("2026-06-14T10:00:00Z"); s.CreatedAt = "2026-06-15T10:00:00Z" })
			f := AnalyticsFilter{Timezone: "UTC", From: "2026-06-14", To: "2026-06-14"}
			before, err := d.GetAnalyticsSignals(ctx, f)
			require.NoError(t, err)
			require.Equal(t, 1, before.UnscoredSessions)
			_, err = d.getWriter().ExecContext(ctx, tc.query)
			require.NoError(t, err)
			after, err := d.GetAnalyticsSignals(ctx, f)
			require.NoError(t, err)
			require.Zero(t, after.UnscoredSessions)
		})
	}
}

func TestAnalyticsKeptSourceEvents(t *testing.T) {
	for _, tc := range []struct {
		name, query string
		loads       [3]int
		tool        string
		calls       int
		chars       float64
		frustration int
	}{
		{"length", `UPDATE messages SET content_length=20 WHERE ordinal=1`, [3]int{0, 1, 0}, "Read", 1, 20, 0},
		{"tool name", `UPDATE tool_calls SET tool_name='Grep'`, [3]int{1, 0, 0}, "Grep", 1, 10, 0},
		{"prompt", `UPDATE messages SET content='this is broken' WHERE ordinal=0; UPDATE sessions SET transcript_revision=CAST(transcript_revision AS INTEGER)+1`, [3]int{0, 0, 1}, "Read", 1, 10, 1},
		{"tool insert", `INSERT INTO tool_calls(message_id,session_id,tool_name,category) SELECT id,session_id,'Bash','Bash' FROM messages WHERE ordinal=1`, [3]int{1, 1, 0}, "Read", 2, 10, 0},
		{"tool delete", `DELETE FROM tool_calls`, [3]int{1, 1, 0}, "", 0, 10, 0},
		{"message insert", `INSERT INTO messages(session_id,ordinal,role,content,content_length,timestamp) VALUES('session',2,'assistant','answer',20,'2024-06-01T09:02:00Z')`, [3]int{1, 1, 1}, "Read", 1, 15, 0},
		{"message delete", `DELETE FROM messages WHERE ordinal=0`, [3]int{1, 1, 1}, "Read", 1, 0, 0},
		{"price", `INSERT INTO model_pricing(model_pattern,output_microdollars_per_mtok) VALUES('model-a',100)`, [3]int{}, "Read", 1, 10, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := testDB(t)
			ctx := t.Context()
			insertSession(t, d, "session", "project", func(s *Session) { s.StartedAt = new("2024-06-01T09:00:00Z"); s.MessageCount = 2 })
			assistant := asstMsgAt("session", 1, "1234567890", "2024-06-01T09:01:00Z")
			assistant.ContentLength = 10
			assistant.ToolCalls = []ToolCall{{SessionID: "session", ToolName: "Read", Category: "Read"}}
			insertMessages(t, d, Message{SessionID: "session", Ordinal: 0, Role: "user", Content: "please fix", Timestamp: "2024-06-01T09:00:00Z"}, assistant)
			updateSignals(t, d, "session", SessionSignalUpdate{QualitySignals: QualitySignals{Version: CurrentQualitySignalVersion}})
			f := AnalyticsFilter{Timezone: "UTC"}
			var loads [3]int
			read := func() (ToolsAnalyticsResponse, VelocityResponse, SignalsAnalyticsResponse) {
				tools, err := keptAnalytics(ctx, d, "tools", f, func(ctx context.Context, f AnalyticsFilter) (ToolsAnalyticsResponse, error) {
					loads[0]++
					return d.analyticsTools(ctx, f)
				})
				require.NoError(t, err)
				velocity, err := keptAnalytics(ctx, d, "velocity", f, func(ctx context.Context, f AnalyticsFilter) (VelocityResponse, error) {
					loads[1]++
					return d.analyticsVelocity(ctx, f)
				})
				require.NoError(t, err)
				signals, err := keptAnalytics(ctx, d, "signals", f, func(ctx context.Context, f AnalyticsFilter) (SignalsAnalyticsResponse, error) {
					loads[2]++
					return d.analyticsSignals(ctx, f)
				})
				require.NoError(t, err)
				return tools, velocity, signals
			}
			read()
			loads = [3]int{}
			_, err := d.getWriter().ExecContext(ctx, tc.query)
			require.NoError(t, err)
			tools, velocity, signals := read()
			require.Equal(t, tc.loads, loads)
			require.Equal(t, tc.calls, tools.TotalCalls)
			var names []string
			for _, row := range tools.ByTool {
				names = append(names, row.ToolName)
			}
			if tc.tool != "" {
				require.Contains(t, names, tc.tool)
			}
			require.InDelta(t, tc.chars, velocity.Overall.CharsPerActiveMin, 0)
			require.Equal(t, tc.frustration, signals.QualityHealth.Totals.FrustrationMarkerCount)
		})
	}
}

func TestAnalyticsKeptInvalidPanelToken(t *testing.T) {
	cache := t.TempDir()
	t.Setenv("CACHE_DIRECTORY", cache)
	d := testDB(t)
	ctx := t.Context()
	insertSession(t, d, "session", "project")
	message := asstMsgAt("session", 0, "answer", "2024-06-01T09:00:00Z")
	message.ToolCalls = []ToolCall{{SessionID: "session", ToolName: "Read", Category: "Read"}}
	insertMessages(t, d, message)
	for _, query := range []string{`DELETE FROM archive_metadata WHERE key='dashboard_tools'`, `INSERT INTO archive_metadata(key,value) VALUES('dashboard_tools','invalid') ON CONFLICT(key) DO UPDATE SET value=excluded.value`} {
		_, err := d.getWriter().ExecContext(ctx, query)
		require.NoError(t, err)
		tools, err := d.GetAnalyticsTools(ctx, AnalyticsFilter{Timezone: "UTC"})
		require.NoError(t, err)
		require.Equal(t, "Read", tools.ByTool[0].ToolName)
		files, err := filepath.Glob(filepath.Join(cache, "sqlite-trends", "*", "*.gob"))
		require.NoError(t, err)
		require.Empty(t, files)
	}
	_, err := d.getWriter().ExecContext(ctx, `UPDATE tool_calls SET tool_name='Grep' WHERE session_id='session'`)
	require.NoError(t, err)
	tools, err := d.GetAnalyticsTools(ctx, AnalyticsFilter{Timezone: "UTC"})
	require.NoError(t, err)
	require.Equal(t, "Grep", tools.ByTool[0].ToolName)
	files, err := filepath.Glob(filepath.Join(cache, "sqlite-trends", "*", "*.gob"))
	require.NoError(t, err)
	require.Len(t, files, 1)
}

func TestAnalyticsKeptModelTimeScopeChanges(t *testing.T) {
	for _, tc := range []struct{ name, query string }{
		{"model", `UPDATE messages SET model='model-b' WHERE session_id='session' AND ordinal=1`},
		{"role", `UPDATE messages SET role='assistant' WHERE session_id='session' AND ordinal=0`},
		{"system", `UPDATE messages SET is_system=1 WHERE session_id='session' AND ordinal=0`},
		{"timestamp", `UPDATE messages SET timestamp=NULL WHERE session_id='session' AND ordinal=0`},
		{"ordinal", `UPDATE messages SET ordinal=3 WHERE session_id='session' AND ordinal=0`},
		{"move", `UPDATE messages SET session_id='other' WHERE session_id='session' AND ordinal=0`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := testDB(t)
			ctx := t.Context()
			insertSession(t, d, "session", "project", func(s *Session) { s.StartedAt = new("2026-06-14T10:00:00Z"); s.MessageCount = 2 })
			insertSession(t, d, "other", "other")
			insertMessages(t, d,
				Message{SessionID: "session", Ordinal: 0, Role: "user", Content: "hello", Timestamp: "2026-06-14T10:00:00Z"},
				Message{SessionID: "session", Ordinal: 1, Role: "assistant", Content: "answer", Model: "model-a", Timestamp: "2026-06-14T11:00:00Z"},
			)
			f := AnalyticsFilter{Timezone: "UTC", Model: "model-a", Project: "project", Hour: new(10), DayOfWeek: new(6)}
			before, err := d.GetAnalyticsSignals(ctx, f)
			require.NoError(t, err)
			require.Equal(t, 1, before.UnscoredSessions)
			_, err = d.getWriter().ExecContext(ctx, tc.query)
			require.NoError(t, err)
			after, err := d.GetAnalyticsSignals(ctx, f)
			require.NoError(t, err)
			require.Zero(t, after.UnscoredSessions)
		})
	}
}

func TestAnalyticsKeptRestoreRewrite(t *testing.T) {
	for _, tc := range []struct {
		name, first, second string
		older, usage        bool
	}{
		{"analytics", `UPDATE sessions SET tool_retry_count=1 WHERE id='session'; UPDATE tool_calls SET tool_name='first' WHERE session_id='session'; UPDATE messages SET content_length=10 WHERE session_id='session' AND ordinal=1`, `UPDATE sessions SET tool_retry_count=2 WHERE id='session'; UPDATE tool_calls SET tool_name='second' WHERE session_id='session'; UPDATE messages SET content_length=20 WHERE session_id='session' AND ordinal=1`, false, false},
		{"older writer", `UPDATE sessions SET tool_retry_count=1 WHERE id='session'; UPDATE tool_calls SET tool_name='first' WHERE session_id='session'; UPDATE messages SET content_length=10 WHERE session_id='session' AND ordinal=1`, `UPDATE sessions SET tool_retry_count=2 WHERE id='session'; UPDATE tool_calls SET tool_name='second' WHERE session_id='session'; UPDATE messages SET content_length=20 WHERE session_id='session' AND ordinal=1`, true, false},
		{"usage", `UPDATE sessions SET project='first' WHERE id='session'`, `UPDATE sessions SET project='second' WHERE id='session'`, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("CACHE_DIRECTORY", t.TempDir())
			ctx := t.Context()
			path := filepath.Join(t.TempDir(), "archive.db")
			d := testDBAtPath(t, path, "archive")
			insertSession(t, d, "session", "project", func(s *Session) { s.StartedAt = new("2024-06-01T09:00:00Z"); s.MessageCount = 2 })
			message := asstMsgAt("session", 1, "answer", "2024-06-01T09:01:00Z")
			message.ToolCalls = []ToolCall{{SessionID: "session", ToolName: "Read", Category: "Read"}}
			insertMessages(t, d, Message{SessionID: "session", Ordinal: 0, Role: "user", Content: "hello", Timestamp: "2024-06-01T09:00:00Z"}, message)
			updateSignals(t, d, "session", SessionSignalUpdate{QualitySignals: QualitySignals{Version: CurrentQualitySignalVersion}})
			require.NoError(t, d.Close())
			backup, err := os.ReadFile(path)
			require.NoError(t, err)
			d, err = OpenIsolated(ctx, path)
			require.NoError(t, err)
			_, err = d.getWriter().ExecContext(ctx, tc.first)
			require.NoError(t, err)
			id, err := d.GetDatabaseID(ctx)
			require.NoError(t, err)
			usage, err := d.UsageChangeRevision(ctx)
			require.NoError(t, err)
			analytics, err := d.archiveJournalRevision(ctx, archiveMetadataAnalyticsChangeRevisionKey, "analytics")
			require.NoError(t, err)
			filter := AnalyticsFilter{Timezone: "UTC"}
			if tc.usage {
				filter.Project = "second"
			}
			beforeTools, err := d.GetAnalyticsTools(ctx, filter)
			require.NoError(t, err)
			beforeVelocity, err := d.GetAnalyticsVelocity(ctx, filter)
			require.NoError(t, err)
			beforeSignals, err := d.GetAnalyticsSignals(ctx, filter)
			require.NoError(t, err)
			if !tc.usage {
				require.Equal(t, "first", beforeTools.ByTool[0].ToolName)
				require.InDelta(t, 10, beforeVelocity.Overall.CharsPerActiveMin, 0)
				require.Equal(t, 1, beforeSignals.ToolHealth.TotalRetries)
			} else {
				require.Zero(t, beforeTools.TotalCalls)
				require.Empty(t, beforeVelocity.ByAgent)
				require.Zero(t, beforeSignals.UnscoredSessions)
			}
			require.NoError(t, d.Close())
			require.NoError(t, os.WriteFile(path, backup, 0o600))
			d, err = OpenIsolated(ctx, path)
			require.NoError(t, err)
			if tc.older {
				_, err = d.getWriter().ExecContext(ctx, usageChangeJournalTriggerDropsSQL+usageChangeJournalTriggerSQL)
				require.NoError(t, err)
			}
			_, err = d.getWriter().ExecContext(ctx, tc.second)
			require.NoError(t, err)
			if tc.older {
				require.NoError(t, d.Close())
				d, err = OpenIsolated(ctx, path)
				require.NoError(t, err)
			}
			newID, err := d.GetDatabaseID(ctx)
			require.NoError(t, err)
			newUsage, err := d.UsageChangeRevision(ctx)
			require.NoError(t, err)
			newAnalytics, err := d.archiveJournalRevision(ctx, archiveMetadataAnalyticsChangeRevisionKey, "analytics")
			require.NoError(t, err)
			require.Equal(t, id, newID)
			require.Equal(t, usage, newUsage)
			require.Equal(t, analytics, newAnalytics)
			calls := [3]int{}
			read := func() {
				tools, err := keptAnalytics(ctx, d, "tools", filter, func(ctx context.Context, f AnalyticsFilter) (ToolsAnalyticsResponse, error) {
					calls[0]++
					return d.analyticsTools(ctx, f)
				})
				require.NoError(t, err)
				require.Equal(t, 1, tools.TotalCalls)
				if !tc.usage {
					require.Equal(t, "second", tools.ByTool[0].ToolName)
				}
				velocity, err := keptAnalytics(ctx, d, "velocity", filter, func(ctx context.Context, f AnalyticsFilter) (VelocityResponse, error) {
					calls[1]++
					return d.analyticsVelocity(ctx, f)
				})
				require.NoError(t, err)
				if !tc.usage {
					require.InDelta(t, 20, velocity.Overall.CharsPerActiveMin, 0)
				} else {
					require.Len(t, velocity.ByAgent, 1)
				}
				result, err := keptAnalytics(ctx, d, "signals", filter, func(ctx context.Context, f AnalyticsFilter) (SignalsAnalyticsResponse, error) {
					calls[2]++
					return d.analyticsSignals(ctx, f)
				})
				require.NoError(t, err)
				if tc.usage {
					require.Equal(t, 1, result.UnscoredSessions)
				} else {
					require.Equal(t, 2, result.ToolHealth.TotalRetries)
				}
			}
			read()
			require.Equal(t, [3]int{1, 1, 1}, calls)
			require.NoError(t, d.Close())
			d, err = OpenIsolated(ctx, path)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, d.Close()) })
			read()
			require.Equal(t, [3]int{1, 1, 1}, calls)
		})
	}
}

func TestAnalyticsKeptNestedCounterTrigger(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()
	_, err := d.getWriter().ExecContext(ctx, `PRAGMA recursive_triggers = ON`)
	require.NoError(t, err)
	before, err := d.GetAnalyticsSignals(ctx, AnalyticsFilter{})
	require.NoError(t, err)
	require.Zero(t, before.UnscoredSessions)
	insertSession(t, d, "nested", "project")
	after, err := d.GetAnalyticsSignals(ctx, AnalyticsFilter{})
	require.NoError(t, err)
	require.Equal(t, 1, after.UnscoredSessions)
}

func TestAnalyticsSignalsMessageRepair(t *testing.T) {
	t.Setenv("CACHE_DIRECTORY", t.TempDir())
	d := testDB(t)
	ctx := t.Context()
	seedFrustrationSession(t, d, "session", "test")
	before, err := d.GetAnalyticsSignals(ctx, AnalyticsFilter{})
	require.NoError(t, err)
	require.Equal(t, 1, before.QualityHealth.Totals.FrustrationMarkerCount)
	_, err = d.getWriter().ExecContext(ctx, `UPDATE messages SET content = 'Please explain this function.' WHERE session_id = 'session'`)
	require.NoError(t, err)
	after, err := d.GetAnalyticsSignals(ctx, AnalyticsFilter{})
	require.NoError(t, err)
	require.Zero(t, after.QualityHealth.Totals.FrustrationMarkerCount)
	reopened, err := OpenIsolated(ctx, d.Path())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, reopened.Close()) })
	afterRestart, err := reopened.GetAnalyticsSignals(ctx, AnalyticsFilter{})
	require.NoError(t, err)
	require.Zero(t, afterRestart.QualityHealth.Totals.FrustrationMarkerCount)
}
