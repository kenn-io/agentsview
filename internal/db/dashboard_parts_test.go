//go:build fts5

package db

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
