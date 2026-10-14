package db

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/activity"
)

func TestActivityMemoReuseAndReset(t *testing.T) {
	for _, action := range []string{"rename", "week", "recreate", "reopen", "compact", "stale store", "truncate"} {
		t.Run(action, func(t *testing.T) {
			d := testDB(t)
			insertSession(t, d, "s", "p")
			seedMessage(t, d, "s", 0, "user", "2026-06-16T10:00:00Z", "")
			seedMessage(t, d, "s", 1, "assistant", "2026-06-16T10:02:00Z", "m")
			q := dayQuery(t, "2026-06-16", "UTC")
			before, err := d.activityReportCandidates(t.Context(), []string{"s"}, q)
			require.NoError(t, err)
			require.Len(t, before, 1)
			key := activityMemoKey{"s", q.RangeStart.Add(-time.Duration(q.GapCapSeconds) * time.Second).UnixMicro()}
			generation := d.activityMemo.generation
			entry := d.activityMemo.entries[key].Value.(activityMemoEntry)
			_, err = d.getWriter().ExecContext(t.Context(), `UPDATE messages SET timestamp = '2026-06-16T10:04:00Z' WHERE session_id = 's' AND ordinal = 1`)
			require.NoError(t, err)
			wantMinutes := 2.0
			wantCount := 1
			switch action {
			case "truncate":
				_, err := d.WriteSessionIncremental(t.Context(), "s", nil, IncrementalSessionUpdate{ReplaceFromOrdinal: new(1)})
				require.NoError(t, err)
				wantCount = 0
			case "rename":
				require.NoError(t, d.RenameSession(t.Context(), "s", Ptr("renamed")))
			case "week":
				week := q
				week.RangeStart = week.RangeStart.AddDate(0, 0, -6)
				got, err := d.activityReportCandidates(t.Context(), []string{"s"}, week)
				require.NoError(t, err)
				require.Len(t, got, 1)
				assert.Equal(t, 4*time.Minute, got[0].End.Sub(got[0].Start))
			case "recreate":
				_, err := d.getWriter().ExecContext(t.Context(), `DELETE FROM sessions WHERE id = 's'`)
				require.NoError(t, err)
				insertSession(t, d, "s", "p")
				seedMessage(t, d, "s", 0, "user", "2026-06-16T10:00:00Z", "")
				seedMessage(t, d, "s", 1, "assistant", "2026-06-16T10:04:00Z", "m")
				_, err = d.getWriter().ExecContext(t.Context(), `UPDATE sessions SET transcript_revision = ?, created_at = '2026-06-17T00:00:00Z' WHERE id = 's'`, "2")
				require.NoError(t, err)
				wantMinutes = 4
			case "reopen", "stale store":
				require.NoError(t, d.Reopen())
				if action == "stale store" {
					d.activityMemo.store(generation, entry)
				}
				wantMinutes = 4
			case "compact":
				_, err := d.Compact(t.Context(), CompactOptions{StagingDir: t.TempDir()})
				require.NoError(t, err)
				wantMinutes = 4
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
	seedMessage(t, d, "s", 5, "assistant", "2026-06-16T10:02:30Z", "m")
	seedMessage(t, d, "s", 6, "assistant", "2026-06-16T10:03:00Z", "m")
	got, err = d.activityReportCandidates(t.Context(), []string{"s"}, q)
	require.NoError(t, err)
	require.Len(t, got, 3)
	assert.Equal(t, 5, got[2].StartOrdinal)
	var reference []activity.IntervalCandidate
	require.NoError(t, d.activityReportCandidateSQL([]string{"s"}, q)(t.Context(), func(c activity.IntervalCandidate) error { reference = append(reference, c); return nil }))
	assert.Equal(t, reference, got)
}

func TestActivityMemoOverflowIsolation(t *testing.T) {
	old := activityMemoMaxCandidates
	activityMemoMaxCandidates = 5
	t.Cleanup(func() { activityMemoMaxCandidates = old })
	d := testDB(t)
	for _, id := range []string{"large", "small"} {
		insertSession(t, d, id, "p")
		count := 2
		if id == "large" {
			count = 8
		}
		for i := range count {
			seedMessage(t, d, id, i, "assistant", time.Date(2026, 6, 16, 10, i, 0, 0, time.UTC).Format(time.RFC3339), "m")
		}
	}
	q := dayQuery(t, "2026-06-16", "UTC")
	got, err := d.activityReportCandidates(t.Context(), []string{"large", "small"}, q)
	require.NoError(t, err)
	assert.Len(t, got, 8)
	_, err = d.getWriter().ExecContext(t.Context(), `UPDATE messages SET timestamp = '2026-06-16T10:04:00Z' WHERE ordinal = 1`)
	require.NoError(t, err)
	got, err = d.activityReportCandidates(t.Context(), []string{"large", "small"}, q)
	require.NoError(t, err)
	require.Len(t, got, 8)
	for _, c := range got {
		if c.StartOrdinal == 0 && c.SessionID == "small" {
			assert.Equal(t, time.Minute, c.End.Sub(c.Start))
		}
		if c.StartOrdinal == 0 && c.SessionID == "large" {
			assert.Equal(t, 4*time.Minute, c.End.Sub(c.Start))
		}
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
