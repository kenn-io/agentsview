//go:build chtest

package clickhouse

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/activity"
	"go.kenn.io/agentsview/internal/db"
)

// A range still in progress keeps every session's usage rows. After a push
// its report re-reads only the pushed session, whether the pushed rows are
// still in the read-time delta or already refreshed, and still matches the
// local archive, including the Claude peers of a filtered report.
func TestPartialActivityUsageRereadsOnlyPushedSessions(t *testing.T) {
	store, syncer, local := newPushedStore(t)
	ctx := t.Context()
	const peerID = "range-peer"
	started := time.Date(2026, 1, 10, 1, 0, 0, 0, time.UTC)
	messageID := func(session, ordinal int) string { return fmt.Sprintf("msg-%d-%d", session, ordinal) }
	requestID := func(session, ordinal int) string { return fmt.Sprintf("req-%d-%d", session, ordinal) }
	var writes []db.SessionBatchWrite
	for i := range 3 {
		id := fmt.Sprintf("range-candidate-%d", i)
		msgs := []db.Message{fixtureMessage(id, 0, "user", "candidate", started.Format(time.RFC3339Nano))}
		for j := range 4 {
			m := fixtureMessage(id, j+1, "assistant", "reply", started.Add(time.Duration(i*10+j+1)*time.Second).Format(time.RFC3339Nano))
			m.ClaudeMessageID, m.ClaudeRequestID = messageID(i, j), requestID(i, j)
			msgs = append(msgs, m)
		}
		writes = append(writes, db.SessionBatchWrite{
			Session:  fixtureSession(id, "range", "candidate", started.Format(time.RFC3339Nano), len(msgs)),
			Messages: msgs, DataVersion: 1, ReplaceMessages: true,
		})
	}
	// The peer is outside the project filter. Its copies of the first
	// candidate's replies carry more output tokens, so survivor selection
	// must pick them.
	peerMessages := func(copies int) []db.Message {
		msgs := []db.Message{fixtureMessage(peerID, 0, "user", "peer", started.Format(time.RFC3339Nano))}
		for j := range copies {
			m := fixtureMessage(peerID, j+1, "assistant", "reply", started.Add(time.Duration(j+2)*time.Second).Format(time.RFC3339Nano))
			m.ClaudeMessageID, m.ClaudeRequestID = messageID(0, j), requestID(0, j)
			m.TokenUsage = []byte(`{"input_tokens":1,"output_tokens":5}`)
			msgs = append(msgs, m)
		}
		return msgs
	}
	writePeer := func(copies int) {
		msgs := peerMessages(copies)
		ended := started.Add(time.Duration(copies+2) * time.Second).Format(time.RFC3339Nano)
		peer := fixtureSession(peerID, "elsewhere", "peer", started.Format(time.RFC3339Nano), len(msgs))
		peer.EndedAt, peer.LocalModifiedAt = &ended, &ended
		_, err := local.WriteSessionBatchAtomic(ctx, []db.SessionBatchWrite{{
			Session: peer, Messages: msgs, DataVersion: 1, ReplaceMessages: true,
		}})
		require.NoError(t, err)
	}
	_, err := local.WriteSessionBatchAtomic(ctx, writes)
	require.NoError(t, err)
	writePeer(2)
	_, err = syncer.Push(ctx, false, nil)
	require.NoError(t, err)
	_, err = store.DB().ExecContext(ctx, "SYSTEM WAIT VIEW prepare_usage")
	require.NoError(t, err)

	now := time.Now()
	q, err := activity.ResolveQuery(activity.QueryInput{
		Preset: "custom", From: "2026-01-01T00:00:00Z", To: now.AddDate(0, 0, 2).UTC().Format(time.RFC3339), Timezone: "UTC",
	}, now)
	require.NoError(t, err)
	require.True(t, q.Partial)
	filtered := db.AnalyticsFilter{Timezone: "UTC", Project: "range"}
	everything := db.AnalyticsFilter{Timezone: "UTC"}
	requireParity := func(label string) {
		t.Helper()
		for _, f := range []db.AnalyticsFilter{filtered, everything} {
			want, err := local.GetActivityReport(ctx, f, q)
			require.NoError(t, err)
			got, err := store.GetActivityReport(ctx, f, q)
			require.NoError(t, err)
			require.Equal(t, want.Totals.OutputTokens, got.Totals.OutputTokens, "%s, project %q", label, f.Project)
			require.Equal(t, want.Totals.Cost, got.Totals.Cost, "%s, project %q", label, f.Project)
			require.Equal(t, want.Totals.Sessions, got.Totals.Sessions, "%s, project %q", label, f.Project)
		}
	}
	requireParity("first read")
	first := store.activityUsageRowsRead.Load()
	require.Positive(t, first)

	// With the refresh stopped, the pushed peer's rows come from the delta
	// prepared at read time.
	_, err = store.DB().ExecContext(ctx, "SYSTEM STOP VIEW prepare_usage")
	require.NoError(t, err)
	writePeer(4)
	_, err = syncer.Push(ctx, false, nil)
	require.NoError(t, err)
	requireParity("delta")
	reread := store.activityUsageRowsRead.Load() - first
	require.Equal(t, int64(len(peerMessages(4))), reread, "only the pushed peer's rows are read again")

	// The refresh stores the same rows the delta supplied, so nothing is
	// read again.
	_, err = store.DB().ExecContext(ctx, "SYSTEM START VIEW prepare_usage")
	require.NoError(t, err)
	_, err = store.DB().ExecContext(ctx, "SYSTEM REFRESH VIEW prepare_usage")
	require.NoError(t, err)
	_, err = store.DB().ExecContext(ctx, "SYSTEM WAIT VIEW prepare_usage")
	require.NoError(t, err)
	before := store.activityUsageRowsRead.Load()
	requireParity("refreshed")
	require.Equal(t, before, store.activityUsageRowsRead.Load(), "a refresh alone reads no rows")
}

