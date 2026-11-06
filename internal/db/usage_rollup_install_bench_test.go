//go:build fts5

package db

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Measure cold installs separately from fact extraction and pricing. Each batch
// has one daily row per session, as in an initial coverage pass.
func BenchmarkUsageRollupColdInstall(b *testing.B) {
	cache, err := initializeUsageCache(b.Context(), filepath.Join(b.TempDir(), "usage.db"), "benchmark", true)
	require.NoError(b, err)
	b.Cleanup(func() { require.NoError(b, cache.db.Close()) })
	conn, err := cache.db.Conn(b.Context())
	require.NoError(b, err)
	b.Cleanup(func() { require.NoError(b, conn.Close()) })
	builds := make([]usageRollupBuild, usageFillInstallBatchSize)
	sessions := make(map[string]usageQuerySession, len(builds))
	for i := range builds {
		id := fmt.Sprintf("session-%d", i)
		sessions[id] = usageQuerySession{ID: id}
		_, err := conn.ExecContext(b.Context(), `INSERT INTO usage_cached_sessions(session_id, source_sync_marker, source_transcript_rev, usage_event_fingerprint, install_revision, source_usage_generation) VALUES (?, '', '', '', 1, '')`, id)
		require.NoError(b, err)
		builds[i] = usageRollupBuild{
			SessionID: id, Agent: "codex", StartedAt: "2026-08-10T12:00:00Z",
			Source: usageSourceVersion{SessionID: id}, FactRevision: 1,
			Daily: []usageDailyContribution{{
				LocalDate: "2026-08-10", ReportedModel: "model-a", PricedModel: "model-a",
				RateHash: "rate-a", PricingTimestamp: "2026-08-10T12:00:00Z", RateOK: true,
				InputTokens: 100, OutputTokens: 20, CostMicrodollars: 200, ComputedRequestCount: 1,
			}},
		}
	}
	_, err = loadUsageRollupFacts(b.Context(), conn, sessions)
	require.NoError(b, err)
	identity := usageTimezoneIdentityFor(time.UTC, nil)
	b.ResetTimer()
	for range b.N {
		require.NoError(b, installUsageRollupBuilds(b.Context(), conn, identity, time.UTC, builds, usageDedupIdentitySet{}))
		b.StopTimer()
		var rows, input, cost int64
		require.NoError(b, conn.QueryRowContext(b.Context(), `SELECT COUNT(*), SUM(input_tokens), SUM(estimated_cost_microdollars) FROM usage_daily_rollups`).Scan(&rows, &input, &cost))
		require.Equal(b, int64(256), rows)
		require.Equal(b, int64(25600), input)
		require.Equal(b, int64(51200), cost)
		require.NoError(b, conn.QueryRowContext(b.Context(), `SELECT SUM(row_count), SUM(input_tokens), SUM(estimated_cost_microdollars) FROM usage_day_totals`).Scan(&rows, &input, &cost))
		require.Equal(b, int64(256), rows)
		require.Equal(b, int64(25600), input)
		require.Equal(b, int64(51200), cost)
		_, err := conn.ExecContext(b.Context(), `DELETE FROM usage_rollup_timezones`)
		require.NoError(b, err)
		b.StartTimer()
	}
}
