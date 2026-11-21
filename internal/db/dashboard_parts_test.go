//go:build fts5

package db

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/kept"
)

func TestDashboardPartsMatchScansAndReuse(t *testing.T) {
	t.Setenv("CACHE_DIRECTORY", t.TempDir())
	d := testDB(t)
	ctx := t.Context()
	for _, id := range []string{"a", "b", "empty"} {
		insertSession(t, d, id, "project", func(s *Session) {
			s.StartedAt = new("2024-06-01T09:00:00Z")
			s.MessageCount = 2
			if id == "b" {
				s.Agent = "codex"
			}
		})
	}
	a := userMsgAt("a", 0, "hello", "2024-06-01T09:00:00Z")
	b := asstMsgAt("a", 1, "answer", "2024-06-01T09:01:00Z")
	b.HasThinking = true
	b.ToolCalls = []ToolCall{{SessionID: "a", ToolName: "Read", Category: "Read"}}
	hook := userMsgAt("b", 0, "hook", "invalid")
	hook.SourceSubtype = "hook_success"
	result := userMsgAt("b", 1, "tool result", "2024-06-02T01:00:00Z")
	result.SourceSubtype = "tool_result"
	nullTime := asstMsgAt("b", 2, "undated", "2024-06-02T02:00:00Z")
	insertMessages(t, d, a, b, hook, result, nullTime)
	_, err := d.getWriter().ExecContext(ctx, `UPDATE messages SET timestamp=NULL WHERE session_id='b' AND ordinal=2`)
	require.NoError(t, err)
	f := AnalyticsFilter{Timezone: "UTC"}
	var reads [][]string
	dashboardPartsReadObserver = func(ids []string) { reads = append(reads, append([]string(nil), ids...)) }
	t.Cleanup(func() { dashboardPartsReadObserver = nil })
	check := func(filter AnalyticsFilter) {
		activity, err := d.GetAnalyticsActivity(ctx, filter, "day")
		require.NoError(t, err)
		hours, err := d.GetAnalyticsHourOfWeek(ctx, filter)
		require.NoError(t, err)
		velocity, err := d.analyticsVelocity(ctx, filter)
		require.NoError(t, err)
		d.readOnly = true
		scanActivity, err := d.GetAnalyticsActivity(ctx, filter, "day")
		require.NoError(t, err)
		scanHours, err := d.GetAnalyticsHourOfWeek(ctx, filter)
		require.NoError(t, err)
		scanVelocity, err := d.analyticsVelocity(ctx, filter)
		require.NoError(t, err)
		d.readOnly = false
		assert.Equal(t, scanActivity, activity)
		assert.Equal(t, scanHours, hours)
		assert.Equal(t, scanVelocity, velocity)
	}
	check(f)
	require.Equal(t, [][]string{{"a", "b", "empty"}}, reads)
	check(AnalyticsFilter{Timezone: "UTC", Agent: "codex"})
	require.Len(t, reads, 1)
	insertMessages(t, d, asstMsgAt("a", 2, "later", "2024-06-01T09:02:00Z"))
	check(f)
	require.Equal(t, [][]string{{"a", "b", "empty"}, {"a"}}, reads)
	b.ToolCalls = append(b.ToolCalls, ToolCall{SessionID: "a", ToolName: "Write", Category: "Write"})
	require.NoError(t, d.ReplaceSessionMessages(ctx, "a", []Message{a, b}))
	check(f)
	require.NoError(t, d.DeleteSession(ctx, "a"))
	_, err = d.getWriter().ExecContext(ctx, `DELETE FROM excluded_sessions WHERE id = 'a'`)
	require.NoError(t, err)
	insertSession(t, d, "a", "other", func(s *Session) { s.StartedAt = new("2024-06-01T09:00:00Z") })
	insertMessages(t, d, userMsgAt("a", 0, "new", "2024-06-01T10:00:00Z"))
	check(f)
	check(AnalyticsFilter{Timezone: "UTC", Project: "other"})
	check(AnalyticsFilter{Timezone: "UTC", Project: "absent"})
	session, err := d.GetSession(ctx, "b")
	require.NoError(t, err)
	require.NotNil(t, session)
	session.Agent = "claude"
	session.Project = "other"
	session.ParentSessionID = new("empty")
	require.NoError(t, d.UpsertSession(ctx, *session))
	check(f)
	check(AnalyticsFilter{Timezone: "UTC", IncludeSubagents: true})
	check(AnalyticsFilter{Timezone: "America/New_York", From: "2024-06-01", To: "2024-06-01"})
	hour := 9
	check(AnalyticsFilter{Timezone: "UTC", Hour: &hour})
	check(AnalyticsFilter{Timezone: "UTC", Model: "other"})
}

