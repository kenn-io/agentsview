//go:build pgtest

package postgres

import (
	"context"
	"database/sql"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
)

const pgUsageBenchmarkSchema = "agentsview_pg_usage_bench"

type pgUsageBenchmarkCase struct {
	name       string
	from, to   string
	breakdowns bool
}

func pgUsageBenchmarkCases() []pgUsageBenchmarkCase {
	return []pgUsageBenchmarkCase{
		{name: "one-day/no-breakdowns", from: "2026-08-12", to: "2026-08-12"},
		{name: "one-day/breakdowns", from: "2026-08-12", to: "2026-08-12", breakdowns: true},
		{name: "seven-day/no-breakdowns", from: "2026-08-06", to: "2026-08-12"},
		{name: "seven-day/breakdowns", from: "2026-08-06", to: "2026-08-12", breakdowns: true},
		{name: "thirty-day/no-breakdowns", from: "2026-07-14", to: "2026-08-12"},
		{name: "thirty-day/breakdowns", from: "2026-07-14", to: "2026-08-12", breakdowns: true},
		{name: "all-history/no-breakdowns"},
		{name: "all-history/breakdowns", breakdowns: true},
	}
}

func pgUsageBenchmarkFilter(tc pgUsageBenchmarkCase) db.UsageFilter {
	return db.UsageFilter{From: tc.from, To: tc.to, Timezone: "UTC", Breakdowns: tc.breakdowns}
}

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

	for _, tc := range pgUsageBenchmarkCases() {
		t.Run(tc.name, func(t *testing.T) {
			filter := pgUsageBenchmarkFilter(tc)
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
	for _, tc := range pgUsageBenchmarkCases() {
		b.Run(tc.name, func(b *testing.B) {
			filter := pgUsageBenchmarkFilter(tc)
			rowsScanned, tokenUsageBytes := measurePGUsageFixture(b, fixture.remote, filter)
			validatePGUsageBenchmarkRead(b, fixture.remote, ctx, filter)
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				daily, err := fixture.remote.GetDailyUsage(ctx, filter)
				if err != nil {
					b.Fatal(err)
				}
				top, err := fixture.remote.GetTopSessionsByCost(ctx, filter, 10)
				if err != nil {
					b.Fatal(err)
				}
				counts, err := fixture.remote.GetUsageSessionCounts(ctx, filter)
				if err != nil {
					b.Fatal(err)
				}
				matching, err := fixture.remote.GetUsageMatchingSessionCount(ctx, filter)
				if err != nil {
					b.Fatal(err)
				}
				session, err := fixture.remote.GetSessionUsage(ctx, "snapshot-winner", true)
				if err != nil {
					b.Fatal(err)
				}
				if len(daily.Daily) == 0 || len(top) == 0 || counts.Total == 0 || matching == 0 || session == nil {
					b.Fatal("usage benchmark returned an empty result")
				}
			}
			b.ReportMetric(float64(rowsScanned), "rows_scanned")
			b.ReportMetric(float64(tokenUsageBytes), "bytes_token_usage")
		})
	}
}

func validatePGUsageBenchmarkRead(b *testing.B, store *Store, ctx context.Context, filter db.UsageFilter) {
	b.Helper()
	daily, err := store.GetDailyUsage(ctx, filter)
	require.NoError(b, err)
	top, err := store.GetTopSessionsByCost(ctx, filter, 10)
	require.NoError(b, err)
	counts, err := store.GetUsageSessionCounts(ctx, filter)
	require.NoError(b, err)
	matching, err := store.GetUsageMatchingSessionCount(ctx, filter)
	require.NoError(b, err)
	session, err := store.GetSessionUsage(ctx, "snapshot-winner", true)
	require.NoError(b, err)
	require.NotEmpty(b, daily.Daily)
	require.NotEmpty(b, top)
	require.NotZero(b, counts.Total)
	require.NotZero(b, matching)
	require.NotNil(b, session)
}

func measurePGUsageFixture(b *testing.B, store *Store, filter db.UsageFilter) (int, int64) {
	b.Helper()
	ctx := b.Context()
	messageWhere, args := pgUsageBenchmarkWindow("timestamp", filter)
	var messageRows int
	var tokenUsageBytes int64
	if err := store.DB().QueryRowContext(ctx,
		"SELECT count(*), COALESCE(sum(octet_length(token_usage)), 0) FROM messages"+messageWhere,
		args...,
	).Scan(&messageRows, &tokenUsageBytes); err != nil {
		b.Fatal(err)
	}
	eventWhere, eventArgs := pgUsageBenchmarkWindow("occurred_at", filter)
	var eventRows int
	if err := store.DB().QueryRowContext(ctx,
		"SELECT count(*) FROM usage_events"+eventWhere, eventArgs...,
	).Scan(&eventRows); err != nil {
		b.Fatal(err)
	}
	return messageRows + eventRows, tokenUsageBytes
}

func pgUsageBenchmarkWindow(column string, filter db.UsageFilter) (string, []any) {
	clauses := make([]string, 0, 2)
	args := make([]any, 0, 2)
	if filter.From != "" {
		args = append(args, filter.From)
		clauses = append(clauses, column+" >= $"+strconv.Itoa(len(args))+"::date")
	}
	if filter.To != "" {
		args = append(args, filter.To)
		clauses = append(clauses, column+" < ($"+strconv.Itoa(len(args))+"::date + INTERVAL '1 day')")
	}
	if len(clauses) == 0 {
		return "", nil
	}
	return " WHERE " + strings.Join(clauses, " AND "), args
}

func BenchmarkPGUsageRefresh(b *testing.B) {
	fixture := openPGUsageBenchmarkFixture(b)
	ctx := context.Background()
	b.Run("ColdPush", func(b *testing.B) {
		var sessionsPushed, messagesPushed int
		for i := 0; i < b.N; i++ {
			result, err := fixture.sync.Push(ctx, true, nil)
			if err != nil {
				b.Fatal(err)
			}
			sessionsPushed += result.SessionsPushed
			messagesPushed += result.MessagesPushed
		}
		require.NotZero(b, sessionsPushed)
		require.NotZero(b, messagesPushed)
		b.ReportMetric(float64(sessionsPushed)/float64(b.N), "sessions_pushed")
		b.ReportMetric(float64(messagesPushed)/float64(b.N), "messages_pushed")
	})
	b.Run("DeltaPush", func(b *testing.B) {
		var sessionsPushed, messagesPushed int
		for i := 0; i < b.N; i++ {
			session := usageParitySessionFixture("snapshot-winner", "project-b", "claude", "2026-08-12T10:01:00Z", 10+i+1, 20)
			if err := fixture.local.UpsertSession(session); err != nil {
				b.Fatal(err)
			}
			result, err := fixture.sync.Push(ctx, false, nil)
			if err != nil {
				b.Fatal(err)
			}
			sessionsPushed += result.SessionsPushed
			messagesPushed += result.MessagesPushed
		}
		require.NotZero(b, sessionsPushed)
		require.NotZero(b, messagesPushed)
		b.ReportMetric(float64(sessionsPushed)/float64(b.N), "sessions_pushed")
		b.ReportMetric(float64(messagesPushed)/float64(b.N), "messages_pushed")
	})
	b.Run("CatalogProbe", func(b *testing.B) {
		var tables int
		for i := 0; i < b.N; i++ {
			err := fixture.remote.DB().QueryRowContext(ctx, `
				SELECT count(*) FROM pg_catalog.pg_class c
				JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
				WHERE n.nspname = $1 AND c.relkind IN ('r', 'i')`, pgUsageBenchmarkSchema).Scan(&tables)
			if err != nil {
				b.Fatal(err)
			}
		}
		require.NotZero(b, tables)
		b.ReportMetric(float64(tables), "catalog_rows")
	})
}
