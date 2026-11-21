//go:build fts5

package db

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/kept"
)

func dashboardTestSlot(t *testing.T, f AnalyticsFilter) string {
	t.Helper()
	data, err := json.Marshal(f, json.Deterministic(true))
	require.NoError(t, err)
	return "dashboard-parts|" + TrendsTimezoneKey(f) + "|" + string(data)
}

func TestDashboardPartsMatchScans(t *testing.T) {
	t.Setenv("CACHE_DIRECTORY", t.TempDir())
	d := testDB(t)
	ctx := t.Context()
	for _, id := range []string{"a", "b", "empty"} {
		insertSession(t, d, id, "project", func(s *Session) {
			s.StartedAt = new("2024-06-01T09:00:00Z")
			switch id {
			case "a":
				s.MessageCount = 2
			case "b":
				s.MessageCount = 3
			case "empty":
				s.MessageCount = 0
			}
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
	check(AnalyticsFilter{Timezone: "UTC", Agent: "codex"})
	insertMessages(t, d, asstMsgAt("a", 2, "later", "2024-06-01T09:02:00Z"))
	_, err = d.getWriter().ExecContext(ctx, `UPDATE sessions SET message_count=3 WHERE id='a'`)
	require.NoError(t, err)
	check(f)
	b.ToolCalls = append(b.ToolCalls, ToolCall{SessionID: "a", ToolName: "Write", Category: "Write"})
	require.NoError(t, d.ReplaceSessionMessages(ctx, "a", []Message{a, b}))
	_, err = d.getWriter().ExecContext(ctx, `UPDATE sessions SET message_count=2 WHERE id='a'`)
	require.NoError(t, err)
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

func TestDashboardPartsReuseAcrossAgentAndDateFilters(t *testing.T) {
	t.Setenv("CACHE_DIRECTORY", t.TempDir())
	d := testDB(t)
	ctx := t.Context()
	for i := range 6 {
		id := fmt.Sprintf("s%d", i)
		insertSession(t, d, id, "project", func(s *Session) {
			s.StartedAt = new("2024-06-01T09:00:00Z")
			if i < 3 {
				s.Agent = "codex"
			} else {
				s.Agent = "gemini"
			}
		})
		insertMessages(t, d, userMsgAt(id, 0, "hello", "2024-06-01T09:00:00Z"))
	}
	var reads [][]string
	dashboardPartsReadObserver = func(ids []string) { reads = append(reads, append([]string(nil), ids...)) }
	t.Cleanup(func() { dashboardPartsReadObserver = nil })
	for _, f := range []AnalyticsFilter{{Timezone: "UTC"}, {Timezone: "UTC", Agent: "codex"}, {Timezone: "UTC", From: "2024-06-01", To: "2024-06-01"}} {
		_, parts, handled, err := d.dashboardParts(ctx, f)
		require.NoError(t, err)
		require.True(t, handled)
		if f.Agent != "" {
			require.Len(t, parts, 3)
		} else {
			require.Len(t, parts, 6)
		}
	}
	assert.Equal(t, [][]string{{"s0", "s1", "s2", "s3", "s4", "s5"}}, reads)
	// A filter with no saved file must not reuse the retained part of a changed session.
	insertMessages(t, d, asstMsgAt("s0", 1, "later", "2024-06-01T09:01:00Z"))
	_, parts, handled, err := d.dashboardParts(ctx, AnalyticsFilter{Timezone: "UTC", Project: "project"})
	require.NoError(t, err)
	require.True(t, handled)
	assert.Equal(t, 2, parts["s0"].Value.Messages)
	assert.Equal(t, [][]string{{"s0", "s1", "s2", "s3", "s4", "s5"}, {"s0"}}, reads)
	// Save the updated full selection, then simulate an empty in-process memo.
	_, _, handled, err = d.dashboardParts(ctx, AnalyticsFilter{Timezone: "UTC"})
	require.NoError(t, err)
	require.True(t, handled)
	d.dashboardMemo.slot, d.dashboardMemo.version, d.dashboardMemo.parts, d.dashboardMemo.bytes = "", "", nil, 0
	_, _, handled, err = d.dashboardParts(ctx, AnalyticsFilter{Timezone: "UTC"})
	require.NoError(t, err)
	require.True(t, handled)
	_, _, handled, err = d.dashboardParts(ctx, AnalyticsFilter{Timezone: "UTC", Agent: "gemini"})
	require.NoError(t, err)
	require.True(t, handled)
	assert.Equal(t, [][]string{{"s0", "s1", "s2", "s3", "s4", "s5"}, {"s0"}}, reads)
}

func TestDashboardPartsWriteDuringFillFallsBack(t *testing.T) {
	t.Run("selected message", func(t *testing.T) {
		t.Setenv("CACHE_DIRECTORY", t.TempDir())
		d := testDB(t)
		ctx := t.Context()
		insertSession(t, d, "s", "project", func(s *Session) { s.StartedAt = new("2024-06-01T09:00:00Z") })
		insertMessages(t, d, userMsgAt("s", 0, "hello", "2024-06-01T09:00:00Z"))
		dashboardPartsBeforeValidationObserver = func(context.Context) {
			dashboardPartsBeforeValidationObserver = nil
			insertMessages(t, d, asstMsgAt("s", 1, "answer", "2024-06-01T09:01:00Z"))
			_, err := d.getWriter().ExecContext(ctx, `UPDATE sessions SET message_count=2 WHERE id='s'`)
			require.NoError(t, err)
		}
		t.Cleanup(func() { dashboardPartsBeforeValidationObserver = nil })
		result, err := d.GetAnalyticsActivity(ctx, AnalyticsFilter{Timezone: "UTC"}, "day")
		require.NoError(t, err)
		require.Len(t, result.Series, 1)
		assert.Equal(t, 2, result.Series[0].Messages)
		identity, err := d.GetDatabaseID(ctx)
		require.NoError(t, err)
		parts := kept.LoadParts[dashboardPart](&d.trendsDisk, dashboardTestSlot(t, AnalyticsFilter{Timezone: "UTC"}), fmt.Sprintf("%s|%d", identity, dashboardPartsVersion))
		assert.NotContains(t, parts, "s")
		var reads [][]string
		dashboardPartsReadObserver = func(ids []string) { reads = append(reads, append([]string(nil), ids...)) }
		t.Cleanup(func() { dashboardPartsReadObserver = nil })
		_, err = d.GetAnalyticsActivity(ctx, AnalyticsFilter{Timezone: "UTC"}, "day")
		require.NoError(t, err)
		assert.Equal(t, [][]string{{"s"}}, reads)
	})
	t.Run("unselected message", func(t *testing.T) {
		t.Setenv("CACHE_DIRECTORY", t.TempDir())
		d := testDB(t)
		ctx := t.Context()
		insertSession(t, d, "selected", "project", func(s *Session) { s.Agent = "codex" })
		insertSession(t, d, "other", "project", func(s *Session) { s.Agent = "gemini"; s.MessageCount = 0 })
		insertMessages(t, d, userMsg("selected", 0, "hello"))
		dashboardPartsBeforeValidationObserver = func(context.Context) {
			dashboardPartsBeforeValidationObserver = nil
			insertMessages(t, d, userMsg("other", 0, "elsewhere"))
			_, err := d.getWriter().ExecContext(ctx, `UPDATE sessions SET message_count=1 WHERE id='other'`)
			require.NoError(t, err)
		}
		t.Cleanup(func() { dashboardPartsBeforeValidationObserver = nil })
		sessions, parts, handled, err := d.dashboardParts(ctx, AnalyticsFilter{Timezone: "UTC", Agent: "codex"})
		require.NoError(t, err)
		require.True(t, handled)
		require.Len(t, sessions, 1)
		assert.Equal(t, 1, parts["selected"].Value.Messages)
		identity, err := d.GetDatabaseID(ctx)
		require.NoError(t, err)
		_, ok := d.trendsDisk.Get(dashboardTestSlot(t, AnalyticsFilter{Timezone: "UTC", Agent: "codex"}), fmt.Sprintf("%s|%d", identity, dashboardPartsVersion))
		assert.True(t, ok)
	})
	t.Run("selected metadata", func(t *testing.T) {
		t.Setenv("CACHE_DIRECTORY", t.TempDir())
		d := testDB(t)
		ctx := t.Context()
		insertSession(t, d, "s", "project", func(s *Session) { s.Agent = "codex" })
		insertMessages(t, d, userMsg("s", 0, "hello"))
		dashboardPartsBeforeValidationObserver = func(context.Context) {
			dashboardPartsBeforeValidationObserver = nil
			_, err := d.getWriter().ExecContext(ctx, `UPDATE sessions SET agent='gemini' WHERE id='s'`)
			require.NoError(t, err)
		}
		t.Cleanup(func() { dashboardPartsBeforeValidationObserver = nil })
		_, _, handled, err := d.dashboardParts(ctx, AnalyticsFilter{Timezone: "UTC"})
		require.NoError(t, err)
		assert.False(t, handled)
	})
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

func TestDashboardPartsConcurrentPanels(t *testing.T) {
	t.Setenv("CACHE_DIRECTORY", t.TempDir())
	d := testDB(t)
	insertSession(t, d, "s", "project", func(s *Session) { s.StartedAt = new("2024-06-01T09:00:00Z"); s.MessageCount = 2 })
	insertMessages(t, d, userMsgAt("s", 0, "hello", "2024-06-01T09:00:00Z"), asstMsgAt("s", 1, "answer", "2024-06-01T09:01:00Z"))
	ctx := t.Context()
	f := AnalyticsFilter{Timezone: "UTC"}
	start := make(chan struct{})
	started := make(chan struct{}, 3)
	entered := make(chan struct{}, 3)
	unblock := make(chan struct{})
	results := make(chan error, 3)
	var reads, fills atomic.Int32
	dashboardPartsReadObserver = func([]string) { reads.Add(1) }
	dashboardPartsBeforeValidationObserver = func(context.Context) {
		if fills.Add(1) == 1 {
			entered <- struct{}{}
			<-unblock
		}
	}
	t.Cleanup(func() { dashboardPartsReadObserver = nil; dashboardPartsBeforeValidationObserver = nil })
	go func() {
		<-start
		started <- struct{}{}
		_, err := d.GetAnalyticsActivity(ctx, f, "day")
		results <- err
	}()
	go func() { <-start; started <- struct{}{}; _, err := d.GetAnalyticsHourOfWeek(ctx, f); results <- err }()
	go func() { <-start; started <- struct{}{}; _, err := d.GetAnalyticsVelocity(ctx, f); results <- err }()
	close(start)
	for range 3 {
		<-started
	}
	<-entered
	//nolint:kennlint // deadlinetest: panels must wait for the shared fill.
	var early bool
	select {
	case <-results:
		early = true
	case <-time.After(time.Second):
	}
	close(unblock)
	remaining := 3
	if early {
		remaining--
	}
	for range remaining {
		require.NoError(t, <-results)
	}
	assert.False(t, early, "a panel completed before the fill was released")
	assert.EqualValues(t, 1, reads.Load())
}

func TestDashboardPartsLengthUpdateAndReplacementWithoutRevision(t *testing.T) {
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
	//nolint:kennlint // deadlinetest: detect fills blocked by held reader permits.
	guard, stop := context.WithTimeout(t.Context(), 5*time.Second)
	defer stop()
	count := 0
	for count < 2 {
		select {
		case <-entered:
			count++
		case <-guard.Done():
			cancel()
			close(unblock)
			for range 2 {
				<-results
			}
			t.Fatal("fills could not both enter validation")
		}
	}
	cancel()
	close(unblock)
	for range 2 {
		<-results
	}
}

func TestDashboardPartsToolCallCountsRefresh(t *testing.T) {
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
	_, err = d.getWriter().ExecContext(ctx, `DELETE FROM tool_calls WHERE session_id='s'`)
	require.NoError(t, err)
	after, err := d.GetAnalyticsActivity(ctx, f, "day")
	require.NoError(t, err)
	assert.Zero(t, after.Series[0].ToolCalls)
	_, err = d.getWriter().ExecContext(ctx, `INSERT INTO tool_calls(session_id,message_id,tool_name,category) SELECT 's', id, 'Write', 'Write' FROM messages WHERE session_id='s' AND ordinal=1`)
	require.NoError(t, err)
	after, err = d.GetAnalyticsActivity(ctx, f, "day")
	require.NoError(t, err)
	assert.Equal(t, 1, after.Series[0].ToolCalls)
}

func TestDashboardPartsAgentChangeBeforeStamp(t *testing.T) {
	t.Setenv("CACHE_DIRECTORY", t.TempDir())
	d := testDB(t)
	ctx := t.Context()
	insertSession(t, d, "s", "project", func(s *Session) { s.Agent = "claude"; s.StartedAt = new("2024-06-01T09:00:00Z") })
	msg := asstMsgAt("s", 0, "answer", "2024-06-01T09:01:00Z")
	msg.ToolCalls = []ToolCall{{SessionID: "s", ToolName: "Read", Category: "Read"}}
	insertMessages(t, d, msg)
	other, err := sql.Open("sqlite3", makeDSN(d.Path(), false))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, other.Close()) })
	dashboardPartsBeforeStampObserver = func() {
		dashboardPartsBeforeStampObserver = nil
		tx, err := other.BeginTx(ctx, nil)
		require.NoError(t, err)
		_, err = tx.ExecContext(ctx, `UPDATE sessions SET agent='codex' WHERE id='s'`)
		require.NoError(t, err)
		require.NoError(t, tx.Commit())
	}
	t.Cleanup(func() { dashboardPartsBeforeStampObserver = nil })
	f := AnalyticsFilter{Timezone: "UTC"}
	activity, err := d.GetAnalyticsActivity(ctx, f, "day")
	require.NoError(t, err)
	require.Len(t, activity.Series, 1)
	assert.Equal(t, map[string]int{"codex": 1}, activity.Series[0].ByAgent)
}