func TestDashboardPartsContentLengthReplacement(t *testing.T) {
	t.Setenv("CACHE_DIRECTORY", t.TempDir())
	d := testDB(t)
	ctx := t.Context()
	insertSession(t, d, "s", "project", func(s *Session) { s.StartedAt = new("2024-06-01T09:00:00Z"); s.MessageCount = 2 })
	messages := []Message{userMsgAt("s", 0, "hello", "2024-06-01T09:00:00Z"), asstMsgAt("s", 1, "answer", "2024-06-01T09:01:00Z")}
	insertMessages(t, d, messages...)
	before, err := d.GetAnalyticsVelocity(ctx, AnalyticsFilter{Timezone: "UTC"})
	require.NoError(t, err)
	assert.InDelta(t, 6, before.Overall.CharsPerActiveMin, 0)
	messages[1].ContentLength = 20
	require.NoError(t, d.ReplaceSessionMessages(ctx, "s", messages))
	after, err := d.GetAnalyticsVelocity(ctx, AnalyticsFilter{Timezone: "UTC"})
	require.NoError(t, err)
	assert.InDelta(t, 20, after.Overall.CharsPerActiveMin, 0)
	stored, err := d.GetAllMessages(ctx, "s")
	require.NoError(t, err)
	session, err := d.GetSession(ctx, "s")
	require.NoError(t, err)
	require.NotNil(t, session)
	stored[1].ContentLength = 30
	batch, err := d.WriteSessionBatchContext(ctx, []SessionBatchWrite{{Session: *session, Messages: stored, ReplaceMessages: true, SkipSignalUpdates: true}})
	require.NoError(t, err)
	require.Zero(t, batch.FailedSessions)
	updated, err := d.GetAllMessages(ctx, "s")
	require.NoError(t, err)
	require.Equal(t, stored[1].ID, updated[1].ID)
	after, err = d.GetAnalyticsVelocity(ctx, AnalyticsFilter{Timezone: "UTC"})
	require.NoError(t, err)
	assert.InDelta(t, 30, after.Overall.CharsPerActiveMin, 0)
}

func TestDashboardPartsWriteDuringFillFallsBack(t *testing.T) {
	t.Setenv("CACHE_DIRECTORY", t.TempDir())
	d := testDB(t)
	ctx := t.Context()
	insertSession(t, d, "s", "project", func(s *Session) { s.StartedAt = new("2024-06-01T09:00:00Z") })
	insertMessages(t, d, userMsgAt("s", 0, "hello", "2024-06-01T09:00:00Z"))
	dashboardPartsBeforeValidationObserver = func(context.Context) {
		dashboardPartsBeforeValidationObserver = nil
		insertMessages(t, d, asstMsgAt("s", 1, "answer", "2024-06-01T09:01:00Z"))
	}
	t.Cleanup(func() { dashboardPartsBeforeValidationObserver = nil })
	result, err := d.GetAnalyticsActivity(ctx, AnalyticsFilter{Timezone: "UTC"}, "day")
	require.NoError(t, err)
	require.Len(t, result.Series, 1)
	assert.Equal(t, 2, result.Series[0].Messages)
	identity, err := d.GetDatabaseID(ctx)
	require.NoError(t, err)
	_, ok := d.trendsDisk.Get("dashboard-parts|"+TrendsTimezoneKey(AnalyticsFilter{Timezone: "UTC"}), fmt.Sprintf("%s|%d", identity, dashboardPartsVersion))
	assert.False(t, ok)
}

