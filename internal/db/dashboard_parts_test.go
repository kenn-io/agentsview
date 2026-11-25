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
		_, _, handled, err := d.dashboardParts(ctx, filter)
		require.NoError(t, err)
		if filter.Hour != nil || filter.Model != "" {
			require.False(t, handled)
		} else {
			require.True(t, handled)
		}
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
	_, err := d.getWriter().ExecContext(ctx, `UPDATE sessions SET message_count=2 WHERE id='s0'`)
	require.NoError(t, err)
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

func TestDashboardPartsDiskPreservesZeroFirstResponse(t *testing.T) {
	t.Setenv("CACHE_DIRECTORY", t.TempDir())
	d := testDB(t)
	ctx := t.Context()
	for i, seconds := range []int{0, 10, 60, 10, 60, -1} {
		id := fmt.Sprintf("s%d", i)
		insertSession(t, d, id, "project", func(s *Session) {
			s.StartedAt = new("2024-06-01T09:00:00Z")
			s.MessageCount = 2
			s.Agent = "zero"
			if i >= 3 {
				s.Agent = "missing"
			}
		})
		insertMessages(t, d, userMsgAt(id, 0, "hello", "2024-06-01T09:00:00Z"))
		if seconds >= 0 {
			ts := time.Date(2024, 6, 1, 9, 0, seconds, 0, time.UTC).Format(time.RFC3339)
			insertMessages(t, d, asstMsgAt(id, 1, "answer", ts))
		} else {
			insertMessages(t, d, userMsgAt(id, 1, "again", "2024-06-01T09:01:00Z"))
		}
	}
	f := AnalyticsFilter{Timezone: "UTC"}
	_, _, handled, err := d.dashboardParts(ctx, f)
	require.NoError(t, err)
	require.True(t, handled)
	reopened, err := OpenIsolated(ctx, d.Path())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, reopened.Close()) })
	var reads int
	dashboardPartsReadObserver = func([]string) { reads++ }
	t.Cleanup(func() { dashboardPartsReadObserver = nil })
	got, err := reopened.GetAnalyticsVelocity(ctx, f)
	require.NoError(t, err)
	assert.Zero(t, reads)
	reopened.readOnly = true
	want, err := reopened.GetAnalyticsVelocity(ctx, f)
	require.NoError(t, err)
	require.Len(t, got.ByAgent, 2)
	assert.Equal(t, "missing", got.ByAgent[0].Label)
	assert.InDelta(t, 60, got.ByAgent[0].Overview.FirstResponseSec.P50, 0)
	assert.Equal(t, "zero", got.ByAgent[1].Label)
	assert.InDelta(t, 10, got.ByAgent[1].Overview.FirstResponseSec.P50, 0)
	assert.Equal(t, want, got)
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
	var reads atomic.Int32
	dashboardPartsReadObserver = func([]string) {
		if reads.Add(1) == 1 {
			entered <- struct{}{}
			<-unblock
		}
	}
	t.Cleanup(func() { dashboardPartsReadObserver = nil })
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

