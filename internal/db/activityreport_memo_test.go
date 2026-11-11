package db

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"os"
	"slices"
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
			before := activityRuns(t, d, q, "s")
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
			got := activityRuns(t, d, q, "s")
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
		got := activityRuns(t, d, q, "s")
		require.Len(t, got, 1)
		rewriteActivityModels(t, d, "reread", "s")
		q.EffectiveEnd = q.EffectiveEnd.Add(2 * time.Minute)
		// The kept window reaches past the first effective end, so the
		// second pair comes from the memo with its first model.
		assert.Equal(t, []activity.Run{{
			SessionID: "s", Start: time.Date(2026, 6, 16, 10, 0, 0, 0, time.UTC), End: q.EffectiveEnd,
			Tail: time.Date(2026, 6, 16, 10, 2, 0, 0, time.UTC), Model: "m",
		}}, activityRuns(t, d, q, "s"))
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
		activityRuns(t, d, q, "s")
		q.EffectiveEnd = q.RangeStart.Add(400 * time.Nanosecond)
		warm := activityRuns(t, d, q, "s")
		d.activityMemo.swap(func() {})
		cold := activityRuns(t, d, q, "s")
		// The pair from 10:00 starts in the effective end's microsecond, so
		// the candidate stream leaves it out; the pair before it ends at 10:00.
		assert.Empty(t, cold)
		assert.Equal(t, cold, warm)
	})
}

func TestActivityMemoOverflowSkipsReport(t *testing.T) {
	old := activityMemoMaxBytes
	activityMemoMaxBytes = 1
	t.Cleanup(func() { activityMemoMaxBytes = old })
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
	assert.Len(t, activityRuns(t, d, q, "large", "small"), 2)
	assert.Empty(t, d.activityMemo.entries)
	rewriteActivityModels(t, d, "reread", "large", "small")
	assert.Equal(t, []string{"large", "small"}, rereadRuns(activityRuns(t, d, q, "large", "small"), "reread"))
}

// activityRuns returns the report runs of the sessions for q.
func activityRuns(tb testing.TB, d *DB, q activity.Query, ids ...string) []activity.Run {
	tb.Helper()
	runs, _, err := d.activityReportRuns(tb.Context(), ids, q)
	require.NoError(tb, err)
	return runs
}

// rereadRuns returns the sorted sessions with a run attributed to model.
func rereadRuns(runs []activity.Run, model string) []string {
	var out []string
	for _, run := range runs {
		if run.Model == model && !slices.Contains(out, run.SessionID) {
			out = append(out, run.SessionID)
		}
	}
	slices.Sort(out)
	return out
}

// rewriteActivityModels stores model on every message of the sessions and
// leaves their transcript revision as it was, so the memo still treats them
// as unchanged. Pairs read from storage afterwards close on model; pairs the
// memo serves keep the model they were first read with.
func rewriteActivityModels(tb testing.TB, d *DB, model string, ids ...string) {
	tb.Helper()
	encoded, err := json.Marshal(ids)
	require.NoError(tb, err)
	_, err = d.getWriter().ExecContext(tb.Context(),
		`UPDATE messages SET model = ? WHERE session_id IN (SELECT value FROM json_each(?))`,
		model, string(encoded))
	require.NoError(tb, err)
}

// reportModels returns the sorted model keys of a report's model minutes.
func reportModels(artifacts activity.CandidateArtifacts) []string {
	out := make([]string, 0, len(artifacts.Report.ByModel))
	for _, row := range artifacts.Report.ByModel {
		out = append(out, row.Key)
	}
	slices.Sort(out)
	return out
}

func TestActivityMemoProductionWrites(t *testing.T) {
	d := testDB(t)
	insertSession(t, d, "s", "p")
	seedMessage(t, d, "s", 0, "assistant", "2026-06-16T10:00:00Z", "m")
	seedMessage(t, d, "s", 1, "assistant", "2026-06-16T10:02:00Z", "m")
	q := dayQuery(t, "2026-06-16", "UTC")
	activityRuns(t, d, q, "s")
	rewriteActivityModels(t, d, "reread", "s")
	require.NoError(t, d.RenameSession(t.Context(), "s", Ptr("renamed")))
	got := activityRuns(t, d, q, "s")
	require.Len(t, got, 1)
	assert.InEpsilon(t, 2.0, got[0].End.Sub(got[0].Start).Minutes(), 1e-9)
	assert.Empty(t, rereadRuns(got, "reread"))
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
	q := dayQuery(t, "2026-06-16", "UTC")
	f := AnalyticsFilter{Timezone: "UTC"}
	cold, err := d.BuildActivityReportArtifacts(t.Context(), f, q, nil)
	require.NoError(t, err)
	assert.InEpsilon(t, 4.0, cold.Report.Totals.AgentMinutes, 1e-9)
	assert.InEpsilon(t, 4.0, cold.Report.Totals.ActiveMinutes, 1e-9)
	rewriteActivityModels(t, d, "idle-reread", "idle")
	rewriteActivityModels(t, d, "changed-reread", "changed")
	clearKeptActivity(d)
	warm, err := d.BuildActivityReportArtifacts(t.Context(), f, q, nil)
	require.NoError(t, err)
	assert.InEpsilon(t, 4.0, warm.Report.Totals.AgentMinutes, 1e-9)
	assert.InEpsilon(t, 4.0, warm.Report.Totals.ActiveMinutes, 1e-9)
	assert.Equal(t, []string{"m"}, reportModels(warm))
	_, err = d.WriteSessionIncremental(t.Context(), "changed", []Message{{SessionID: "changed", Ordinal: 2, Role: "assistant", Content: "x", Timestamp: "2026-06-16T10:15:00Z", Model: "m"}}, IncrementalSessionUpdate{MsgCount: 3, UserMsgCount: 1, NextOrdinal: 3})
	require.NoError(t, err)
	updated, err := d.BuildActivityReportArtifacts(t.Context(), f, q, nil)
	require.NoError(t, err)
	assert.InEpsilon(t, 7.0, updated.Report.Totals.AgentMinutes, 1e-9)
	assert.InEpsilon(t, 7.0, updated.Report.Totals.ActiveMinutes, 1e-9)
	// Only the changed session is read again; the idle one comes from the memo.
	assert.Equal(t, []string{"changed-reread", "m"}, reportModels(updated))
}

