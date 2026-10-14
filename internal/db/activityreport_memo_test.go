package db

import (
	"context"
	"fmt"
	"os"
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
			_, err := d.getWriter().ExecContext(t.Context(), `UPDATE sessions SET created_at = '2026-06-01T00:00:00Z' WHERE id = 's'`)
			require.NoError(t, err)
			seedMessage(t, d, "s", 0, "user", "2026-06-16T10:00:00Z", "")
			seedMessage(t, d, "s", 1, "assistant", "2026-06-16T10:02:00Z", "m")
			q := dayQuery(t, "2026-06-16", "UTC")
			before, err := d.activityReportCandidates(t.Context(), []string{"s"}, q)
			require.NoError(t, err)
			require.Len(t, before, 1)
			wantMinutes := 2.0
			wantCount := 1
			switch action {
			case "truncate":
				_, err := d.WriteSessionIncremental(t.Context(), "s", nil, IncrementalSessionUpdate{ReplaceFromOrdinal: new(1)})
				require.NoError(t, err)
				session, err := d.GetSession(t.Context(), "s")
				require.NoError(t, err)
				require.NotNil(t, session.TranscriptRevision)
				assert.Equal(t, "3", *session.TranscriptRevision)
				wantCount = 0
			case "recreate":
				_, err := d.DeleteParserExcludedSessions(t.Context(), []string{"s"})
				require.NoError(t, err)
				insertSession(t, d, "s", "p")
				seedMessage(t, d, "s", 0, "user", "2026-06-16T10:00:00Z", "")
				seedMessage(t, d, "s", 1, "assistant", "2026-06-16T10:04:00Z", "m")
				wantMinutes = 4
			case "reopen", "stale store":
				key := activityMemoKey{"s", q.RangeStart.Add(-time.Duration(q.GapCapSeconds) * time.Second).UnixMicro()}
				generation := d.activityMemo.generation
				entry := d.activityMemo.entries[key].Value.(activityMemoEntry)
				replacement := testDB(t)
				insertSession(t, replacement, "s", "p")
				seedMessage(t, replacement, "s", 0, "user", "2026-06-16T10:00:00Z", "")
				seedMessage(t, replacement, "s", 1, "assistant", "2026-06-16T10:04:00Z", "m")
				_, err := replacement.getWriter().ExecContext(t.Context(), `UPDATE sessions SET created_at = '2026-06-01T00:00:00Z' WHERE id = 's'`)
				require.NoError(t, err)
				var version string
				require.NoError(t, replacement.getReader().QueryRowContext(t.Context(), `SELECT transcript_revision || '|' || created_at FROM sessions WHERE id = 's'`).Scan(&version))
				require.Equal(t, entry.version, version)
				require.NoError(t, replacement.Close())
				require.NoError(t, d.CloseConnections(t.Context()))
				require.NoError(t, os.Rename(replacement.Path(), d.Path()))
				require.NoError(t, d.Reopen())
				if action == "stale store" {
					d.activityMemo.store(generation, entry)
				}
				wantMinutes = 4
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
	reads := activityMemoPairingReads(t, d)
	got, err := d.activityReportCandidates(t.Context(), []string{"s"}, q)
	require.NoError(t, err)
	require.Len(t, got, 1)
	clear(reads)
	q.EffectiveEnd = q.EffectiveEnd.Add(2 * time.Minute)
	got, err = d.activityReportCandidates(t.Context(), []string{"s"}, q)
	require.NoError(t, err)
	require.Len(t, got, 2)
	assert.Empty(t, reads)
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
	activityMemoMaxCandidates = 4
	t.Cleanup(func() { activityMemoMaxCandidates = old })
	d := testDB(t)
	for _, id := range []string{"large", "small"} {
		insertSession(t, d, id, "p")
		count := 2
		hour := 11
		if id == "large" {
			count = 5
			hour = 10
		}
		for i := range count {
			seedMessage(t, d, id, i, "assistant", time.Date(2026, 6, 16, hour, i, 0, 0, time.UTC).Format(time.RFC3339), "m")
		}
	}
	q := dayQuery(t, "2026-06-16", "UTC")
	got, err := d.activityReportCandidates(t.Context(), []string{"large", "small"}, q)
	require.NoError(t, err)
	assert.Len(t, got, 5)
	reads := activityMemoPairingReads(t, d)
	got, err = d.activityReportCandidates(t.Context(), []string{"large", "small"}, q)
	require.NoError(t, err)
	assert.Len(t, got, 5)
	assert.Positive(t, reads["2026-06-16T10:00:00Z"])
	assert.Positive(t, reads["2026-06-16T11:00:00Z"])
	for _, id := range []string{"large", "small"} {
		timestamp := "2026-06-16T11:04:00Z"
		if id == "large" {
			timestamp = "2026-06-16T10:04:00Z"
		}
		_, err = d.WriteSessionIncremental(t.Context(), id, []Message{
			{SessionID: id, Ordinal: 1, Role: "assistant", Content: "x", Timestamp: timestamp, Model: "m"},
		}, IncrementalSessionUpdate{ReplaceFromOrdinal: new(1)})
		require.NoError(t, err)
	}
	got, err = d.activityReportCandidates(t.Context(), []string{"large", "small"}, q)
	require.NoError(t, err)
	require.Len(t, got, 2)
	for _, c := range got {
		assert.Equal(t, 4*time.Minute, c.End.Sub(c.Start))
	}
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

func TestActivityMemoRefreshPreservesTies(t *testing.T) {
	for _, terminal := range []bool{false, true} {
		t.Run(fmt.Sprint(terminal), func(t *testing.T) {
			d := testDB(t)
			ids := []string{"b", "a"}
			if terminal {
				ids = ids[:1]
			}
			for _, id := range ids {
				insertSession(t, d, id, "p", func(s *Session) {
					s.StartedAt = Ptr("2026-06-16T10:00:00Z")
					s.EndedAt = Ptr("2026-06-16T10:02:00Z")
				})
				seedMessage(t, d, id, 0, "user", "2026-06-16T10:00:00.0000009Z", "")
				seedMessage(t, d, id, 1, "assistant", "2026-06-16T10:01:00Z", "A")
				seedMessage(t, d, id, 2, "user", "2026-06-16T10:00:00.0000001Z", "")
				seedMessage(t, d, id, 3, "assistant", "2026-06-16T10:02:00Z", "B")
				if terminal {
					timingInsertToolResultEvent(t, d, id, 0, 0, "call", "completed", "2026-06-16T10:00:00.0000005Z", 0)
					timingInsertToolResultEvent(t, d, id, 0, 0, "call", "completed", "2026-06-16T10:00:00.0000002Z", 1)
				}
			}
			q := dayQuery(t, "2026-06-16", "UTC")
			cold, err := d.activityReportCandidates(t.Context(), ids, q)
			require.NoError(t, err)
			for _, cached := range []int{2, 1} {
				if cached == 1 {
					d.activityMemo.swap(func() {})
					_, err = d.activityReportCandidates(t.Context(), ids[:1], q)
					require.NoError(t, err)
				}
				warm, err := d.activityReportCandidates(t.Context(), ids, q)
				require.NoError(t, err)
				assert.Equal(t, cold, warm)
			}
			d.activityMemo.swap(func() {})
			coldReport, err := d.GetActivityReport(t.Context(), AnalyticsFilter{Timezone: "UTC"}, q)
			require.NoError(t, err)
			warmReport, err := d.GetActivityReport(t.Context(), AnalyticsFilter{Timezone: "UTC"}, q)
			require.NoError(t, err)
			assert.Equal(t, coldReport, warmReport)
			if !terminal {
				minutes := make(map[string]float64)
				for _, row := range warmReport.ByModel {
					minutes[row.Key] = row.AgentMinutes
				}
				assert.InDelta(t, 2.0, minutes["A"], 1e-6)
				assert.InDelta(t, 2.0, minutes["B"], 1e-6)
			}
		})
	}
}

func TestActivityMemoLRUAndBudgets(t *testing.T) {
	oldCandidates, oldEntries := activityMemoMaxCandidates, activityMemoMaxEntries
	activityMemoMaxCandidates, activityMemoMaxEntries = 3, 5
	t.Cleanup(func() { activityMemoMaxCandidates, activityMemoMaxEntries = oldCandidates, oldEntries })
	for _, order := range [][]string{{"c", "b"}, {"b", "c"}} {
		t.Run(fmt.Sprint(order), func(t *testing.T) {
			d := testDB(t)
			q := dayQuery(t, "2026-06-16", "UTC")
			for i, id := range []string{"a", "b", "c", "empty-1", "empty-2", "empty-3", "empty-4"} {
				insertSession(t, d, id, "p")
				if len(id) == 1 {
					seedMessage(t, d, id, 0, "user", fmt.Sprintf("2026-06-16T10:%02d:00Z", i*10), "")
					seedMessage(t, d, id, 1, "assistant", fmt.Sprintf("2026-06-16T10:%02d:00Z", i*10+1), "m")
				}
			}
			reads := activityMemoPairingReads(t, d)
			read := func(ids ...string) {
				t.Helper()
				_, err := d.activityReportCandidates(t.Context(), ids, q)
				require.NoError(t, err)
			}
			read("a", "b")
			clear(reads)
			read("a")
			assert.Empty(t, reads)
			seedMessage(t, d, "c", 2, "assistant", "2026-06-16T10:22:00Z", "m")
			read("c")
			assert.Equal(t, 3, d.activityMemo.rows)
			_, err := d.WriteSessionIncremental(t.Context(), "c", nil, IncrementalSessionUpdate{ReplaceFromOrdinal: new(2)})
			require.NoError(t, err)
			clear(reads)
			read(order...)
			assert.Positive(t, reads["2026-06-16T10:10:00Z"])
			assert.Positive(t, reads["2026-06-16T10:20:00Z"])
			clear(reads)
			read("a", "b", "c")
			assert.Empty(t, reads)
			assert.Equal(t, 3, d.activityMemo.rows)
			read("empty-1", "empty-2")
			assert.Len(t, d.activityMemo.entries, 5)
			assert.Equal(t, 3, d.activityMemo.rows)
			read("empty-3", "empty-4")
			assert.Len(t, d.activityMemo.entries, 5)
			assert.Equal(t, 1, d.activityMemo.rows)
			d.activityMemo.swap(func() {})
			d.rawReader().SetMaxOpenConns(1)
			conn, err := d.getReader().Conn(t.Context())
			require.NoError(t, err)
			require.NoError(t, conn.Raw(func(raw any) error {
				raw.(*sqlite3.SQLiteConn).RegisterAuthorizer(func(op int, table, column, _ string) int {
					if op == sqlite3.SQLITE_READ && table == "sessions" && column == "transcript_revision" {
						return sqlite3.SQLITE_DENY
					}
					return sqlite3.SQLITE_OK
				})
				return nil
			}))
			require.NoError(t, conn.Close())
			got, err := d.activityReportCandidates(t.Context(), []string{"a", "b", "empty-1", "empty-2", "empty-3", "empty-4"}, q)
			require.NoError(t, err)
			assert.Len(t, got, 2)
			assert.Empty(t, d.activityMemo.entries)
		})
	}
}