func TestDashboardPartsRawMessageEdits(t *testing.T) {
	for _, tc := range []struct {
		name, query           string
		user, messages, tools int
		chars                 float64
	}{
		{"insert", `INSERT INTO messages(session_id,ordinal,role,content,content_length,timestamp) VALUES('s',2,'assistant','later',20,'2024-06-01T09:02:00Z')`, 1, 3, 1, 13},
		{"delete", `DELETE FROM messages WHERE session_id='s' AND ordinal=1`, 1, 1, 0, 0},
		{"length", `UPDATE messages SET content_length=20 WHERE session_id='s' AND ordinal=1`, 1, 2, 1, 20},
		{"thinking", `UPDATE messages SET has_thinking=1 WHERE session_id='s' AND ordinal=1`, 1, 2, 1, 6},
		{"tool", `DELETE FROM tool_calls WHERE session_id='s'`, 1, 2, 0, 6},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("CACHE_DIRECTORY", t.TempDir())
			d := testDB(t)
			ctx := t.Context()
			insertSession(t, d, "s", "project", func(s *Session) { s.StartedAt = new("2024-06-01T09:00:00Z"); s.MessageCount = 2 })
			assistant := asstMsgAt("s", 1, "answer", "2024-06-01T09:01:00Z")
			assistant.ToolCalls = []ToolCall{{SessionID: "s", ToolName: "Read", Category: "Read"}}
			insertMessages(t, d, userMsgAt("s", 0, "hello", "2024-06-01T09:00:00Z"), assistant)
			f := AnalyticsFilter{Timezone: "UTC"}
			before, err := d.GetAnalyticsActivity(ctx, f, "day")
			require.NoError(t, err)
			require.Len(t, before.Series, 1)
			require.Equal(t, 1, before.Series[0].ToolCalls)
			_, err = d.GetAnalyticsVelocity(ctx, f)
			require.NoError(t, err)
			other, err := sql.Open("sqlite3", makeDSN(d.Path(), false))
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, other.Close()) })
			_, err = other.ExecContext(ctx, tc.query)
			require.NoError(t, err)
			activity, err := d.GetAnalyticsActivity(ctx, f, "day")
			require.NoError(t, err)
			velocity, err := d.GetAnalyticsVelocity(ctx, f)
			require.NoError(t, err)
			require.Len(t, activity.Series, 1)
			assert.Equal(t, tc.user, activity.Series[0].UserMessages)
			assert.Equal(t, tc.messages, activity.Series[0].Messages)
			assert.Equal(t, tc.tools, activity.Series[0].ToolCalls)
			assert.InDelta(t, tc.chars, velocity.Overall.CharsPerActiveMin, 0)
			d.readOnly = true
			scanActivity, err := d.GetAnalyticsActivity(ctx, f, "day")
			require.NoError(t, err)
			scanVelocity, err := d.GetAnalyticsVelocity(ctx, f)
			require.NoError(t, err)
			d.readOnly = false
			assert.Equal(t, scanActivity, activity)
			assert.Equal(t, scanVelocity, velocity)
		})
	}
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

func TestDashboardPartsReleasesScanPermitBeforeFill(t *testing.T) {
	t.Setenv("CACHE_DIRECTORY", t.TempDir())
	d := testDB(t)
	insertSession(t, d, "a", "project", func(s *Session) { s.Agent = "codex" })
	insertSession(t, d, "b", "project", func(s *Session) { s.Agent = "gemini" })
	entered := make(chan struct{}, 2)
	unblock := make(chan struct{})
	ctx, cancel := context.WithCancel(t.Context())
	dashboardPartsReadObserver = func([]string) {
		entered <- struct{}{}
		select {
		case <-unblock:
		case <-ctx.Done():
		}
	}
	t.Cleanup(func() { dashboardPartsReadObserver = nil })
	results := make(chan error, 2)
	for _, agent := range []string{"codex", "gemini"} {
		go func() {
			_, err := d.GetAnalyticsHourOfWeek(ctx, AnalyticsFilter{Timezone: "UTC", Agent: agent})
			results <- err
		}()
	}
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
				require.ErrorIs(t, <-results, context.Canceled)
			}
			require.Fail(t, "fills could not both start")
		}
	}
	cancel()
	close(unblock)
	for range 2 {
		require.ErrorIs(t, <-results, context.Canceled)
	}
	permitCtx, stopPermits := context.WithTimeout(t.Context(), 5*time.Second)
	defer stopPermits()
	for range bulkReadSlots {
		release, err := d.acquireBulkRead(permitCtx)
		require.NoError(t, err)
		t.Cleanup(release)
	}
}

