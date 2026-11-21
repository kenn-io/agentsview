//go:build fts5

package db

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/kept"
)

func addDashboardCacheSessions(t *testing.T, d *DB, count int) {
	t.Helper()
	for i := range count {
		insertSession(t, d, fmt.Sprintf("extra-%d", i), "project", func(s *Session) {
			s.StartedAt = new("2024-06-01T09:00:00Z")
		})
	}
}

func dashboardTestSlot(t *testing.T, f AnalyticsFilter) string {
	t.Helper()
	data, err := json.Marshal(f, json.Deterministic(true))
	require.NoError(t, err)
	return "dashboard-parts|" + TrendsTimezoneKey(f) + "|" + string(data)
}

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
	addDashboardCacheSessions(t, d, 2)
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
	require.Equal(t, [][]string{{"a", "b", "empty", "extra-0", "extra-1"}}, reads)
	check(AnalyticsFilter{Timezone: "UTC", Agent: "codex"})
	require.Equal(t, [][]string{{"a", "b", "empty", "extra-0", "extra-1"}, {"b"}}, reads)
	insertMessages(t, d, asstMsgAt("a", 2, "later", "2024-06-01T09:02:00Z"))
	check(f)
	require.Equal(t, [][]string{{"a", "b", "empty", "extra-0", "extra-1"}, {"b"}, {"a"}}, reads)
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

func TestDashboardPartsSelectionUsesSeparateFile(t *testing.T) {
	t.Setenv("CACHE_DIRECTORY", t.TempDir())
	d := testDB(t)
	ctx := t.Context()
	for i := range 10 {
		id := fmt.Sprintf("s%d", i)
		insertSession(t, d, id, "project", func(s *Session) {
			if i < 5 {
				s.Agent = "codex"
			} else {
				s.Agent = "gemini"
			}
		})
		insertMessages(t, d, userMsg(id, 0, "hello"))
	}
	var reads [][]string
	dashboardPartsReadObserver = func(ids []string) { reads = append(reads, append([]string(nil), ids...)) }
	t.Cleanup(func() { dashboardPartsReadObserver = nil })
	_, _, handled, err := d.dashboardParts(ctx, AnalyticsFilter{Timezone: "UTC"})
	require.NoError(t, err)
	require.True(t, handled)
	_, parts, handled, err := d.dashboardParts(ctx, AnalyticsFilter{Timezone: "UTC", Agent: "codex"})
	require.NoError(t, err)
	require.True(t, handled)
	require.Len(t, parts, 5)
	require.Len(t, reads, 2)
	assert.Equal(t, []string{"s0", "s1", "s2", "s3", "s4"}, reads[1])
}

func TestDashboardPartsContentLengthReplacement(t *testing.T) {
	t.Setenv("CACHE_DIRECTORY", t.TempDir())
	d := testDB(t)
	ctx := t.Context()
	insertSession(t, d, "s", "project", func(s *Session) { s.StartedAt = new("2024-06-01T09:00:00Z"); s.MessageCount = 2 })
	addDashboardCacheSessions(t, d, 4)
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
	addDashboardCacheSessions(t, d, 4)
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
	d.readOnly = true
	scan, err := d.GetAnalyticsActivity(ctx, AnalyticsFilter{Timezone: "UTC"}, "day")
	d.readOnly = false
	require.NoError(t, err)
	assert.Equal(t, scan, result)
	identity, err := d.GetDatabaseID(ctx)
	require.NoError(t, err)
	data, ok := d.trendsDisk.Get(dashboardTestSlot(t, AnalyticsFilter{Timezone: "UTC"}), fmt.Sprintf("%s|%d", identity, dashboardPartsVersion))
	require.True(t, ok)
	_ = data
	parts := kept.LoadParts[dashboardPart](&d.trendsDisk, dashboardTestSlot(t, AnalyticsFilter{Timezone: "UTC"}), fmt.Sprintf("%s|%d", identity, dashboardPartsVersion))
	assert.NotContains(t, parts, "s")
	assert.Contains(t, parts, "extra-0")
	var reads [][]string
	dashboardPartsReadObserver = func(ids []string) { reads = append(reads, append([]string(nil), ids...)) }
	t.Cleanup(func() { dashboardPartsReadObserver = nil })
	_, err = d.GetAnalyticsActivity(ctx, AnalyticsFilter{Timezone: "UTC"}, "day")
	require.NoError(t, err)
	assert.Equal(t, [][]string{{"s"}}, reads)
}