func TestDashboardPartsCancellationReleasesPermit(t *testing.T) {
	t.Setenv("CACHE_DIRECTORY", t.TempDir())
	d := testDB(t)
	insertSession(t, d, "s", "project")
	entered := make(chan struct{})
	left := make(chan struct{})
	dashboardPartsBeforeValidationObserver = func(ctx context.Context) { close(entered); <-ctx.Done(); close(left) }
	t.Cleanup(func() { dashboardPartsBeforeValidationObserver = nil })
	ctx, cancel := context.WithCancel(t.Context())
	result := make(chan error, 1)
	go func() { _, err := d.GetAnalyticsHourOfWeek(ctx, AnalyticsFilter{Timezone: "UTC"}); result <- err }()
	<-entered
	cancel()
	require.ErrorIs(t, <-result, context.Canceled)
	<-left
	releases := make([]func(), 0, readerMaxOpenConns-1)
	for range readerMaxOpenConns - 1 {
		release, err := d.acquireBulkRead(t.Context())
		require.NoError(t, err)
		releases = append(releases, release)
	}
	for _, release := range releases {
		release()
	}
}

func TestDashboardPartsReadOnlyBypassesDisk(t *testing.T) {
	t.Setenv("CACHE_DIRECTORY", t.TempDir())
	d := testDB(t)
	insertSession(t, d, "s", "project")
	insertMessages(t, d, userMsg("s", 0, "hello"))
	reader, err := OpenReadOnly(t.Context(), d.Path())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, reader.Close()) })
	result, err := reader.GetAnalyticsActivity(t.Context(), AnalyticsFilter{Timezone: "UTC"}, "day")
	require.NoError(t, err)
	require.Len(t, result.Series, 1)
	assert.Equal(t, 1, result.Series[0].Messages)
	assert.False(t, reader.trendsDisk.Enabled())
}

func TestDashboardPartsConcurrentPanels(t *testing.T) {
	t.Setenv("CACHE_DIRECTORY", t.TempDir())
	d := testDB(t)
	insertSession(t, d, "s", "project", func(s *Session) { s.StartedAt = new("2024-06-01T09:00:00Z"); s.MessageCount = 2 })
	insertMessages(t, d, userMsgAt("s", 0, "hello", "2024-06-01T09:00:00Z"), asstMsgAt("s", 1, "answer", "2024-06-01T09:01:00Z"))
	ctx := t.Context()
	f := AnalyticsFilter{Timezone: "UTC"}
	start := make(chan struct{})
	results := make(chan error, 3)
	var reads atomic.Int32
	dashboardPartsReadObserver = func([]string) { reads.Add(1) }
	t.Cleanup(func() { dashboardPartsReadObserver = nil })
	go func() {
		<-start
		a, err := d.GetAnalyticsActivity(ctx, f, "day")
		if err == nil && (len(a.Series) != 1 || a.Series[0].Messages != 2) {
			err = errors.New("wrong activity")
		}
		results <- err
	}()
	go func() {
		<-start
		h, err := d.GetAnalyticsHourOfWeek(ctx, f)
		if err == nil && (len(h.Cells) != 168 || h.Cells[129] != (HourOfWeekCell{DayOfWeek: 5, Hour: 9, Messages: 2})) {
			err = errors.New("wrong hours")
		}
		results <- err
	}()
	go func() {
		<-start
		v, err := d.GetAnalyticsVelocity(ctx, f)
		if err == nil && v.Overall.CharsPerActiveMin != 6 {
			err = errors.New("wrong velocity")
		}
		results <- err
	}()
	close(start)
	for range 3 {
		require.NoError(t, <-results)
	}
	assert.EqualValues(t, 1, reads.Load())
}

