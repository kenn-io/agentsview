//go:build chtest

package clickhouse

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	chdriver "github.com/ClickHouse/clickhouse-go/v2"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/clickhouse/chtest"
)

// Scale of the mirror the probe benchmark seeds: a long-lived mirror with
// about a hundred thousand sessions, three snapshot rows per session not yet
// merged together, millions of messages, and one pricing digest's records.
const (
	probeBenchSessions       = 100_000
	probeBenchSnapshotWrites = 3
	probeBenchMessages       = 5_000_000
	probeBenchPrices         = 1_500_000
	probeBenchPushSessions   = 5
)

// probeBench is a mirror seeded at the probe benchmarks' scale.
type probeBench struct {
	conn   *sql.DB
	target Target
	store  *Store
	digest string
	nextID int64
}

// newProbeBench seeds the mirror. sessionBytes pads each session row's
// first message, so a full read of sessions costs what it costs on a real
// mirror.
func newProbeBench(b *testing.B, sessionBytes int) *probeBench {
	b.Helper()
	ctx := b.Context()
	dsn, database := chtest.FreshDatabase(b)
	conn := chtest.Open(b, dsn, database)
	require.NoError(b, EnsureSchemaOn(ctx, conn))
	target := Target{URL: dsn, Database: database}
	pb := &probeBench{conn: conn, target: target, store: newTestStore(b, conn, target), nextID: probeBenchMessages + 1}
	exec := func(query string, args ...any) {
		b.Helper()
		_, err := conn.ExecContext(ctx, query, args...)
		require.NoError(b, err, query)
	}
	exec("SYSTEM STOP VIEW prepare_usage")
	exec(fmt.Sprintf(`INSERT INTO sessions (id, machine, project, agent, started_at, local_modified_at,
		data_version, push_version, first_message)
		SELECT concat('s', toString(number)), 'bench', 'bench', 'claude',
			toDateTime64('2026-04-01 00:00:00', 6, 'UTC') + number,
			toDateTime64('2026-04-01 00:00:00', 6, 'UTC') + number, 1, %d, randomPrintableASCII(%d)
		FROM numbers(%d)`, probeBenchSnapshotWrites, sessionBytes, probeBenchSessions))
	for version := 1; version <= probeBenchSnapshotWrites; version++ {
		exec(fmt.Sprintf(`INSERT INTO usage_session_snapshots (id, machine, project, agent, started_at,
			data_version, push_version, usage_messages)
			SELECT concat('s', toString(number)), 'bench', 'bench', 'claude', ts, 1, %d,
				[(0, 'assistant', ts, 'claude-test', '', '{"input_tokens":10,"output_tokens":5}',
					concat('m', toString(number)), concat('r', toString(number)), '')]
			FROM (SELECT number, toDateTime64('2026-04-01 00:00:00', 6, 'UTC') + number AS ts
				FROM numbers(%d))`, version, probeBenchSessions))
	}
	exec(fmt.Sprintf(`INSERT INTO messages (id, session_id, ordinal, push_version)
		SELECT number + 1, concat('s', toString(number %% %[2]d)), intDiv(number, %[2]d), %[3]d
		FROM numbers(%[1]d)`, probeBenchMessages, probeBenchSessions, probeBenchSnapshotWrites))
	exec(`INSERT INTO sync_metadata (key, value, push_version) VALUES (?, '1', 1)`,
		usageSnapshotReadyKeyBase+":bench")
	exec("SYSTEM START VIEW prepare_usage")
	exec("SYSTEM REFRESH VIEW prepare_usage")
	exec("SYSTEM WAIT VIEW prepare_usage")
	exec("SYSTEM STOP VIEW prepare_usage")
	require.NoError(b, conn.QueryRowContext(ctx,
		`SELECT pricing_digest FROM prepared_usage LIMIT 1 SETTINGS final=0`).Scan(&pb.digest))
	exec(fmt.Sprintf(`INSERT INTO usage_event_prices (pricing_digest, price_key, priced, push_version)
		SELECT ?, hex(sipHash128(number)), 1, 1 FROM numbers(%d)`, probeBenchPrices), pb.digest)
	pb.probe(b, pb.store, "probe-bench-cold")
	state, err := pb.store.preparedUsageState(ctx)
	require.NoError(b, err)
	require.True(b, state.ready, "the seeded mirror must serve prepared usage")
	return pb
}

// probe runs the checks an Activity report runs before its first byte,
// with comment as the queries' log comment.
func (pb *probeBench) probe(b *testing.B, store *Store, comment string) {
	b.Helper()
	ctx := chdriver.Context(b.Context(), chdriver.WithSettings(chdriver.Settings{"log_comment": comment}))
	_, err := store.ActivityReportSourceProbe(ctx)
	require.NoError(b, err)
}