func TestDashboardPartsCancellationReleasesPermit(t *testing.T) {
	t.Setenv("CACHE_DIRECTORY", t.TempDir())
	d := testDB(t)
	insertSession(t, d, "s", "project")
	addDashboardCacheSessions(t, d, 4)
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
	addDashboardCacheSessions(t, d, 4)
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
	for i := 1; i <= 32; i++ {
		insertMessages(t, d, userMsg("selected", i, "hello"))
	}
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
	assert.Equal(t, 33, parts["selected"].Value.Messages)
	identity, err := d.GetDatabaseID(ctx)
	require.NoError(t, err)
	_, ok := d.trendsDisk.Get(dashboardTestSlot(t, AnalyticsFilter{Timezone: "UTC", Agent: "codex"}), fmt.Sprintf("%s|%d", identity, dashboardPartsVersion))
	assert.True(t, ok)
}

func TestDashboardPartsLengthUpdateWithoutRevision(t *testing.T) {
	t.Setenv("CACHE_DIRECTORY", t.TempDir())
	d := testDB(t)
	ctx := t.Context()
	insertSession(t, d, "s", "project", func(s *Session) { s.StartedAt = new("2024-06-01T09:00:00Z"); s.MessageCount = 2 })
	addDashboardCacheSessions(t, d, 4)
	insertMessages(t, d, userMsgAt("s", 0, "hello", "2024-06-01T09:00:00Z"), asstMsgAt("s", 1, "answer", "2024-06-01T09:01:00Z"))
	f := AnalyticsFilter{Timezone: "UTC"}
	before, err := d.GetAnalyticsVelocity(ctx, f)
	require.NoError(t, err)
	require.InDelta(t, 6, before.Overall.CharsPerActiveMin, 0)
	var revision string
	require.NoError(t, d.getReader().QueryRowContext(ctx, `SELECT transcript_revision FROM sessions WHERE id='s'`).Scan(&revision))
	other, err := sql.Open("sqlite3", makeDSN(d.Path(), false))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, other.Close()) })
	_, err = other.ExecContext(ctx, `UPDATE messages SET content_length=20 WHERE session_id='s' AND ordinal=1`)
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

func TestDashboardPartsArchiveStripInvalidatesVelocity(t *testing.T) {
	t.Setenv("CACHE_DIRECTORY", t.TempDir())
	d := testDB(t)
	ctx := t.Context()
	insertSession(t, d, "s", "project", func(s *Session) { s.StartedAt = new("2024-06-01T09:00:00Z"); s.MessageCount = 2 })
	answer := asstMsgAt("s", 1, "answer", "2024-06-01T09:01:00Z")
	answer.SourceSubtype = "tool_result"
	insertMessages(t, d, userMsgAt("s", 0, "hello", "2024-06-01T09:00:00Z"), answer)
	f := AnalyticsFilter{Timezone: "UTC"}
	before, err := d.GetAnalyticsVelocity(ctx, f)
	require.NoError(t, err)
	require.InDelta(t, 6, before.Overall.CharsPerActiveMin, 0)
	tx, err := d.getWriter().BeginTx(ctx, nil)
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx, `CREATE TEMP TABLE selected_copy_ids (id TEXT); INSERT INTO selected_copy_ids VALUES ('s')`)
	require.NoError(t, err)
	require.NoError(t, dropCopiedToolContentTx(ctx, tx, "selected_copy_ids"))
	require.NoError(t, tx.Commit())
	after, err := d.GetAnalyticsVelocity(ctx, f)
	require.NoError(t, err)
	assert.Zero(t, after.Overall.CharsPerActiveMin)
}

func TestDashboardPartsPrunesDeletedSessions(t *testing.T) {
	t.Setenv("CACHE_DIRECTORY", t.TempDir())
	d := testDB(t)
	ctx := t.Context()
	insertSession(t, d, "a", "project", func(s *Session) { s.Agent = "codex" })
	insertSession(t, d, "b", "project", func(s *Session) { s.Agent = "gemini" })
	addDashboardCacheSessions(t, d, 4)
	_, _, handled, err := d.dashboardParts(ctx, AnalyticsFilter{Timezone: "UTC"})
	require.NoError(t, err)
	require.True(t, handled)
	require.NoError(t, d.DeleteSession(ctx, "a"))
	_, _, handled, err = d.dashboardParts(ctx, AnalyticsFilter{Timezone: "UTC"})
	require.NoError(t, err)
	require.True(t, handled)
	identity, err := d.GetDatabaseID(ctx)
	require.NoError(t, err)
	parts := kept.LoadParts[dashboardPart](&d.trendsDisk, dashboardTestSlot(t, AnalyticsFilter{Timezone: "UTC"}), fmt.Sprintf("%s|%d", identity, dashboardPartsVersion))
	assert.NotContains(t, parts, "a")
	assert.Contains(t, parts, "b")
}