func TestDashboardPartsOtherSessionWriteDuringFill(t *testing.T) {
	t.Setenv("CACHE_DIRECTORY", t.TempDir())
	d := testDB(t)
	ctx := t.Context()
	insertSession(t, d, "selected", "project", func(s *Session) { s.Agent = "codex" })
	insertSession(t, d, "other", "project", func(s *Session) { s.Agent = "gemini" })
	insertMessages(t, d, userMsg("selected", 0, "hello"))
	var before, after, commonBefore, commonAfter string
	require.NoError(t, d.getReader().QueryRowContext(ctx, `SELECT (SELECT value FROM archive_metadata WHERE key='dashboard_scope'), (SELECT value FROM archive_metadata WHERE key='dashboard_common')`).Scan(&before, &commonBefore))
	dashboardPartsBeforeValidationObserver = func(context.Context) {
		dashboardPartsBeforeValidationObserver = nil
		insertMessages(t, d, userMsg("other", 0, "elsewhere"))
		_, err := d.getWriter().ExecContext(ctx, `UPDATE sessions SET message_count=2 WHERE id='other'`)
		require.NoError(t, err)
	}
	t.Cleanup(func() { dashboardPartsBeforeValidationObserver = nil })
	sessions, parts, handled, err := d.dashboardParts(ctx, AnalyticsFilter{Timezone: "UTC", Agent: "codex"})
	require.NoError(t, err)
	require.True(t, handled)
	require.NoError(t, d.getReader().QueryRowContext(ctx, `SELECT (SELECT value FROM archive_metadata WHERE key='dashboard_scope'), (SELECT value FROM archive_metadata WHERE key='dashboard_common')`).Scan(&after, &commonAfter))
	require.NotEqual(t, before, after)
	require.NotEqual(t, commonBefore, commonAfter)
	require.Len(t, sessions, 1)
	assert.Equal(t, 1, parts["selected"].Value.Messages)
	identity, err := d.GetDatabaseID(ctx)
	require.NoError(t, err)
	_, ok := d.trendsDisk.Get("dashboard-parts|"+TrendsTimezoneKey(AnalyticsFilter{Timezone: "UTC"}), fmt.Sprintf("%s|%d", identity, dashboardPartsVersion))
	assert.True(t, ok)
}

func TestDashboardPartsLengthUpdateWithoutRevision(t *testing.T) {
	t.Setenv("CACHE_DIRECTORY", t.TempDir())
	d := testDB(t)
	ctx := t.Context()
	insertSession(t, d, "s", "project", func(s *Session) { s.StartedAt = new("2024-06-01T09:00:00Z"); s.MessageCount = 2 })
	insertMessages(t, d, userMsgAt("s", 0, "hello", "2024-06-01T09:00:00Z"), asstMsgAt("s", 1, "answer", "2024-06-01T09:01:00Z"))
	f := AnalyticsFilter{Timezone: "UTC"}
	before, err := d.GetAnalyticsVelocity(ctx, f)
	require.NoError(t, err)
	require.InDelta(t, 6, before.Overall.CharsPerActiveMin, 0)
	var revision string
	require.NoError(t, d.getReader().QueryRowContext(ctx, `SELECT transcript_revision FROM sessions WHERE id='s'`).Scan(&revision))
	_, err = d.getWriter().ExecContext(ctx, `UPDATE messages SET content_length=20 WHERE session_id='s' AND ordinal=1`)
	require.NoError(t, err)
	var afterRevision string
	require.NoError(t, d.getReader().QueryRowContext(ctx, `SELECT transcript_revision FROM sessions WHERE id='s'`).Scan(&afterRevision))
	require.Equal(t, revision, afterRevision)
	after, err := d.GetAnalyticsVelocity(ctx, f)
	require.NoError(t, err)
	assert.InDelta(t, 20, after.Overall.CharsPerActiveMin, 0)
	_, err = d.getWriter().ExecContext(ctx, `BEGIN; DELETE FROM messages WHERE session_id='s' AND ordinal=1; INSERT INTO messages(session_id,ordinal,role,content,timestamp,content_length) VALUES('s',1,'assistant','answer','2024-06-01T09:01:00Z',30); COMMIT`)
	require.NoError(t, err)
	require.NoError(t, d.getReader().QueryRowContext(ctx, `SELECT transcript_revision FROM sessions WHERE id='s'`).Scan(&afterRevision))
	require.Equal(t, revision, afterRevision)
	after, err = d.GetAnalyticsVelocity(ctx, f)
	require.NoError(t, err)
	assert.InDelta(t, 30, after.Overall.CharsPerActiveMin, 0)
}

