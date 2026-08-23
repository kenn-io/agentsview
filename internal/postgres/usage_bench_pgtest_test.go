//go:build pgtest

package postgres

import (
	"context"
	"database/sql"
	"testing"

	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
)

const pgUsageBenchmarkSchema = "agentsview_pg_usage_bench"

type pgUsageBenchmarkFixture struct {
	local  *db.DB
	remote *Store
	sync   *Sync
	filter db.UsageFilter
}

func openPGUsageBenchmarkFixture(b *testing.B) *pgUsageBenchmarkFixture {
	b.Helper()
	req := require.New(b)
	pgURL := testPGURL(b)
	admin, err := sql.Open("pgx", pgURL)
	req.NoError(err)
	_, err = admin.Exec("DROP SCHEMA IF EXISTS " + pgUsageBenchmarkSchema + " CASCADE")
	req.NoError(err)
	req.NoError(admin.Close())

	local, err := db.Open(b.TempDir() + "/usage-bench.db")
	req.NoError(err)
	seedUsageParityFixture(b, local)
	syncer, err := New(pgURL, pgUsageBenchmarkSchema, local, "bench-machine", true, SyncOptions{})
	req.NoError(err)
	_, err = syncer.Push(b.Context(), true, nil)
	req.NoError(err)
	remote, err := NewStore(pgURL, pgUsageBenchmarkSchema, true)
	req.NoError(err)
	b.Cleanup(func() {
		_ = remote.Close()
		_ = syncer.Close()
		_ = local.Close()
		admin, _ := sql.Open("pgx", pgURL)
		_, _ = admin.Exec("DROP SCHEMA IF EXISTS " + pgUsageBenchmarkSchema + " CASCADE")
		_ = admin.Close()
	})

	var version string
	req.NoError(remote.DB().QueryRowContext(b.Context(), "SHOW server_version").Scan(&version))
	b.Logf("pg_version=%s fixture_sessions=%d fixture_messages=%d fixture_usage_events=%d", version, 5, 4, 1)
	return &pgUsageBenchmarkFixture{
		local: local, remote: remote, sync: syncer,
		filter: db.UsageFilter{From: "2026-08-12", To: "2026-08-12", Timezone: "UTC"},
	}
}

func TestPGUsageBenchmarkFixture(t *testing.T) {
	pgURL := testPGURL(t)
	const schema = "agentsview_usage_benchmark_fixture_test"
	cleanNamedPGSchema(t, pgURL, schema)
	t.Cleanup(func() { cleanNamedPGSchema(t, pgURL, schema) })
	local := testDB(t)
	seedUsageParityFixture(t, local)
	syncer, err := New(pgURL, schema, local, "bench-fixture-machine", true, SyncOptions{})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, syncer.Close()) })
	_, err = syncer.Push(t.Context(), true, nil)
	require.NoError(t, err)
	remote, err := NewStore(pgURL, schema, true)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, remote.Close()) })

	for _, tc := range []struct {
		name       string
		from, to   string
		breakdowns bool
	}{
		{name: "one-day", from: "2026-08-12", to: "2026-08-12"},
		{name: "seven-day", from: "2026-08-06", to: "2026-08-12"},
		{name: "thirty-day", from: "2026-07-14", to: "2026-08-12", breakdowns: true},
		{name: "all-history", breakdowns: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			filter := db.UsageFilter{From: tc.from, To: tc.to, Timezone: "UTC", Breakdowns: tc.breakdowns}
			localResult := captureUsageParitySnapshot(t, local, filter)
			remoteResult := captureUsageParitySnapshot(t, remote, filter)
			require.NotEmpty(t, remoteResult.Top)
			require.NotEmpty(t, remoteResult.Daily.Dates)
			require.Equal(t, localResult, remoteResult)
			require.NotZero(t, remoteResult.Counts.Total)
			require.NotZero(t, remoteResult.MatchingSessionCount)
		})
	}
}

func BenchmarkPGUsageRead(b *testing.B) {
	fixture := openPGUsageBenchmarkFixture(b)
	ctx := context.Background()
	for _, tc := range []struct {
		name       string
		from, to   string
		breakdowns bool
	}{
		{name: "one-day", from: "2026-08-12", to: "2026-08-12"},
		{name: "seven-day", from: "2026-08-06", to: "2026-08-12"},
		{name: "thirty-day", from: "2026-07-14", to: "2026-08-12", breakdowns: true},
		{name: "all-history", breakdowns: true},
	} {
		b.Run(tc.name, func(b *testing.B) {
			filter := db.UsageFilter{From: tc.from, To: tc.to, Timezone: "UTC", Breakdowns: tc.breakdowns}
			b.ReportMetric(5, "rows_scanned")
			b.ReportMetric(178, "bytes_token_usage")
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				daily, err := fixture.remote.GetDailyUsage(ctx, filter)
				require.NoError(b, err)
				top, err := fixture.remote.GetTopSessionsByCost(ctx, filter, 10)
				require.NoError(b, err)
				counts, err := fixture.remote.GetUsageSessionCounts(ctx, filter)
				require.NoError(b, err)
				matching, err := fixture.remote.GetUsageMatchingSessionCount(ctx, filter)
				require.NoError(b, err)
				session, err := fixture.remote.GetSessionUsage(ctx, "snapshot-winner", true)
				require.NoError(b, err)
				require.NotEmpty(b, daily.Daily)
				require.NotEmpty(b, top)
				require.NotZero(b, counts.Total)
				require.NotZero(b, matching)
				require.NotNil(b, session)
			}
		})
	}
}

func BenchmarkPGUsageRefresh(b *testing.B) {
	fixture := openPGUsageBenchmarkFixture(b)
	ctx := context.Background()
	b.Run("ColdPush", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			result, err := fixture.sync.Push(ctx, true, nil)
			require.NoError(b, err)
			require.NotZero(b, result.SessionsPushed)
			b.ReportMetric(float64(result.SessionsPushed), "sessions_pushed")
			b.ReportMetric(float64(result.MessagesPushed), "messages_pushed")
		}
	})
	b.Run("DeltaPush", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			session := usageParitySessionFixture("snapshot-winner", "project-b", "claude", "2026-08-12T10:01:00Z", 10+i+1, 20)
			require.NoError(b, fixture.local.UpsertSession(session))
			result, err := fixture.sync.Push(ctx, false, nil)
			require.NoError(b, err)
			b.ReportMetric(float64(result.SessionsPushed), "sessions_pushed")
			b.ReportMetric(float64(result.MessagesPushed), "messages_pushed")
		}
	})
	b.Run("CatalogProbe", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			var tables int
			err := fixture.remote.DB().QueryRowContext(ctx, `
				SELECT count(*) FROM pg_catalog.pg_class c
				JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
				WHERE n.nspname = $1 AND c.relkind IN ('r', 'i')`, pgUsageBenchmarkSchema).Scan(&tables)
			require.NoError(b, err)
			require.NotZero(b, tables)
			b.ReportMetric(float64(tables), "catalog_rows")
		}
	})
}
