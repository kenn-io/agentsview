//go:build pgtest

package postgres

import (
	"context"
	"fmt"
	"net"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/stretchr/testify/require"
)

// BenchmarkPGAutomationPreparation reports performance metrics without enforcing thresholds.
func BenchmarkPGAutomationPreparation(b *testing.B) {
	for _, size := range []int{2_000, 20_000} {
		b.Run(fmt.Sprint(size), func(b *testing.B) {
			url := testPGURL(b)
			const schema = "agentsview_automation_bench"
			cleanNamedPGSchema(b, url, schema)
			b.Cleanup(func() { cleanNamedPGSchema(b, url, schema) })
			pg, err := Open(b.Context(), url, schema, true)
			require.NoError(b, err)
			b.Cleanup(func() { require.NoError(b, pg.Close()) })
			require.NoError(b, EnsureSchema(b.Context(), pg, schema))
			_, err = pg.ExecContext(b.Context(), `INSERT INTO sessions (id, machine, project, agent, first_message, user_message_count)
			 SELECT 'session-' || n, 'host', 'project', 'codex', 'Explain this function.', 1 FROM generate_series(1, $1) n`, size)
			require.NoError(b, err)
			require.NoError(b, backfillIsAutomatedPG(b.Context(), pg))
			_, err = pg.ExecContext(b.Context(), `ANALYZE sessions`)
			require.NoError(b, err)
			config, err := pgx.ParseConfig(url)
			require.NoError(b, err)
			config.RuntimeParams["search_path"] = schema
			var received atomic.Int64
			config.AfterNetConnect = func(_ context.Context, _ *pgconn.Config, conn net.Conn) (net.Conn, error) {
				return &pushResponseConn{Conn: conn, received: &received}, nil
			}
			metered := stdlib.OpenDB(*config)
			b.Cleanup(func() { require.NoError(b, metered.Close()) })
			require.NoError(b, metered.PingContext(b.Context()))
			for _, mode := range []string{"fresh", "reused", "audit"} {
				b.Run(mode, func(b *testing.B) {
					syncer := &Sync{pg: metered, schema: schema}
					require.NoError(b, syncer.EnsureSchema(b.Context()))
					received.Store(0)
					var prefetched, full int
					b.ResetTimer()
					for b.Loop() {
						if mode == "audit" {
							progress, err := backfillIsAutomatedPGWithProgress(b.Context(), metered)
							require.NoError(b, err)
							prefetched += progress.RowsPrefetched
							full += progress.RowsFullText
						} else {
							if mode == "fresh" {
								syncer = &Sync{pg: metered, schema: schema}
							}
							require.NoError(b, syncer.EnsureSchema(b.Context()))
						}
					}
					b.ReportMetric(float64(received.Load())/float64(b.N), "response-B/op")
					if mode == "audit" {
						b.ReportMetric(float64(prefetched)/float64(b.N), "candidates/op")
						b.ReportMetric(float64(full)/float64(b.N), "fallback/op")
					}
				})
			}
		})
	}
}
