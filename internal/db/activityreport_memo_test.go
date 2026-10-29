package db

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/activity"
)

func TestActivityMemoReuseAndReset(t *testing.T) {
	for _, action := range []string{"recreate", "stale store", "truncate"} {
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
				original, err := d.GetSession(t.Context(), "s")
				require.NoError(t, err)
				require.NotNil(t, original.TranscriptRevision)
				_, err = d.WriteSessionIncremental(t.Context(), "s", nil, IncrementalSessionUpdate{ReplaceFromOrdinal: new(1)})
				require.NoError(t, err)
				session, err := d.GetSession(t.Context(), "s")
				require.NoError(t, err)
				require.NotNil(t, session.TranscriptRevision)
				assert.NotEqual(t, *original.TranscriptRevision, *session.TranscriptRevision)
				wantCount = 0
			case "recreate":
				_, err := d.DeleteParserExcludedSessions(t.Context(), []string{"s"})
				require.NoError(t, err)
				insertSession(t, d, "s", "p")
				seedMessage(t, d, "s", 0, "user", "2026-06-16T10:00:00Z", "")
				seedMessage(t, d, "s", 1, "assistant", "2026-06-16T10:04:00Z", "m")
				wantMinutes = 4
			case "stale store":
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
				d.activityMemo.store(generation, entry)
				wantMinutes = 4
			}
			got, err := d.activityReportCandidates(t.Context(), []string{"s"}, q)
			require.NoError(t, err)
			require.Len(t, got, wantCount)
			if wantCount > 0 {
				assert.InEpsilon(t, wantMinutes, got[0].End.Sub(got[0].Start).Minutes(), 1e-9)
			}
		})
	}
}

func TestActivityMemoWindow(t *testing.T) {
	t.Run("extend", func(t *testing.T) {
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
	})
	t.Run("fractional end", func(t *testing.T) {
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
		_, err = d.activityReportCandidates(t.Context(), []string{"s"}, q)
		require.NoError(t, err)
		q.EffectiveEnd = q.RangeStart.Add(400 * time.Nanosecond)
		warm, err := d.activityReportCandidates(t.Context(), []string{"s"}, q)
		require.NoError(t, err)
		d.activityMemo.swap(func() {})
		cold, err := d.activityReportCandidates(t.Context(), []string{"s"}, q)
		require.NoError(t, err)
		require.Len(t, cold, 1)
		assert.Equal(t, 0, cold[0].StartOrdinal)
		assert.Equal(t, cold, warm)
	})
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
}

func activityMemoPairingReads(tb testing.TB, d *DB) map[string]int {
	tb.Helper()
	d.rawReader().SetMaxOpenConns(1)
	conn, err := d.getReader().Conn(tb.Context())
	require.NoError(tb, err)
	defer conn.Close()
	reads := make(map[string]int)
	require.NoError(tb, conn.Raw(func(driverConn any) error {
		return driverConn.(interface{ RegisterFunc(string, any, bool) error }).RegisterFunc("agentsview_timestamp_unix_micro", func(raw string) any {
			reads[raw]++
			return sqliteTimestampUnixMicro(raw)
		}, false)
	}))
	require.NoError(tb, conn.Close())
	return reads
}

func TestActivityMemoProductionWrites(t *testing.T) {
	d := testDB(t)
	insertSession(t, d, "s", "p")
	seedMessage(t, d, "s", 0, "assistant", "2026-06-16T10:00:00Z", "m")
	seedMessage(t, d, "s", 1, "assistant", "2026-06-16T10:02:00Z", "m")
	reads := activityMemoPairingReads(t, d)
	q := dayQuery(t, "2026-06-16", "UTC")
	_, err := d.activityReportCandidates(t.Context(), []string{"s"}, q)
	require.NoError(t, err)
	require.Positive(t, reads["2026-06-16T10:00:00Z"])
	clear(reads)
	require.NoError(t, d.RenameSession(t.Context(), "s", Ptr("renamed")))
	got, err := d.activityReportCandidates(t.Context(), []string{"s"}, q)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.InEpsilon(t, 2.0, got[0].End.Sub(got[0].Start).Minutes(), 1e-9)
	assert.Empty(t, reads)
}

