//go:build chtest

package clickhouse

import (
	"context"
	"fmt"
	"testing"
	"time"

	chdriver "github.com/ClickHouse/clickhouse-go/v2"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
)

// Mirror shape the ID-set benchmark seeds. A 365-day Analytics load on a
// long-lived mirror selected about 10,800 sessions, which the model lookup
// once read in 12 batches of 900 IDs.
const (
	benchIDSetSessions = 10800
	benchIDSetMessages = 40
	benchIDSetModels   = 20
)

// BenchmarkAnalyticsIDSetReads times the analytics reads that look up the
// selected sessions' messages by ID:
//
//   - summary: the Summary panel's model list.
//   - summary_hour: the model list under an hour filter.
//   - activity_model_hour: the activity timeline's tool counts under a
//     model and hour filter.
//
// Besides time, it reports the statements each read ran and the server CPU
// and rows read they used, from system.query_log.
func BenchmarkAnalyticsIDSetReads(b *testing.B) {
	store, _ := seedIDSetMirror(b, benchIDSetSessions, benchIDSetMessages, benchIDSetModels)
	january := db.AnalyticsFilter{From: "2026-01-01", To: "2026-01-31", Timezone: "UTC"}
	hour := 10
	atTen := january
	atTen.Hour = &hour
	sharedAtTen := atTen
	sharedAtTen.Model = "shared"

	for _, tc := range []struct {
		name string
		read func(context.Context) error
	}{
		{"summary", func(ctx context.Context) error {
			_, err := store.GetAnalyticsSummary(ctx, january)
			return err
		}},
		{"summary_hour", func(ctx context.Context) error {
			_, err := store.GetAnalyticsSummary(ctx, atTen)
			return err
		}},
		{"activity_model_hour", func(ctx context.Context) error {
			_, err := store.GetAnalyticsActivity(ctx, sharedAtTen, "day")
			return err
		}},
	} {
		b.Run(tc.name, func(b *testing.B) {
			comment := fmt.Sprintf("bench-id-set-%s-%d", tc.name, time.Now().UnixNano())
			ctx := chdriver.Context(b.Context(),
				chdriver.WithSettings(chdriver.Settings{"log_comment": comment}))
			for b.Loop() {
				require.NoError(b, tc.read(ctx))
			}
			b.StopTimer()
			_, err := store.conn.ExecContext(b.Context(), `SYSTEM FLUSH LOGS`)
			require.NoError(b, err)
			var statements, readRows uint64
			var cpuSeconds float64
			require.NoError(b, store.conn.QueryRowContext(b.Context(), `
				SELECT count(),
					sum(ProfileEvents['UserTimeMicroseconds'] + ProfileEvents['SystemTimeMicroseconds']) / 1e6,
					sum(read_rows)
				FROM system.query_log
				WHERE type = 'QueryFinish' AND log_comment = ?`,
				comment).Scan(&statements, &cpuSeconds, &readRows))
			n := float64(b.N)
			b.ReportMetric(float64(statements)/n, "statements/op")
			b.ReportMetric(cpuSeconds/n, "server-cpu-s/op")
			b.ReportMetric(float64(readRows)/n, "read-rows/op")
		})
	}
}