func TestDashboardPartsMemoRespectsSizeLimit(t *testing.T) {
	t.Run("oversized part", func(t *testing.T) {
		var memo dashboardPartsMemo
		small := kept.Part[dashboardPart]{ID: "a", Fingerprint: "v"}
		large := kept.Part[dashboardPart]{ID: "b", Fingerprint: "v", Value: dashboardPart{Velocity: SessionVelocity{TurnCycles: make([]float64, 8_388_592)}}}
		require.EqualValues(t, kept.DefaultMaxBytes+2, dashboardPartBytes(large))
		memo.store("UTC", "version", map[string]kept.Part[dashboardPart]{"a": small})
		memo.store("UTC", "version", map[string]kept.Part[dashboardPart]{"b": large})
		parts := make(map[string]kept.Part[dashboardPart])
		missing := memo.reuse("UTC", "version", []string{"a", "b"}, map[string]string{"a": "v", "b": "v"}, parts)
		assert.Equal(t, []string{"b"}, missing)
		assert.Equal(t, map[string]kept.Part[dashboardPart]{"a": small}, parts)
	})
	t.Run("successive fills", func(t *testing.T) {
		var memo dashboardPartsMemo
		versions := make(map[string]string, 24_000)
		ids := make([]string, 0, 24_000)
		for batch := range 2 {
			fill := make(map[string]kept.Part[dashboardPart], 12_000)
			for i := range 12_000 {
				id := fmt.Sprintf("s%05d", batch*12_000+i)
				hours := make(map[int]int, 168)
				for hour := range 168 {
					hours[hour] = 1
				}
				fill[id] = kept.Part[dashboardPart]{ID: id, Fingerprint: "v", Value: dashboardPart{Hours: hours}}
				ids = append(ids, id)
				versions[id] = "v"
			}
			memo.store("UTC", "version", fill)
			assert.LessOrEqual(t, memo.bytes, int64(kept.DefaultMaxBytes))
		}
		parts := make(map[string]kept.Part[dashboardPart])
		missing := memo.reuse("UTC", "version", ids, versions, parts)
		assert.NotEmpty(t, missing)
		assert.NotEmpty(t, parts)
	})
}

func TestDashboardPartsWorksWhenWriterClosed(t *testing.T) {
	t.Setenv("CACHE_DIRECTORY", t.TempDir())
	d := testDB(t)
	ctx := t.Context()
	insertSession(t, d, "s", "project", func(s *Session) {
		s.StartedAt = new("2024-06-01T09:00:00Z")
		s.MessageCount = 2
	})
	assistant := asstMsgAt("s", 1, "answer", "2024-06-01T09:01:00Z")
	assistant.ToolCalls = []ToolCall{{SessionID: "s", ToolName: "Read", Category: "Read"}}
	insertMessages(t, d, userMsgAt("s", 0, "hello", "2024-06-01T09:00:00Z"), assistant)
	_, err := d.getWriter().ExecContext(ctx, `UPDATE sessions SET tools_generation = NULL WHERE id = 's'`)
	require.NoError(t, err)
	f := AnalyticsFilter{Timezone: "UTC"}

	// Record the direct reads without building dashboard parts or stamping the dirty session.
	d.readOnly = true
	activity, err := d.GetAnalyticsActivity(ctx, f, "day")
	require.NoError(t, err)
	hours, err := d.GetAnalyticsHourOfWeek(ctx, f)
	require.NoError(t, err)
	velocity, err := d.GetAnalyticsVelocity(ctx, f)
	require.NoError(t, err)
	d.readOnly = false
	require.Len(t, activity.Series, 1)
	assert.Equal(t, 2, activity.Series[0].Messages)
	assert.Equal(t, 1, activity.Series[0].ToolCalls)
	require.NoError(t, d.CloseWriter())
	_, _, handled, err := d.dashboardParts(ctx, f)
	require.NoError(t, err)
	require.True(t, handled)

	t.Run("activity", func(t *testing.T) {
		got, err := d.GetAnalyticsActivity(ctx, f, "day")
		require.NoError(t, err)
		assert.Equal(t, activity, got)
	})
	t.Run("hour of week", func(t *testing.T) {
		got, err := d.GetAnalyticsHourOfWeek(ctx, f)
		require.NoError(t, err)
		assert.Equal(t, hours, got)
	})
	t.Run("velocity", func(t *testing.T) {
		got, err := d.GetAnalyticsVelocity(ctx, f)
		require.NoError(t, err)
		assert.Equal(t, velocity, got)
	})
}