func TestActivityMemoReportReuse(t *testing.T) {
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
	cold, err := d.BuildActivityReportArtifacts(t.Context(), f, q, nil)
	require.NoError(t, err)
	assert.InEpsilon(t, 4.0, cold.Report.Totals.AgentMinutes, 1e-9)
	assert.InEpsilon(t, 4.0, cold.Report.Totals.ActiveMinutes, 1e-9)
	require.Positive(t, reads["2026-06-16T10:00:00Z"])
	require.Positive(t, reads["2026-06-16T10:10:00Z"])
	clear(reads)
	warm, err := d.BuildActivityReportArtifacts(t.Context(), f, q, nil)
	require.NoError(t, err)
	assert.InEpsilon(t, 4.0, warm.Report.Totals.AgentMinutes, 1e-9)
	assert.InEpsilon(t, 4.0, warm.Report.Totals.ActiveMinutes, 1e-9)
	assert.Empty(t, reads)
	_, err = d.WriteSessionIncremental(t.Context(), "changed", []Message{{SessionID: "changed", Ordinal: 2, Role: "assistant", Content: "x", Timestamp: "2026-06-16T10:15:00Z", Model: "m"}}, IncrementalSessionUpdate{MsgCount: 3, UserMsgCount: 1, NextOrdinal: 3})
	require.NoError(t, err)
	clear(reads)
	updated, err := d.BuildActivityReportArtifacts(t.Context(), f, q, nil)
	require.NoError(t, err)
	assert.InEpsilon(t, 7.0, updated.Report.Totals.AgentMinutes, 1e-9)
	assert.InEpsilon(t, 7.0, updated.Report.Totals.ActiveMinutes, 1e-9)
	assert.Positive(t, reads["2026-06-16T10:10:00Z"])
	assert.Positive(t, reads["2026-06-16T10:12:00Z"])
	assert.Zero(t, reads["2026-06-16T10:00:00Z"])
	assert.Zero(t, reads["2026-06-16T10:02:00Z"])
}

func TestActivityMemoReportAboveEntryCap(t *testing.T) {
	d := testDB(t)
	start := time.Date(2026, 6, 16, 10, 0, 0, 0, time.UTC)
	for i := range 4_097 {
		id := fmt.Sprintf("s-%04d", i)
		at := start.Add(time.Duration(i) * time.Second)
		insertSession(t, d, id, "p", func(s *Session) {
			s.StartedAt = Ptr(at.Format(time.RFC3339))
			s.EndedAt = Ptr(at.Add(time.Minute).Format(time.RFC3339))
		})
		seedMessage(t, d, id, 0, "user", at.Format(time.RFC3339), "")
		seedMessage(t, d, id, 1, "assistant", at.Add(time.Minute).Format(time.RFC3339), "m")
	}
	reads := activityMemoPairingReads(t, d)
	q := dayQuery(t, "2026-06-16", "UTC")
	f := AnalyticsFilter{Timezone: "UTC"}
	cold, err := d.BuildActivityReportArtifacts(t.Context(), f, q, nil)
	require.NoError(t, err)
	assert.InEpsilon(t, 4_097.0, cold.Report.Totals.AgentMinutes, 1e-9)
	require.Len(t, d.activityMemo.entries, 4_096)
	clear(reads)
	warm, err := d.BuildActivityReportArtifacts(t.Context(), f, q, nil)
	require.NoError(t, err)
	assert.InEpsilon(t, 4_097.0, warm.Report.Totals.AgentMinutes, 1e-9)
	assert.Equal(t, cold.Report.Totals, warm.Report.Totals)
	assert.Len(t, d.activityMemo.entries, 4_096)
	// Only the unretained session's two timestamps reach message pairing.
	assert.Len(t, reads, 2)
}

func TestActivityMemoEmptyIDsWithCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	require.NoError(t, testDB(t).activityReportCandidateSource(nil, activity.Query{})(ctx, func(activity.IntervalCandidate) error { require.FailNow(t, "unexpected candidate"); return nil }))
}

func TestActivityMemoRefreshPreservesTies(t *testing.T) {
	for _, terminal := range []bool{false, true} {
		t.Run(strconv.FormatBool(terminal), func(t *testing.T) {
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
			cachedCounts := []int{2, 1}
			if terminal {
				cachedCounts = cachedCounts[:1]
			}
			for _, cached := range cachedCounts {
				if cached == 1 {
					d.activityMemo.swap(func() {})
					_, err = d.activityReportCandidates(t.Context(), ids[:1], q)
					require.NoError(t, err)
				}
				warm, err := d.activityReportCandidates(t.Context(), ids, q)
				require.NoError(t, err)
				assert.Equal(t, cold, warm)
			}
		})
	}
}

func TestActivityMemoPartialInvalidationPreservesSubmicrosecondReport(t *testing.T) {
	d := testDB(t)
	for _, id := range []string{"a", "b"} {
		insertSession(t, d, id, "p", func(s *Session) {
			s.StartedAt = Ptr("2026-06-16T10:00:00Z")
			s.EndedAt = Ptr("2026-06-16T10:03:00Z")
		})
		seedMessage(t, d, id, 0, "user", "2026-06-16T10:00:00.0000005Z", "")
		seedMessage(t, d, id, 1, "assistant", "2026-06-16T10:01:00Z", "A")
		seedMessage(t, d, id, 2, "user", "2026-06-16T10:00:00.0000001Z", "")
		seedMessage(t, d, id, 3, "assistant", "2026-06-16T10:02:00Z", "B")
		terminalTimestamp := "2026-06-16T10:00:00.0000009Z"
		if id == "b" {
			terminalTimestamp = "2026-06-16T10:00:00.0000000Z"
		}
		timingInsertToolResultEvent(t, d, id, 2, 0, "call", "completed", terminalTimestamp, 0)
	}
	q := dayQuery(t, "2026-06-16", "UTC")
	f := AnalyticsFilter{Timezone: "UTC"}
	_, err := d.GetActivityReport(t.Context(), f, q)
	require.NoError(t, err)
	seedMessage(t, d, "b", 4, "assistant", "2026-06-16T10:03:00Z", "C")
	partial, err := d.GetActivityReport(t.Context(), f, q)
	require.NoError(t, err)
	d.activityMemo.swap(func() {})
	cold, err := d.GetActivityReport(t.Context(), f, q)
	require.NoError(t, err)
	require.NotEmpty(t, cold.ByModel)
	assert.Equal(t, cold, partial)
}

func TestActivityMemoLRUAndBudgets(t *testing.T) {
	oldCandidates, oldEntries := activityMemoMaxCandidates, activityMemoMaxEntries
	activityMemoMaxCandidates, activityMemoMaxEntries = 3, 5
	t.Cleanup(func() { activityMemoMaxCandidates, activityMemoMaxEntries = oldCandidates, oldEntries })
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
	clear(reads)
	read("c", "b")
	assert.Positive(t, reads["2026-06-16T10:10:00Z"])
	assert.Zero(t, reads["2026-06-16T10:20:00Z"])
	clear(reads)
	read("b", "c")
	assert.Empty(t, reads)
	assert.Equal(t, 3, d.activityMemo.rows)
	read("empty-1", "empty-2", "empty-3")
	assert.Len(t, d.activityMemo.entries, 5)
	assert.Equal(t, 3, d.activityMemo.rows)
	read("empty-4")
	assert.Len(t, d.activityMemo.entries, 5)
	assert.Equal(t, 2, d.activityMemo.rows)
}