// A host that has not published usage snapshots is read raw while every
// other host keeps its prepared rows. The mixed reads must give what raw
// reads of the whole mirror give, for the Activity report, a range still
// in progress, and daily usage.
func TestHostWithoutSnapshotsIsReadRaw(t *testing.T) {
	store, _, _ := newPushedStore(t)
	ctx := t.Context()
	// The revision is left to its default, so the copy is a new snapshot.
	// Each copy filters in a subquery: an outer WHERE would see the
	// replaced id.
	var snapshotColumns string
	require.NoError(t, store.DB().QueryRowContext(ctx, `SELECT arrayStringConcat(groupArray(name), ',')
		FROM (SELECT name FROM system.columns WHERE database = currentDatabase() AND table = 'usage_session_snapshots'
		AND default_kind != 'MATERIALIZED' AND name != 'revision' ORDER BY position)`).Scan(&snapshotColumns))
	// The other host once published a snapshot of one session, which the
	// refresh prepared, and has since pushed that session again without
	// one. Its stored prepared rows are stale and must not be read.
	for _, query := range []string{
		`INSERT INTO usage_session_snapshots (` + snapshotColumns + `) SELECT * EXCEPT (revision) REPLACE ('older-snapshot' AS id, 'host-b' AS machine, toUInt64(1) AS push_version)
		 FROM (SELECT * FROM usage_session_snapshots FINAL WHERE id = '` + fixtureAlphaID + `')`,
		`INSERT INTO sessions SELECT * REPLACE ('older-snapshot' AS id, 'host-b' AS machine, toUInt64(1) AS push_version)
		 FROM (SELECT * FROM sessions FINAL WHERE id = '` + fixtureAlphaID + `')`,
		"SYSTEM REFRESH VIEW prepare_usage",
		"SYSTEM WAIT VIEW prepare_usage",
		`INSERT INTO messages (session_id,ordinal,timestamp,model,token_usage,claude_message_id,claude_request_id,push_version) VALUES
		 ('older-snapshot',0,'2026-01-10 13:00:00','claude-test','{"input_tokens":1,"output_tokens":40}','older-m0','older-r0',2)`,
		`INSERT INTO sessions SELECT * REPLACE ('older-snapshot' AS id, 'host-b' AS machine, toUInt64(2) AS push_version, 1 AS message_count)
		 FROM (SELECT * FROM sessions FINAL WHERE id = '` + fixtureAlphaID + `')`,
	} {
		_, err := store.DB().ExecContext(ctx, query)
		require.NoError(t, err, query)
	}
	// The other host's newer session reaches the mirror through its message
	// and session tables only, as an older client publishes it.
	for _, query := range []string{
		`INSERT INTO messages (session_id,ordinal,timestamp,model,token_usage,claude_message_id,claude_request_id,push_version) VALUES
		 ('other-host',0,'2026-01-10 12:00:00','claude-test','{"input_tokens":3,"output_tokens":17}','other-m0','other-r0',1),
		 ('other-host',1,'2026-01-10 12:05:00','claude-test','{"input_tokens":4,"output_tokens":5}','other-m1','other-r1',1)`,
		`INSERT INTO sessions (id,project,machine,agent,started_at,ended_at,message_count,push_version) VALUES
		 ('other-host','elsewhere','host-b','claude','2026-01-10 12:00:00','2026-01-10 12:05:00',2,1)`,
	} {
		_, err := store.DB().ExecContext(ctx, query)
		require.NoError(t, err)
	}

	state, err := NewStoreFromDB(store.DB()).preparedUsageState(ctx)
	require.NoError(t, err)
	require.True(t, state.ready, "the host with snapshots keeps prepared reads")
	require.Equal(t, []string{"older-snapshot", "other-host"}, state.raw)

	day, err := activity.ResolveQuery(activity.QueryInput{Preset: "day", Date: "2026-01-10", Timezone: "UTC"},
		time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC))
	require.NoError(t, err)
	now := time.Now()
	partial, err := activity.ResolveQuery(activity.QueryInput{
		Preset: "custom", From: "2026-01-01T00:00:00Z", To: now.AddDate(0, 0, 2).UTC().Format(time.RFC3339), Timezone: "UTC",
	}, now)
	require.NoError(t, err)
	filter := db.AnalyticsFilter{Timezone: "UTC", IncludeSubagents: true}
	usageFilter := db.UsageFilter{Timezone: "UTC", Breakdowns: true}
	type reads struct {
		day, partial activity.Report
		daily        db.DailyUsageResult
	}
	read := func() reads {
		t.Helper()
		fresh := NewStoreFromDB(store.DB())
		var r reads
		r.day, err = fresh.GetActivityReport(ctx, filter, day)
		require.NoError(t, err)
		r.partial, err = fresh.GetActivityReport(ctx, filter, partial)
		require.NoError(t, err)
		r.daily, err = fresh.GetDailyUsage(ctx, usageFilter)
		require.NoError(t, err)
		r.day.ReportID, r.partial.ReportID = "", ""
		return r
	}
	mixed := read()
	snapshotHost := filter
	snapshotHost.Machine = "test-machine"
	withSnapshots, err := NewStoreFromDB(store.DB()).GetActivityReport(ctx, snapshotHost, day)
	require.NoError(t, err)
	require.Equal(t, 22+40, mixed.day.Totals.OutputTokens-withSnapshots.Totals.OutputTokens,
		"the raw host's current tokens are counted, and its stale prepared rows are not")

	// With the snapshot readiness keys gone, every read is raw.
	_, err = store.DB().ExecContext(ctx, "DELETE FROM sync_metadata WHERE startsWith(key, ?)", usageSnapshotReadyKeyBase+":")
	require.NoError(t, err)
	state, err = NewStoreFromDB(store.DB()).preparedUsageState(ctx)
	require.NoError(t, err)
	require.False(t, state.ready)
	raw := read()

	require.Equal(t, raw.day.Totals, mixed.day.Totals)
	require.Equal(t, raw.day.BySession, mixed.day.BySession)
	require.Equal(t, raw.partial.Totals, mixed.partial.Totals)
	require.Equal(t, raw.partial.BySession, mixed.partial.BySession)
	require.Equal(t, dailyUsageWire(t, raw.daily, false), dailyUsageWire(t, mixed.daily, false))
}