// A report over thousands of sessions finds every one kept.
func TestActivityMemoReportKeepsManySessions(t *testing.T) {
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
	q := dayQuery(t, "2026-06-16", "UTC")
	f := AnalyticsFilter{Timezone: "UTC"}
	cold, err := d.BuildActivityReportArtifacts(t.Context(), f, q, nil)
	require.NoError(t, err)
	assert.InEpsilon(t, 4_097.0, cold.Report.Totals.AgentMinutes, 1e-9)
	require.Len(t, d.activityMemo.entries, 4_097)
	ids := make([]string, 4_097)
	for i := range ids {
		ids[i] = fmt.Sprintf("s-%04d", i)
	}
	rewriteActivityModels(t, d, "reread", ids...)
	clearKeptActivity(d)
	warm, err := d.BuildActivityReportArtifacts(t.Context(), f, q, nil)
	require.NoError(t, err)
	assert.Equal(t, cold.Report.Totals, warm.Report.Totals)
	assert.Len(t, d.activityMemo.entries, 4_097)
	assert.Equal(t, []string{"m"}, reportModels(warm))
}

func TestActivityMemoEmptyIDsWithCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	d := testDB(t)
	require.NoError(t, d.activityReportCandidateSource(nil, activity.Query{})(ctx, func(activity.IntervalCandidate) error { require.FailNow(t, "unexpected candidate"); return nil }))
	runs, _, err := d.activityReportRuns(ctx, nil, activity.Query{})
	require.NoError(t, err)
	assert.Empty(t, runs)
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
			cold := activityRuns(t, d, q, ids...)
			cachedCounts := []int{2, 1}
			if terminal {
				cachedCounts = cachedCounts[:1]
			}
			for _, cached := range cachedCounts {
				if cached == 1 {
					d.activityMemo.swap(func() {})
					activityRuns(t, d, q, ids[:1]...)
				}
				assert.Equal(t, cold, activityRuns(t, d, q, ids...))
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
	clearKeptActivity(d)
	cold, err := d.GetActivityReport(t.Context(), f, q)
	require.NoError(t, err)
	require.NotEmpty(t, cold.ByModel)
	assert.Equal(t, cold, partial)
}

// The memo evicts the least recently used sessions to stay within its byte
// budget, counting sessions without activity too.
func TestActivityMemoLRUAndBudget(t *testing.T) {
	old := activityMemoMaxBytes
	t.Cleanup(func() { activityMemoMaxBytes = old })
	d := testDB(t)
	q := dayQuery(t, "2026-06-16", "UTC")
	for i, id := range []string{"a", "b", "c", "d", "e", "f"} {
		insertSession(t, d, id, "p")
		if i < 3 {
			seedMessage(t, d, id, 0, "user", fmt.Sprintf("2026-06-16T10:%02d:00Z", i*10), "")
			seedMessage(t, d, id, 1, "assistant", fmt.Sprintf("2026-06-16T10:%02d:00Z", i*10+1), "m")
		}
	}
	activityRuns(t, d, q, "a")
	size := d.activityMemo.bytes
	require.Positive(t, size)
	// Room for two of the three sessions with activity.
	activityMemoMaxBytes = 2*size + size/2
	activityRuns(t, d, q, "b")
	rewriteActivityModels(t, d, "reread-1", "a", "b", "c")
	assert.Empty(t, rereadRuns(activityRuns(t, d, q, "a"), "reread-1"), "a is kept and now most recent")
	activityRuns(t, d, q, "c")
	rewriteActivityModels(t, d, "reread-2", "a", "b", "c")
	assert.Equal(t, []string{"b"}, rereadRuns(activityRuns(t, d, q, "a", "b", "c"), "reread-2"),
		"keeping c evicted b; keeping b again evicted a")
	assert.Equal(t, []string{"a"}, rereadRuns(activityRuns(t, d, q, "a"), "reread-2"))

	// Sessions without activity count too: keeping three of them after a
	// leaves room for only two, so a is evicted and read again.
	activityMemoMaxBytes = old
	d.activityMemo.swap(func() {})
	activityRuns(t, d, q, "d")
	empty := d.activityMemo.bytes
	require.Positive(t, empty)
	d.activityMemo.swap(func() {})
	activityRuns(t, d, q, "a")
	activityMemoMaxBytes = d.activityMemo.bytes + 2*empty
	activityRuns(t, d, q, "d", "e", "f")
	rewriteActivityModels(t, d, "reread-3", "a")
	assert.Equal(t, []string{"a"}, rereadRuns(activityRuns(t, d, q, "a"), "reread-3"))
}