func TestDashboardPartsPrunesDeletedSessions(t *testing.T) {
	t.Setenv("CACHE_DIRECTORY", t.TempDir())
	d := testDB(t)
	ctx := t.Context()
	insertSession(t, d, "a", "project", func(s *Session) { s.Agent = "codex" })
	insertSession(t, d, "b", "project", func(s *Session) { s.Agent = "gemini" })
	_, _, handled, err := d.dashboardParts(ctx, AnalyticsFilter{Timezone: "UTC"})
	require.NoError(t, err)
	require.True(t, handled)
	require.NoError(t, d.DeleteSession(ctx, "a"))
	_, _, handled, err = d.dashboardParts(ctx, AnalyticsFilter{Timezone: "UTC", Agent: "gemini"})
	require.NoError(t, err)
	require.True(t, handled)
	identity, err := d.GetDatabaseID(ctx)
	require.NoError(t, err)
	parts := kept.LoadParts[dashboardPart](&d.trendsDisk, "dashboard-parts|"+TrendsTimezoneKey(AnalyticsFilter{Timezone: "UTC"}), fmt.Sprintf("%s|%d", identity, dashboardPartsVersion))
	assert.NotContains(t, parts, "a")
	assert.Contains(t, parts, "b")
}

func TestDashboardPartsQueuedFillsDoNotHoldBulkPermits(t *testing.T) {
	t.Setenv("CACHE_DIRECTORY", t.TempDir())
	d := testDB(t)
	insertSession(t, d, "a", "project", func(s *Session) { s.Agent = "codex" })
	insertSession(t, d, "b", "project", func(s *Session) { s.Agent = "gemini" })
	d.dashboardPartsOnce.Do(func() { d.dashboardPartsSlot = make(chan struct{}, 1) })
	d.dashboardPartsSlot <- struct{}{}
	entered := make(chan struct{}, 2)
	dashboardPartsBeforeSlotObserver = func() { entered <- struct{}{} }
	t.Cleanup(func() { dashboardPartsBeforeSlotObserver = nil })
	ctx, cancel := context.WithCancel(t.Context())
	results := make(chan error, 2)
	for _, agent := range []string{"codex", "gemini"} {
		go func() {
			_, err := d.GetAnalyticsHourOfWeek(ctx, AnalyticsFilter{Timezone: "UTC", Agent: agent})
			results <- err
		}()
	}
	<-entered
	<-entered
	//nolint:kennlint // deadlinetest: the deadline detects permits held while fills wait.
	guard, stop := context.WithTimeout(t.Context(), 5*time.Second)
	defer stop()
	var releases []func()
	for range readerMaxOpenConns - 1 {
		release, err := d.acquireBulkRead(guard)
		if err != nil {
			break
		}
		releases = append(releases, release)
	}
	for _, release := range releases {
		release()
	}
	cancel()
	<-d.dashboardPartsSlot
	for range 2 {
		<-results
	}
	assert.Len(t, releases, readerMaxOpenConns-1)
}