func TestDashboardPartsDoNotWaitOnWriterLock(t *testing.T) {
	t.Setenv("CACHE_DIRECTORY", t.TempDir())
	d := testDB(t)
	ctx := t.Context()
	insertSession(t, d, "s", "project", func(s *Session) { s.StartedAt = new("2024-06-01T09:00:00Z"); s.MessageCount = 2 })
	insertMessages(t, d, userMsgAt("s", 0, "hello", "2024-06-01T09:00:00Z"), asstMsgAt("s", 1, "answer", "2024-06-01T09:01:00Z"))
	f := AnalyticsFilter{Timezone: "UTC"}
	d.readOnly = true
	activity, err := d.GetAnalyticsActivity(ctx, f, "day")
	require.NoError(t, err)
	hours, err := d.GetAnalyticsHourOfWeek(ctx, f)
	require.NoError(t, err)
	velocity, err := d.analyticsVelocity(ctx, f)
	require.NoError(t, err)
	d.readOnly = false
	d.mu.Lock()
	done := make(chan struct{}, 3)
	go func() {
		defer func() { done <- struct{}{} }()
		got, err := d.GetAnalyticsActivity(ctx, f, "day")
		assert.NoError(t, err)
		assert.Equal(t, activity, got)
	}()
	go func() {
		defer func() { done <- struct{}{} }()
		got, err := d.GetAnalyticsHourOfWeek(ctx, f)
		assert.NoError(t, err)
		assert.Equal(t, hours, got)
	}()
	go func() {
		defer func() { done <- struct{}{} }()
		got, err := d.analyticsVelocity(ctx, f)
		assert.NoError(t, err)
		assert.Equal(t, velocity, got)
	}()
	guard := time.NewTimer(5 * time.Second)
	defer guard.Stop()
	for i := range 3 {
		select {
		case <-done:
		case <-guard.C:
			d.mu.Unlock()
			for range 3 - i {
				<-done
			}
			t.Fatal("dashboard reads waited on the writer lock")
		}
	}
	d.mu.Unlock()
	for range bulkReadSlots {
		release, err := d.acquireBulkRead(ctx)
		require.NoError(t, err)
		t.Cleanup(release)
	}
}

func TestDashboardPartsDirtySessionReuseAndInvalidation(t *testing.T) {
	t.Setenv("CACHE_DIRECTORY", t.TempDir())
	d := testDB(t)
	ctx := t.Context()
	insertSession(t, d, "s", "project", func(s *Session) { s.StartedAt = new("2024-06-01T09:00:00Z"); s.MessageCount = 2 })
	user := userMsgAt("s", 0, "hello", "2024-06-01T09:00:00Z")
	assistant := asstMsgAt("s", 1, "answer", "2024-06-01T09:01:00Z")
	insertMessages(t, d, user, assistant)
	var reads [][]string
	dashboardPartsReadObserver = func(ids []string) { reads = append(reads, append([]string(nil), ids...)) }
	t.Cleanup(func() { dashboardPartsReadObserver = nil })
	f := AnalyticsFilter{Timezone: "UTC"}
	_, err := d.GetAnalyticsActivity(ctx, f, "day")
	require.NoError(t, err)
	_, err = d.GetAnalyticsActivity(ctx, f, "day")
	require.NoError(t, err)
	f.Project = "project"
	_, err = d.GetAnalyticsActivity(ctx, f, "day")
	require.NoError(t, err)
	assert.Equal(t, [][]string{{"s"}}, reads)
	_, err = d.getWriter().ExecContext(ctx, `UPDATE messages SET content_length=200 WHERE session_id='s' AND ordinal=1`)
	require.NoError(t, err)
	got, err := d.analyticsVelocity(ctx, f)
	require.NoError(t, err)
	d.readOnly = true
	want, err := d.analyticsVelocity(ctx, f)
	require.NoError(t, err)
	assert.Equal(t, want, got)
	d.readOnly = false
	_, err = d.getWriter().ExecContext(ctx, `UPDATE messages SET has_thinking=1 WHERE session_id='s' AND ordinal=1`)
	require.NoError(t, err)
	activity, err := d.GetAnalyticsActivity(ctx, f, "day")
	require.NoError(t, err)
	d.readOnly = true
	scan, err := d.GetAnalyticsActivity(ctx, f, "day")
	require.NoError(t, err)
	assert.Equal(t, scan, activity)
	assert.Len(t, reads, 3)
}