func TestDashboardPartsQueuedFillsDoNotHoldBulkPermits(t *testing.T) {
	t.Setenv("CACHE_DIRECTORY", t.TempDir())
	d := testDB(t)
	insertSession(t, d, "a", "project", func(s *Session) { s.Agent = "codex" })
	insertSession(t, d, "b", "project", func(s *Session) { s.Agent = "gemini" })
	entered := make(chan struct{}, 2)
	unblock := make(chan struct{})
	dashboardPartsBeforeValidationObserver = func(ctx context.Context) {
		entered <- struct{}{}
		select {
		case <-unblock:
		case <-ctx.Done():
		}
	}
	t.Cleanup(func() { dashboardPartsBeforeValidationObserver = nil })
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
	//nolint:kennlint // deadlinetest: detects readers blocked by fills.
	guard, stop := context.WithTimeout(t.Context(), 5*time.Second)
	defer stop()
	release, err := d.acquireBulkRead(guard)
	if err == nil {
		release()
	}
	cancel()
	close(unblock)
	for range 2 {
		<-results
	}
	require.NoError(t, err)
}

func TestDashboardPartsToolChangeWithoutTranscriptRevision(t *testing.T) {
	t.Setenv("CACHE_DIRECTORY", t.TempDir())
	d := testDB(t)
	ctx := t.Context()
	insertSession(t, d, "s", "project", func(s *Session) { s.StartedAt = new("2024-06-01T09:00:00Z"); s.MessageCount = 2 })
	addDashboardCacheSessions(t, d, 4)
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
	addDashboardCacheSessions(t, d, 4)
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

func TestDashboardPartsSmallFilteredSelection(t *testing.T) {
	t.Setenv("CACHE_DIRECTORY", t.TempDir())
	d := testDB(t)
	ctx := t.Context()
	insertSession(t, d, "s", "project", func(s *Session) { s.Agent = "codex"; s.StartedAt = new("2024-06-01T09:00:00Z"); s.MessageCount = 32 })
	messages := make([]Message, 32)
	for i := range messages {
		messages[i] = userMsgAt("s", i, "hello", "2024-06-01T09:00:00Z")
	}
	insertMessages(t, d, messages...)
	for _, f := range []AnalyticsFilter{{Timezone: "UTC"}, {Timezone: "UTC", Agent: "codex"}, {Timezone: "UTC", Project: "project"}, {Timezone: "UTC", From: "2024-06-01", To: "2024-06-01"}} {
		_, _, handled, err := d.dashboardParts(ctx, f)
		require.NoError(t, err)
		assert.True(t, handled)
		hours, err := d.GetAnalyticsHourOfWeek(ctx, f)
		require.NoError(t, err)
		require.Len(t, hours.Cells, 168)
		assert.Equal(t, HourOfWeekCell{DayOfWeek: 5, Hour: 9, Messages: 32}, hours.Cells[129])
	}
	insertMessages(t, d, userMsgAt("s", 32, "hello", "2024-06-01T09:00:00Z"))
	for _, f := range []AnalyticsFilter{{Timezone: "UTC"}, {Timezone: "UTC", Agent: "codex"}, {Timezone: "UTC", Project: "project"}, {Timezone: "UTC", From: "2024-06-01", To: "2024-06-01"}} {
		_, parts, handled, err := d.dashboardParts(ctx, f)
		require.NoError(t, err)
		require.True(t, handled)
		assert.Equal(t, 33, parts["s"].Value.Messages)
	}
}

func TestDashboardPartsSmallSelectionAcrossSessions(t *testing.T) {
	t.Setenv("CACHE_DIRECTORY", t.TempDir())
	d := testDB(t)
	ctx := t.Context()
	for i := range 4 {
		id := fmt.Sprintf("s%d", i)
		insertSession(t, d, id, "project", func(s *Session) { s.Agent = "codex" })
		messages := make([]Message, 8)
		for n := range messages {
			messages[n] = userMsg(id, n, "hello")
		}
		insertMessages(t, d, messages...)
	}
	f := AnalyticsFilter{Timezone: "UTC", Project: "project"}
	_, _, handled, err := d.dashboardParts(ctx, f)
	require.NoError(t, err)
	assert.True(t, handled)
	insertMessages(t, d, userMsg("s0", 8, "hello"))
	_, parts, handled, err := d.dashboardParts(ctx, f)
	require.NoError(t, err)
	require.True(t, handled)
	assert.Equal(t, 9, parts["s0"].Value.Messages)
}