func TestDashboardPartsToolChangeWithoutTranscriptRevision(t *testing.T) {
	t.Setenv("CACHE_DIRECTORY", t.TempDir())
	d := testDB(t)
	ctx := t.Context()
	insertSession(t, d, "s", "project", func(s *Session) { s.StartedAt = new("2024-06-01T09:00:00Z"); s.MessageCount = 2 })
	user := userMsgAt("s", 0, "hello", "2024-06-01T09:00:00Z")
	assistant := asstMsgAt("s", 1, "answer", "2024-06-01T09:01:00Z")
	assistant.ToolCalls = []ToolCall{{SessionID: "s", ToolName: "Read", Category: "Read"}}
	insertMessages(t, d, user, assistant)
	f := AnalyticsFilter{Timezone: "UTC"}
	before, err := d.GetAnalyticsActivity(ctx, f, "day")
	require.NoError(t, err)
	require.Equal(t, 1, before.Series[0].ToolCalls)
	var revision string
	require.NoError(t, d.getReader().QueryRowContext(ctx, `SELECT transcript_revision FROM sessions WHERE id='s'`).Scan(&revision))
	_, err = d.getWriter().ExecContext(ctx, `DELETE FROM tool_calls WHERE session_id='s'`)
	require.NoError(t, err)
	var generation *string
	require.NoError(t, d.getReader().QueryRowContext(ctx, `SELECT tools_generation FROM sessions WHERE id='s'`).Scan(&generation))
	require.Nil(t, generation)
	after, err := d.GetAnalyticsActivity(ctx, f, "day")
	require.NoError(t, err)
	assert.Zero(t, after.Series[0].ToolCalls)
	require.NoError(t, d.getReader().QueryRowContext(ctx, `SELECT transcript_revision, tools_generation FROM sessions WHERE id='s'`).Scan(&revision, &generation))
	require.NotNil(t, generation)
	_, err = d.getWriter().ExecContext(ctx, `INSERT INTO tool_calls(session_id,message_id,tool_name,category) SELECT 's', id, 'Write', 'Write' FROM messages WHERE session_id='s' AND ordinal=1`)
	require.NoError(t, err)
	_, err = d.getWriter().ExecContext(ctx, `INSERT INTO tool_calls(session_id,message_id,tool_name,category) SELECT 's', id, 'Read', 'Read' FROM messages WHERE session_id='s' AND ordinal=1`)
	require.NoError(t, err)
	after, err = d.GetAnalyticsActivity(ctx, f, "day")
	require.NoError(t, err)
	assert.Equal(t, 2, after.Series[0].ToolCalls)
	var finalRevision string
	require.NoError(t, d.getReader().QueryRowContext(ctx, `SELECT transcript_revision FROM sessions WHERE id='s'`).Scan(&finalRevision))
	assert.Equal(t, revision, finalRevision)
}

func TestDashboardPartsSessionMetadataChangeDuringFill(t *testing.T) {
	t.Setenv("CACHE_DIRECTORY", t.TempDir())
	d := testDB(t)
	ctx := t.Context()
	insertSession(t, d, "s", "project", func(s *Session) { s.Agent = "codex" })
	dashboardPartsBeforeValidationObserver = func(context.Context) {
		dashboardPartsBeforeValidationObserver = nil
		_, err := d.getWriter().ExecContext(ctx, `UPDATE sessions SET agent='gemini', message_count=42 WHERE id='s'`)
		require.NoError(t, err)
	}
	t.Cleanup(func() { dashboardPartsBeforeValidationObserver = nil })
	_, _, handled, err := d.dashboardParts(ctx, AnalyticsFilter{Timezone: "UTC"})
	require.NoError(t, err)
	assert.False(t, handled)
}