// push writes the rows a push of probeBenchPushSessions sessions writes.
// It is safe to run beside probes, not beside another push.
func (pb *probeBench) push(ctx context.Context, version int) error {
	ids := make([]string, probeBenchPushSessions)
	for i := range ids {
		ids[i] = fmt.Sprintf("'s%d'", (version*7919+i*104729)%probeBenchSessions)
	}
	list := "[" + strings.Join(ids, ",") + "]"
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{sql: fmt.Sprintf(`INSERT INTO messages (id, session_id, ordinal, push_version)
			SELECT %d + number, arrayElement(%s, number %% %d + 1), 1000 + number, %d
			FROM numbers(%d)`, pb.nextID, list, probeBenchPushSessions, version, 10*probeBenchPushSessions)},
		{sql: fmt.Sprintf(`INSERT INTO usage_session_snapshots (id, machine, project, agent, started_at,
			data_version, push_version, usage_messages)
			SELECT id, 'bench', 'bench', 'claude', ts, 1, %[1]d,
				[(0, 'assistant', ts, 'claude-test', '', '{"input_tokens":20,"output_tokens":5}',
					concat('m', id), concat('r', id), '')]
			FROM (SELECT arrayJoin(%[2]s) AS id, toDateTime64('2026-11-01 00:00:00', 6, 'UTC') AS ts)`,
			version, list)},
		{sql: fmt.Sprintf(`INSERT INTO usage_event_prices (pricing_digest, price_key, priced, push_version)
			SELECT ?, concat('push-', toString(%d), '-', toString(number)), 1, %d FROM numbers(10)`,
			version, version), args: []any{pb.digest}},
		{sql: fmt.Sprintf(`INSERT INTO sessions (id, machine, project, agent, started_at, local_modified_at,
			data_version, push_version)
			SELECT id, 'bench', 'bench', 'claude', ts, ts, 1, %d
			FROM (SELECT arrayJoin(%s) AS id, now64(6, 'UTC') AS ts)`, version, list)},
	} {
		if _, err := pb.conn.ExecContext(ctx, q.sql, q.args...); err != nil {
			return fmt.Errorf("pushing version %d: %w", version, err)
		}
	}
	pb.nextID += int64(10 * probeBenchPushSessions)
	return nil
}

// report adds the ClickHouse time, query count, and full tracker reads of
// the queries logged under comment, per operation.
func (pb *probeBench) report(b *testing.B, comment string) {
	b.Helper()
	ctx := b.Context()
	_, err := pb.conn.ExecContext(ctx, "SYSTEM FLUSH LOGS")
	require.NoError(b, err)
	var clickhouseMS, queries, fullReads uint64
	// A full tracker read selects every row's hash with no key list; the
	// log shows a key lookup's keys inline, in has(...).
	require.NoError(b, pb.conn.QueryRowContext(ctx, `SELECT sum(query_duration_ms), count(),
		countIf(query LIKE '%sipHash128Reference(tuple(%' AND query NOT LIKE '%has(%' AND query NOT LIKE '%sumWithOverflow%'
			AND (query LIKE '%FROM sessions%' OR query LIKE '%FROM usage_session_snapshots%'))
		FROM system.query_log
		WHERE current_database = currentDatabase() AND type = 'QueryFinish' AND log_comment = ?`, comment,
	).Scan(&clickhouseMS, &queries, &fullReads))
	b.ReportMetric(float64(clickhouseMS)/float64(b.N), "clickhouse-ms/op")
	b.ReportMetric(float64(queries)/float64(b.N), "queries/op")
	b.ReportMetric(float64(fullReads)/float64(b.N), "full-reads/op")
}

// BenchmarkActivitySourceProbeAfterPush measures the checks an Activity
// report runs before its first byte, right after a push that rewrote a few
// sessions: each iteration writes the rows a push of probeBenchPushSessions
// sessions writes, then probes. It reports the wall time and the ClickHouse
// time of the probe.
func BenchmarkActivitySourceProbeAfterPush(b *testing.B) {
	pb := newProbeBench(b, 0)
	version := 1_000
	var wall time.Duration
	for b.Loop() {
		b.StopTimer()
		version++
		require.NoError(b, pb.push(b.Context(), version))
		b.StartTimer()
		start := time.Now()
		pb.probe(b, pb.store, "probe-bench-push")
		wall += time.Since(start)
	}
	b.ReportMetric(float64(wall.Milliseconds())/float64(b.N), "wall-ms/op")
	pb.report(b, "probe-bench-push")
}

// probeBenchSessionBytes pads each session row so a full read of sessions
// reads about as much as on a mirror whose sessions carry real text.
const probeBenchSessionBytes = 4096

// BenchmarkActivitySourceProbeDuringPushes measures the probe while another
// client pushes once a second, so parts move while the probe reads. The
// cold case probes from a store that has read nothing yet, as after a
// restart or a long idle spell; the warm case reuses one store.
func BenchmarkActivitySourceProbeDuringPushes(b *testing.B) {
	pb := newProbeBench(b, probeBenchSessionBytes)
	ctx, cancel := context.WithCancel(b.Context())
	pushErr := make(chan error, 1)
	go func() {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for version := 2_000; ; version++ {
			select {
			case <-ctx.Done():
				pushErr <- nil
				return
			case <-ticker.C:
			}
			if err := pb.push(ctx, version); err != nil && ctx.Err() == nil {
				pushErr <- err
				return
			}
		}
	}()
	b.Cleanup(func() {
		cancel()
		require.NoError(b, <-pushErr)
	})
	for _, tc := range []struct {
		name string
		cold bool
	}{{"cold", true}, {"warm", false}} {
		// -count reruns a sub-benchmark on the same mirror, so each run
		// logs its queries under its own comment.
		runs := 0
		b.Run(tc.name, func(b *testing.B) {
			runs++
			comment := fmt.Sprintf("probe-bench-during-%s-%d", tc.name, runs)
			var wall time.Duration
			for b.Loop() {
				store := pb.store
				if tc.cold {
					b.StopTimer()
					store = newTestStore(b, pb.conn, pb.target)
					b.StartTimer()
				}
				start := time.Now()
				pb.probe(b, store, comment)
				wall += time.Since(start)
			}
			b.ReportMetric(float64(wall.Milliseconds())/float64(b.N), "wall-ms/op")
			pb.report(b, comment)
		})
	}
}