func TestDashboardPartsDirtyFillChangesAfterWrite(t *testing.T) {
	t.Setenv("CACHE_DIRECTORY", t.TempDir())
	d := testDB(t)
	ctx := t.Context()
	insertSession(t, d, "s", "project", func(s *Session) { s.StartedAt = new("2024-06-01T09:00:00Z"); s.MessageCount = 2 })
	insertMessages(t, d, userMsgAt("s", 0, "hello", "2024-06-01T09:00:00Z"), asstMsgAt("s", 1, "answer", "2024-06-01T09:01:00Z"))
	entered, unblock := make(chan struct{}), make(chan struct{})
	var reads atomic.Int32
	dashboardPartsReadObserver = func([]string) {
		if reads.Add(1) == 1 {
			close(entered)
			<-unblock
		}
	}
	t.Cleanup(func() { dashboardPartsReadObserver = nil })
	f := AnalyticsFilter{Timezone: "UTC"}
	done := make(chan error, 1)
	go func() { _, err := d.GetAnalyticsVelocity(ctx, f); done <- err }()
	<-entered
	_, err := d.getWriter().ExecContext(ctx, `UPDATE messages SET content_length=200 WHERE session_id='s' AND ordinal=1`)
	if err != nil {
		close(unblock)
		<-done
		require.NoError(t, err)
	}
	type result struct {
		value VelocityResponse
		err   error
	}
	second := make(chan result, 1)
	go func() { value, err := d.GetAnalyticsVelocity(ctx, f); second <- result{value, err} }()
	var got result
	select {
	case got = <-second:
	case <-time.After(5 * time.Second):
		close(unblock)
		<-done
		<-second
		t.Fatal("new request joined the stale fill")
	}
	close(unblock)
	require.NoError(t, <-done)
	require.NoError(t, got.err)
	d.readOnly = true
	want, err := d.analyticsVelocity(ctx, f)
	require.NoError(t, err)
	d.readOnly = false
	assert.Equal(t, want, got.value)
	again, err := d.GetAnalyticsVelocity(ctx, f)
	require.NoError(t, err)
	assert.Equal(t, want, again)
}

func TestDashboardPartsUnrelatedWriteKeepsSelection(t *testing.T) {
	t.Setenv("CACHE_DIRECTORY", t.TempDir())
	d := testDB(t)
	ctx := t.Context()
	for _, id := range []string{"a", "b"} {
		insertSession(t, d, id, id, func(s *Session) { s.StartedAt = new("2024-06-01T09:00:00Z") })
		insertMessages(t, d, userMsgAt(id, 0, "hello", "2024-06-01T09:00:00Z"))
	}
	f := AnalyticsFilter{Timezone: "UTC", Project: "a"}
	_, _, _, err := d.dashboardParts(ctx, f)
	require.NoError(t, err)
	insertMessages(t, d, asstMsgAt("b", 1, "answer", "2024-06-01T09:01:00Z"))
	var reads int
	dashboardPartsReadObserver = func([]string) { reads++ }
	t.Cleanup(func() { dashboardPartsReadObserver = nil })
	_, _, _, err = d.dashboardParts(ctx, f)
	require.NoError(t, err)
	assert.Zero(t, reads)
}

func TestDashboardPartsToolsReadKeepsParts(t *testing.T) {
	t.Setenv("CACHE_DIRECTORY", t.TempDir())
	d := testDB(t)
	ctx := t.Context()
	insertSession(t, d, "s", "project")
	insertMessages(t, d, userMsgAt("s", 0, "hello", "2024-06-01T09:00:00Z"))
	var reads int
	dashboardPartsReadObserver = func([]string) { reads++ }
	t.Cleanup(func() { dashboardPartsReadObserver = nil })
	f := AnalyticsFilter{Timezone: "UTC"}
	_, err := d.GetAnalyticsActivity(ctx, f, "day")
	require.NoError(t, err)
	_, err = d.GetAnalyticsTools(ctx, f)
	require.NoError(t, err)
	_, err = d.GetAnalyticsActivity(ctx, f, "day")
	require.NoError(t, err)
	assert.Equal(t, 1, reads)
}
