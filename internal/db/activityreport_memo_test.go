package db

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/activity"
)

func TestActivityMemoReuseAndReset(t *testing.T) {
	for _, action := range []string{"recreate", "reopen", "compact", "stale store", "truncate"} {
		t.Run(action, func(t *testing.T) {
			d := testDB(t)
			insertSession(t, d, "s", "p")
			if action == "recreate" {
				_, err := d.getWriter().ExecContext(t.Context(), `UPDATE sessions SET created_at = '2026-06-01T00:00:00Z' WHERE id = 's'`)
				require.NoError(t, err)
			}
			seedMessage(t, d, "s", 0, "user", "2026-06-16T10:00:00Z", "")
			seedMessage(t, d, "s", 1, "assistant", "2026-06-16T10:02:00Z", "m")
			q := dayQuery(t, "2026-06-16", "UTC")
			before, err := d.activityReportCandidates(t.Context(), []string{"s"}, q)
			require.NoError(t, err)
			require.Len(t, before, 1)
			key := activityMemoKey{"s", q.RangeStart.Add(-time.Duration(q.GapCapSeconds) * time.Second).UnixMicro()}
			generation := d.activityMemo.generation
			entry := d.activityMemo.entries[key].Value.(activityMemoEntry)
			wantMinutes := 2.0
			wantCount := 1
			switch action {
			case "truncate":
				_, err := d.WriteSessionIncremental(t.Context(), "s", nil, IncrementalSessionUpdate{ReplaceFromOrdinal: new(1)})
				require.NoError(t, err)
				wantCount = 0
			case "recreate":
				_, err := d.DeleteParserExcludedSessions(t.Context(), []string{"s"})
				require.NoError(t, err)
				insertSession(t, d, "s", "p")
				seedMessage(t, d, "s", 0, "user", "2026-06-16T10:00:00Z", "")
				seedMessage(t, d, "s", 1, "assistant", "2026-06-16T10:04:00Z", "m")
				wantMinutes = 4
			case "reopen", "stale store":
				require.NoError(t, d.Reopen())
				if action == "stale store" {
					d.activityMemo.store(generation, entry)
				}
			case "compact":
				_, err := d.Compact(t.Context(), CompactOptions{StagingDir: t.TempDir()})
				require.NoError(t, err)
			}
			got, err := d.activityReportCandidates(t.Context(), []string{"s"}, q)
			require.NoError(t, err)
			require.Len(t, got, wantCount)
			if wantCount > 0 {
				assert.Equal(t, wantMinutes, got[0].End.Sub(got[0].Start).Minutes())
			}
		})
	}
}

func TestActivityMemoWindowAndAppend(t *testing.T) {
	d := testDB(t)
	insertSession(t, d, "s", "p")
	for ordinal, timestamp := range []string{"2026-06-16T10:00:00Z", "2026-06-16T10:02:00Z", "2026-06-16T10:04:00Z", "2026-06-17T10:00:00Z", "2026-06-17T10:02:00Z"} {
		seedMessage(t, d, "s", ordinal, "assistant", timestamp, "m")
	}
	q := dayQuery(t, "2026-06-16", "UTC")
	q.RangeEnd = q.RangeStart.Add(10*time.Hour + 3*time.Minute)
	q.EffectiveEnd = q.RangeStart.Add(10*time.Hour + time.Minute)
	got, err := d.activityReportCandidates(t.Context(), []string{"s"}, q)
	require.NoError(t, err)
	require.Len(t, got, 1)
	for _, e := range d.activityMemo.entries {
		assert.Len(t, e.Value.(activityMemoEntry).candidates, 2)
	}
	q.EffectiveEnd = q.EffectiveEnd.Add(2 * time.Minute)
	got, err = d.activityReportCandidates(t.Context(), []string{"s"}, q)
	require.NoError(t, err)
	require.Len(t, got, 2)
	assert.Equal(t, "2026-06-16T10:02:00Z", got[1].Start.Format(time.RFC3339))
	_, err = d.WriteSessionIncremental(t.Context(), "s", []Message{
		{SessionID: "s", Ordinal: 5, Role: "assistant", Content: "x", Timestamp: "2026-06-16T10:02:30Z", Model: "m"},
		{SessionID: "s", Ordinal: 6, Role: "assistant", Content: "x", Timestamp: "2026-06-16T10:03:00Z", Model: "m"},
	}, IncrementalSessionUpdate{})
	require.NoError(t, err)
	got, err = d.activityReportCandidates(t.Context(), []string{"s"}, q)
	require.NoError(t, err)
	require.Len(t, got, 3)
	assert.Equal(t, 5, got[2].StartOrdinal)
	var reference []activity.IntervalCandidate
	require.NoError(t, activityReportCandidateSQL(t, d, []string{"s"}, q)(t.Context(), func(c activity.IntervalCandidate) error { reference = append(reference, c); return nil }))
	assert.Equal(t, reference, got)
}

