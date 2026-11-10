//go:build chtest

package clickhouse

import (
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

// BenchmarkActivitySourceProbeAfterPush measures the checks an Activity
// report runs before its first byte, right after a push that rewrote a few
// sessions: each iteration writes the rows a push of probeBenchPushSessions
// sessions writes, then probes. It reports the wall time and the ClickHouse
// time of the probe.
func BenchmarkActivitySourceProbeAfterPush(b *testing.B) {
	ctx := b.Context()
	dsn, database := chtest.FreshDatabase(b)
	conn := chtest.Open(b, dsn, database)
	require.NoError(b, EnsureSchemaOn(ctx, conn))
	store := newTestStore(b, conn, Target{URL: dsn, Database: database})
	exec := func(query string, args ...any) {
		b.Helper()
		_, err := conn.ExecContext(ctx, query, args...)
		require.NoError(b, err, query)
	}
	exec("SYSTEM STOP VIEW prepare_usage")
	exec(fmt.Sprintf(`INSERT INTO sessions (id, machine, project, agent, started_at, local_modified_at,
		data_version, push_version)
		SELECT concat('s', toString(number)), 'bench', 'bench', 'claude',
			toDateTime64('2026-04-01 00:00:00', 6, 'UTC') + number,
			toDateTime64('2026-04-01 00:00:00', 6, 'UTC') + number, 1, %d
		FROM numbers(%d)`, probeBenchSnapshotWrites, probeBenchSessions))
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
	var digest string
	require.NoError(b, conn.QueryRowContext(ctx,
		`SELECT pricing_digest FROM prepared_usage LIMIT 1 SETTINGS final=0`).Scan(&digest))
	exec(fmt.Sprintf(`INSERT INTO usage_event_prices (pricing_digest, price_key, priced, push_version)
		SELECT ?, hex(sipHash128(number)), 1, 1 FROM numbers(%d)`, probeBenchPrices), digest)

	probe := func(comment string) {
		b.Helper()
		probeCtx := chdriver.Context(ctx, chdriver.WithSettings(chdriver.Settings{"log_comment": comment}))
		_, err := store.ActivityReportSourceProbe(probeCtx)
		require.NoError(b, err)
	}
	probe("probe-bench-cold")
	state, err := store.preparedUsageState(ctx)
	require.NoError(b, err)
	require.True(b, state.ready, "the seeded mirror must serve prepared usage")

	nextID := int64(probeBenchMessages + 1)
	push := func(version int) {
		b.Helper()
		ids := make([]string, probeBenchPushSessions)
		for i := range ids {
			ids[i] = fmt.Sprintf("'s%d'", (version*7919+i*104729)%probeBenchSessions)
		}
		list := "[" + strings.Join(ids, ",") + "]"
		exec(fmt.Sprintf(`INSERT INTO messages (id, session_id, ordinal, push_version)
			SELECT %d + number, arrayElement(%s, number %% %d + 1), 1000 + number, %d
			FROM numbers(%d)`, nextID, list, probeBenchPushSessions, version, 10*probeBenchPushSessions))
		nextID += int64(10 * probeBenchPushSessions)
		exec(fmt.Sprintf(`INSERT INTO usage_session_snapshots (id, machine, project, agent, started_at,
			data_version, push_version, usage_messages)
			SELECT id, 'bench', 'bench', 'claude', ts, 1, %[1]d,
				[(0, 'assistant', ts, 'claude-test', '', '{"input_tokens":20,"output_tokens":5}',
					concat('m', id), concat('r', id), '')]
			FROM (SELECT arrayJoin(%[2]s) AS id, toDateTime64('2026-11-01 00:00:00', 6, 'UTC') AS ts)`,
			version, list))
		exec(fmt.Sprintf(`INSERT INTO usage_event_prices (pricing_digest, price_key, priced, push_version)
			SELECT ?, concat('push-', toString(%d), '-', toString(number)), 1, %d FROM numbers(10)`,
			version, version), digest)
		exec(fmt.Sprintf(`INSERT INTO sessions (id, machine, project, agent, started_at, local_modified_at,
			data_version, push_version)
			SELECT id, 'bench', 'bench', 'claude', ts, ts, 1, %d
			FROM (SELECT arrayJoin(%s) AS id, now64(6, 'UTC') AS ts)`, version, list))
	}

	version := 1_000
	var wall time.Duration
	for b.Loop() {
		b.StopTimer()
		version++
		push(version)
		b.StartTimer()
		start := time.Now()
		probe("probe-bench-push")
		wall += time.Since(start)
	}
	exec("SYSTEM FLUSH LOGS")
	var clickhouseMS, queries uint64
	require.NoError(b, conn.QueryRowContext(ctx, `SELECT sum(query_duration_ms), count() FROM system.query_log
		WHERE current_database = currentDatabase() AND type = 'QueryFinish' AND log_comment = 'probe-bench-push'`,
	).Scan(&clickhouseMS, &queries))
	b.ReportMetric(float64(wall.Milliseconds())/float64(b.N), "wall-ms/op")
	b.ReportMetric(float64(clickhouseMS)/float64(b.N), "clickhouse-ms/op")
	b.ReportMetric(float64(queries)/float64(b.N), "queries/op")
}
