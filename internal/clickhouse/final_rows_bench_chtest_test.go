//go:build chtest

package clickhouse

import (
	"fmt"
	"maps"
	"slices"
	"testing"
	"time"

	chdriver "github.com/ClickHouse/clickhouse-go/v2"
	"github.com/stretchr/testify/require"
)

// benchFinalSessions and benchFinalChangedKeys size the sessions table the
// tracker benchmark checks: a mirror of a few hundred thousand sessions and
// a set of pushes since the last check whose keys come close to the 2,000
// keys one incremental check used to read.
const (
	benchFinalSessions    = 300_000
	benchFinalBaseParts   = 6
	benchFinalChangedKeys = 1_900
)

// benchSessionsSQL inserts the sessions numbered [from, from+n) at a push
// version. Session n always gets the same id, so a later push of the same
// numbers rewrites those sessions.
func benchSessionsSQL(from, n, version int) string {
	return fmt.Sprintf(`INSERT INTO sessions (id, project, machine, agent, first_message, display_name,
			started_at, ended_at, message_count, user_message_count, file_path, file_size, cwd, git_branch,
			total_output_tokens, push_version)
		SELECT toString(reinterpretAsUUID(sipHash128(number))) AS id,
			concat('project-', toString(number %% 40)),
			['laptop', 'desktop', 'build-box'][number %% 3 + 1],
			['claude', 'codex', 'gemini', 'copilot'][number %% 4 + 1],
			substring(repeat(hex(sipHash128(number, %[3]d)), 10), 1, 80 + number %% 240),
			concat('Session ', toString(number)),
			toDateTime64('2026-01-01 00:00:00', 3) + toIntervalSecond(number * 90),
			toDateTime64('2026-01-01 00:00:00', 3) + toIntervalSecond(number * 90 + 1800),
			10 + number %% 300, 2 + number %% 40,
			concat('/sessions/', id, '.jsonl'), 4096 + number * 7 %% 900000,
			concat('/work/project-', toString(number %% 40)), 'main',
			number * 13 %% 200000, %[3]d
		FROM numbers(%[1]d, %[2]d)`, from, n, version)
}

// BenchmarkFinalRowsIncrementalSessions times one incremental check of the
// sessions tracker after a set of pushes that changed about 1,900 sessions,
// spread over a varying number of new parts. Besides time, it reports from
// system.query_log the statements the check ran, their logged text length,
// server time, and the rows and bytes they read, plus the length of the
// _part list that names the table's active parts.
func BenchmarkFinalRowsIncrementalSessions(b *testing.B) {
	for _, pushes := range []int{20, 100} {
		b.Run(fmt.Sprintf("pushes=%d", pushes), func(b *testing.B) {
			store := newFinalRowsStore(b)
			ctx := b.Context()
			exec := func(stmts ...string) {
				b.Helper()
				for _, stmt := range stmts {
					_, err := store.DB().ExecContext(ctx, stmt)
					require.NoError(b, err, stmt)
				}
			}
			exec("SYSTEM STOP MERGES sessions")
			per := benchFinalSessions / benchFinalBaseParts
			for i := range benchFinalBaseParts {
				exec(benchSessionsSQL(i*per, per, 1))
			}
			f := &finalRows{spec: sessionsFinalTable}
			snapshot, err := store.readPartsSnapshot(ctx)
			require.NoError(b, err)
			change, err := store.prepareFinalRows(ctx, f, snapshot["sessions"])
			require.NoError(b, err)
			f.apply(change)

			// Each push rewrites sessions spread over the whole key range and
			// adds a few new ones, as a sync of recent activity does.
			keysPerPush := benchFinalChangedKeys / pushes
			for p := range pushes {
				updated := keysPerPush * 9 / 10
				exec(fmt.Sprintf(`INSERT INTO sessions SELECT * REPLACE (%d AS push_version, 'renamed' AS display_name)
					FROM sessions WHERE id IN (SELECT toString(reinterpretAsUUID(sipHash128(number)))
						FROM numbers(%d) WHERE number %% %d = %d LIMIT %d)`,
					2+p, benchFinalSessions, pushes, p, updated))
				exec(benchSessionsSQL(benchFinalSessions+p*keysPerPush, keysPerPush-updated, 2+p))
			}
			snapshot, err = store.readPartsSnapshot(ctx)
			require.NoError(b, err)
			tp := snapshot["sessions"]
			require.Len(b, tp.parts, benchFinalBaseParts+2*pushes)
			names := make([]string, len(tp.parts))
			for i, p := range tp.parts {
				names[i] = p.name
			}

			comment := fmt.Sprintf("bench-final-keys-%d-%d", pushes, time.Now().UnixNano())
			loopCtx := chdriver.Context(ctx, chdriver.WithSettings(chdriver.Settings{"log_comment": comment}))
			checks := 0
			for b.Loop() {
				change, err := store.prepareFinalRows(loopCtx, f, tp)
				require.NoError(b, err)
				require.False(b, change.full, "the check must read only the changed keys")
				require.Len(b, slices.Collect(maps.Keys(change.parts)), len(names))
				checks++
			}
			b.StopTimer()
			exec("SYSTEM FLUSH LOGS query_log")
			var statements, textBytes, maxText, durationMS, serverMicros, readRows, readBytes uint64
			require.NoError(b, store.DB().QueryRowContext(ctx, `SELECT count(), sum(length(query)),
					max(length(query)), sum(query_duration_ms),
					sum(dateDiff('microsecond', query_start_time_microseconds, event_time_microseconds)),
					sum(read_rows), sum(read_bytes)
				FROM system.query_log
				WHERE type = 'QueryFinish' AND log_comment = ?`, comment).Scan(
				&statements, &textBytes, &maxText, &durationMS, &serverMicros, &readRows, &readBytes))
			n := float64(checks)
			b.ReportMetric(float64(statements)/n, "queries/op")
			b.ReportMetric(float64(textBytes)/n, "query-text-B/op")
			b.ReportMetric(float64(maxText), "max-query-text-B")
			b.ReportMetric(float64(len(partsClause(names))), "part-list-B")
			b.ReportMetric(float64(durationMS)/n, "server-ms/op")
			b.ReportMetric(float64(serverMicros)/n, "server-us/op")
			b.ReportMetric(float64(readRows)/n, "read-rows/op")
			b.ReportMetric(float64(readBytes)/n, "read-B/op")
		})
	}
}