func TestActivityMemoOverflowSkipsReport(t *testing.T) {
	old := activityMemoMaxCandidates
	activityMemoMaxCandidates = 5
	t.Cleanup(func() { activityMemoMaxCandidates = old })
	d := testDB(t)
	for _, id := range []string{"large", "small"} {
		insertSession(t, d, id, "p")
		count := 2
		if id == "large" {
			count = 5
		}
		for i := range count {
			seedMessage(t, d, id, i, "assistant", time.Date(2026, 6, 16, 10, i, 0, 0, time.UTC).Format(time.RFC3339), "m")
		}
	}
	q := dayQuery(t, "2026-06-16", "UTC")
	got, err := d.activityReportCandidates(t.Context(), []string{"large", "small"}, q)
	require.NoError(t, err)
	assert.Len(t, got, 5)
	assert.Empty(t, d.activityMemo.entries)
	for _, id := range []string{"large", "small"} {
		_, err = d.WriteSessionIncremental(t.Context(), id, []Message{
			{SessionID: id, Ordinal: 1, Role: "assistant", Content: "x", Timestamp: "2026-06-16T10:04:00Z", Model: "m"},
		}, IncrementalSessionUpdate{ReplaceFromOrdinal: new(1)})
		require.NoError(t, err)
	}
	got, err = d.activityReportCandidates(t.Context(), []string{"large", "small"}, q)
	require.NoError(t, err)
	require.Len(t, got, 2)
	for _, c := range got {
		assert.Equal(t, 4*time.Minute, c.End.Sub(c.Start))
	}
	require.NoError(t, d.Reopen())
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	err = d.activityReportCandidateSource([]string{"small"}, q)(ctx, func(activity.IntervalCandidate) error { cancel(); return nil })
	assert.ErrorIs(t, err, context.Canceled)
	got, err = d.activityReportCandidates(t.Context(), []string{"small"}, q)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, 4*time.Minute, got[0].End.Sub(got[0].Start))
}

func activityMemoPairingReads(t *testing.T, d *DB) map[string]int {
	t.Helper()
	d.rawReader().SetMaxOpenConns(1)
	conn, err := d.getReader().Conn(t.Context())
	require.NoError(t, err)
	reads := make(map[string]int)
	require.NoError(t, conn.Raw(func(driverConn any) error {
		return driverConn.(*sqlite3.SQLiteConn).RegisterFunc("agentsview_timestamp_unix_micro", func(raw string) any {
			reads[raw]++
			return sqliteTimestampUnixMicro(raw)
		}, false)
	}))
	require.NoError(t, conn.Close())
	return reads
}

func TestActivityMemoProductionWrites(t *testing.T) {
	for _, action := range []string{"replace", "batch replace", "subagent link", "rename", "week"} {
		t.Run(action, func(t *testing.T) {
			d := testDB(t)
			insertSession(t, d, "s", "p")
			messages := []Message{
				{SessionID: "s", Ordinal: 0, Role: "assistant", Content: "x", Timestamp: "2026-06-16T10:00:00Z", HasToolUse: true, ToolCalls: []ToolCall{{SessionID: "s", ToolUseID: "call", ToolName: "Task", Category: "Task"}}},
				{SessionID: "s", Ordinal: 1, Role: "assistant", Content: "x", Timestamp: "2026-06-16T10:02:00Z"},
			}
			require.NoError(t, d.ReplaceSessionMessages(t.Context(), "s", messages))
			reads := activityMemoPairingReads(t, d)
			q := dayQuery(t, "2026-06-16", "UTC")
			_, err := d.activityReportCandidates(t.Context(), []string{"s"}, q)
			require.NoError(t, err)
			require.Positive(t, reads[messages[0].Timestamp])
			clear(reads)
			wantMinutes := 2.0
			switch action {
			case "replace", "batch replace":
				messages[1].Timestamp = "2026-06-16T10:04:00Z"
				if action == "replace" {
					require.NoError(t, d.ReplaceSessionMessages(t.Context(), "s", messages))
				} else {
					session, err := d.GetSession(t.Context(), "s")
					require.NoError(t, err)
					_, err = d.WriteSessionBatchAtomic(t.Context(), []SessionBatchWrite{{Session: *session, Messages: messages, ReplaceMessages: true}})
					require.NoError(t, err)
				}
				wantMinutes = 4
			case "subagent link":
				require.NoError(t, d.SetToolCallSubagentSession(t.Context(), "s", "call", "child"))
			case "rename":
				require.NoError(t, d.RenameSession(t.Context(), "s", Ptr("renamed")))
			case "week":
				week := q
				week.RangeStart = week.RangeStart.AddDate(0, 0, -6)
				_, err := d.activityReportCandidates(t.Context(), []string{"s"}, week)
				require.NoError(t, err)
				clear(reads)
			}
			got, err := d.activityReportCandidates(t.Context(), []string{"s"}, q)
			require.NoError(t, err)
			require.Len(t, got, 1)
			assert.Equal(t, wantMinutes, got[0].End.Sub(got[0].Start).Minutes())
			if action == "rename" || action == "week" {
				assert.Empty(t, reads)
			} else {
				assert.Positive(t, reads[messages[0].Timestamp])
			}
		})
	}
}

func TestActivityMemoReportRecomputesChangedSession(t *testing.T) {
	d := testDB(t)
	for i, id := range []string{"idle", "changed"} {
		insertSession(t, d, id, "p", func(s *Session) {
			s.StartedAt = Ptr("2026-06-16T10:00:00Z")
			s.EndedAt = Ptr("2026-06-16T11:00:00Z")
		})
		seedMessage(t, d, id, 0, "user", fmt.Sprintf("2026-06-16T10:%02d:00Z", i*10), "")
		seedMessage(t, d, id, 1, "assistant", fmt.Sprintf("2026-06-16T10:%02d:00Z", i*10+2), "m")
	}
	reads := activityMemoPairingReads(t, d)
	q := dayQuery(t, "2026-06-16", "UTC")
	f := AnalyticsFilter{Timezone: "UTC"}
	_, err := d.BuildActivityReportArtifacts(t.Context(), f, q, nil)
	require.NoError(t, err)
	require.Positive(t, reads["2026-06-16T10:00:00Z"])
	_, err = d.WriteSessionIncremental(t.Context(), "changed", []Message{
		{SessionID: "changed", Ordinal: 2, Role: "assistant", Content: "x", Timestamp: "2026-06-16T10:14:00Z", Model: "m"},
	}, IncrementalSessionUpdate{MsgCount: 3, UserMsgCount: 1, NextOrdinal: 3})
	require.NoError(t, err)
	clear(reads)
	got, err := d.BuildActivityReportArtifacts(t.Context(), f, q, nil)
	require.NoError(t, err)
	assert.Equal(t, 6.0, got.Report.Totals.ActiveMinutes)
	assert.Zero(t, reads["2026-06-16T10:00:00Z"])
	assert.Zero(t, reads["2026-06-16T10:02:00Z"])
	assert.Positive(t, reads["2026-06-16T10:10:00Z"])
	assert.Positive(t, reads["2026-06-16T10:14:00Z"])
	d.activityMemo.swap(func() {})
	want, err := d.BuildActivityReportArtifacts(t.Context(), f, q, nil)
	require.NoError(t, err)
	assert.Equal(t, want, got)
}

func TestActivityMemoReportFractionalEnd(t *testing.T) {
	d := testDB(t)
	insertSession(t, d, "s", "p", func(s *Session) {
		s.StartedAt = Ptr("2026-06-16T09:59:00Z")
		s.EndedAt = Ptr("2026-06-16T10:02:00Z")
	})
	for i, ts := range []string{"2026-06-16T09:59:00Z", "2026-06-16T10:00:00Z", "2026-06-16T10:02:00Z"} {
		seedMessage(t, d, "s", i, "assistant", ts, "m")
	}
	q, err := activity.ResolveQuery(activity.QueryInput{
		Preset: "custom", Timezone: "UTC", From: "2026-06-16T10:00:00.0000001Z", To: "2026-06-16T10:03:00Z",
	}, time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC))
	require.NoError(t, err)
	f := AnalyticsFilter{Timezone: "UTC"}
	_, err = d.BuildActivityReportArtifacts(t.Context(), f, q, nil)
	require.NoError(t, err)
	q.EffectiveEnd = q.RangeStart.Add(400 * time.Nanosecond)
	for _, warm := range []bool{true, false} {
		if !warm {
			d.activityMemo.swap(func() {})
		}
		got, err := d.BuildActivityReportArtifacts(t.Context(), f, q, nil)
		require.NoError(t, err)
		assert.Zero(t, got.Report.Totals.ActiveMinutes)
	}
}

func TestActivityMemoEmptyIDsWithCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	require.NoError(t, testDB(t).activityReportCandidateSource(nil, activity.Query{})(ctx, func(activity.IntervalCandidate) error { t.Fatal("unexpected candidate"); return nil }))
}
