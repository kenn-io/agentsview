//go:build pgtest

package postgres

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
)

const pgUsageBenchmarkSchemaPrefix = "agentsview_pg_usage_bench"

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
	pgURL  string
	schema string
	local  *db.DB
	remote *Store
	sync   *Sync
	filter db.UsageFilter
}

func pgUsageRemoteCardinality(t testing.TB, remote *Store) (int, int) {
	t.Helper()
	var sessions, messages int
	err := remote.DB().QueryRowContext(
		t.Context(), "SELECT (SELECT count(*) FROM sessions), (SELECT count(*) FROM messages)",
	).Scan(&sessions, &messages)
	require.NoError(t, err)
	return sessions, messages
}

func pgUsageBenchmarkSchema(t testing.TB) string {
	t.Helper()
	hash := sha256.Sum256([]byte(t.Name() + "\x00" + t.TempDir()))
	return pgUsageBenchmarkSchemaPrefix + "_" + hex.EncodeToString(hash[:8])
}

func openPGUsageBenchmarkFixture(t testing.TB) *pgUsageBenchmarkFixture {
	t.Helper()
	req := require.New(t)
	pgURL := testPGURL(t)
	schema := pgUsageBenchmarkSchema(t)
	admin, err := sql.Open("pgx", pgURL)
	req.NoError(err)
	_, err = admin.Exec("DROP SCHEMA IF EXISTS " + schema + " CASCADE")
	req.NoError(err)
	req.NoError(admin.Close())

	local, err := db.Open(t.TempDir() + "/usage-bench.db")
	req.NoError(err)
	seedUsageParityFixture(t, local)
	syncer, err := New(pgURL, schema, local, "bench-machine", true, SyncOptions{})
	req.NoError(err)
	req.NoError(EnsureSchema(t.Context(), syncer.pg, schema))
	remote, err := NewStore(pgURL, schema, true)
	req.NoError(err)
	t.Cleanup(func() {
		_ = remote.Close()
		_ = syncer.Close()
		_ = local.Close()
		admin, _ := sql.Open("pgx", pgURL)
		_, _ = admin.Exec("DROP SCHEMA IF EXISTS " + schema + " CASCADE")
		_ = admin.Close()
	})

	var version string
	req.NoError(remote.DB().QueryRowContext(t.Context(), "SHOW server_version").Scan(&version))
	t.Logf("pg_version=%s fixture_sessions=%d fixture_messages=%d fixture_usage_events=%d", version, 5, 4, 1)
	return &pgUsageBenchmarkFixture{
		pgURL: pgURL, schema: schema,
		local: local, remote: remote, sync: syncer,
		filter: db.UsageFilter{From: "2026-08-12", To: "2026-08-12", Timezone: "UTC"},
	}
}

func (f *pgUsageBenchmarkFixture) prime(t testing.TB) {
	t.Helper()
	result, err := f.sync.Push(t.Context(), true, nil)
	require.NoError(t, err)
	require.Equal(t, 5, result.SessionsPushed)
	require.Equal(t, 4, result.MessagesPushed)
	sessions, messages := pgUsageRemoteCardinality(t, f.remote)
	require.Equal(t, 5, sessions)
	require.Equal(t, 4, messages)
}

func (f *pgUsageBenchmarkFixture) resetRemoteEmpty(t testing.TB) {
	t.Helper()
	cleanNamedPGSchema(t, f.pgURL, f.schema)
	require.NoError(t, EnsureSchema(t.Context(), f.sync.pg, f.schema))
	sessions, messages := pgUsageRemoteCardinality(t, f.remote)
	require.Zero(t, sessions)
	require.Zero(t, messages)
}

func (f *pgUsageBenchmarkFixture) requireFixedCardinality(t testing.TB) {
	t.Helper()
	messageCount, err := f.local.MessageCount("snapshot-winner")
	require.NoError(t, err)
	require.Equal(t, 1, messageCount)
	sessions, messages := pgUsageRemoteCardinality(t, f.remote)
	require.Equal(t, 5, sessions)
	require.Equal(t, 4, messages)
}

func (f *pgUsageBenchmarkFixture) replaceDeltaMessage(t testing.TB, payload string) {
	t.Helper()
	f.requireFixedCardinality(t)
	messages, err := f.local.GetMessages(t.Context(), "snapshot-winner", 0, 1, true)
	require.NoError(t, err)
	require.Len(t, messages, 1)
	messages[0].TokenUsage = []byte(payload)
	require.NoError(t, f.local.ReplaceSessionMessages("snapshot-winner", messages))
	messageCount, err := f.local.MessageCount("snapshot-winner")
	require.NoError(t, err)
	require.Equal(t, 1, messageCount)
}

func TestPGUsageBenchmarkFixture(t *testing.T) {
	pgURL := testPGURL(t)
	const schema = "agentsview_usage_benchmark_fixture_test"
	cleanNamedPGSchema(t, pgURL, schema)
	t.Cleanup(func() { cleanNamedPGSchema(t, pgURL, schema) })
	fixture := openPGUsageBenchmarkFixture(t)
	sessions, messages := pgUsageRemoteCardinality(t, fixture.remote)
	require.Zero(t, sessions)
	require.Zero(t, messages)
	fixture.prime(t)
	rowsScanned, tokenUsageBytes := measurePGUsageFixture(t, fixture.remote, db.UsageFilter{
		From: "2026-08-12", To: "2026-08-12", Timezone: "UTC",
	})
	require.Equal(t, 4, rowsScanned,
		"daily usage metrics include the blank-timestamp message via session start")
	require.Equal(t, int64(len(`{"input_tokens":10,"output_tokens":5}`))+
		int64(len(`{"input_tokens":20,"output_tokens":10,"server_tool_use":{"web_search_requests":1}}`))+
		int64(len(`{"input_tokens":7,"output_tokens":3}`)), tokenUsageBytes,
		"token bytes cover every token-eligible daily message")

	for _, tc := range pgUsageBenchmarkCases() {
		t.Run(tc.name, func(t *testing.T) {
			filter := pgUsageBenchmarkFilter(tc)
			localResult := captureUsageParitySnapshot(t, fixture.local, filter)
			remoteResult := captureUsageParitySnapshot(t, fixture.remote, filter)
			require.NotEmpty(t, remoteResult.Top)
			require.NotEmpty(t, remoteResult.Daily.Dates)
			require.Equal(t, localResult, remoteResult)
			require.NotZero(t, remoteResult.Counts.Total)
			require.NotZero(t, remoteResult.MatchingSessionCount)
			usage, err := fixture.remote.GetSessionUsage(
				t.Context(), "snapshot-winner", tc.breakdowns)
			require.NoError(t, err)
			if tc.breakdowns {
				require.NotZero(t, len(usage.Breakdown))
			} else {
				require.Zero(t, len(usage.Breakdown))
			}
		})
	}
}

func BenchmarkPGUsageRead(b *testing.B) {
	fixture := openPGUsageBenchmarkFixture(b)
	fixture.prime(b)
	ctx := context.Background()
	for _, tc := range pgUsageBenchmarkCases() {
		b.Run(tc.name, func(b *testing.B) {
			filter := pgUsageBenchmarkFilter(tc)
			rowsScanned, tokenUsageBytes := measurePGUsageFixture(b, fixture.remote, filter)
			validatePGUsageBenchmarkRead(b, fixture.remote, ctx, filter, tc.breakdowns)
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
				session, err := fixture.remote.GetSessionUsage(
					ctx, "snapshot-winner", tc.breakdowns)
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

func validatePGUsageBenchmarkRead(
	b *testing.B, store *Store, ctx context.Context, filter db.UsageFilter,
	breakdowns bool,
) {
	b.Helper()
	daily, err := store.GetDailyUsage(ctx, filter)
	require.NoError(b, err)
	top, err := store.GetTopSessionsByCost(ctx, filter, 10)
	require.NoError(b, err)
	counts, err := store.GetUsageSessionCounts(ctx, filter)
	require.NoError(b, err)
	matching, err := store.GetUsageMatchingSessionCount(ctx, filter)
	require.NoError(b, err)
	session, err := store.GetSessionUsage(ctx, "snapshot-winner", breakdowns)
	require.NoError(b, err)
	require.NotEmpty(b, daily.Daily)
	require.NotEmpty(b, top)
	require.NotZero(b, counts.Total)
	require.NotZero(b, matching)
	require.NotNil(b, session)
	if breakdowns {
		require.NotEmpty(b, session.Breakdown)
	} else {
		require.Empty(b, session.Breakdown)
	}
}

func measurePGUsageFixture(
	t testing.TB, store *Store, filter db.UsageFilter,
) (int, int64) {
	t.Helper()
	ctx := t.Context()
	messageWhere, args := pgUsageBenchmarkWindow(
		"COALESCE(m.timestamp, s.started_at)", filter)
	messageWindow := strings.TrimPrefix(messageWhere, " WHERE ")
	if messageWindow == "" {
		messageWindow = "TRUE"
	}
	var messageRows int
	var tokenUsageBytes int64
	if err := store.DB().QueryRowContext(ctx,
		`SELECT count(*), COALESCE(sum(octet_length(m.token_usage)), 0)
		 FROM messages m
		 JOIN sessions s ON s.id = m.session_id
		 WHERE m.token_usage != ''
		   AND m.model != ''
		   AND m.model != '<synthetic>'
		   AND s.deleted_at IS NULL
		   AND COALESCE(m.timestamp, s.started_at) IS NOT NULL
		   AND `+messageWindow,
		args...,
	).Scan(&messageRows, &tokenUsageBytes); err != nil {
		t.Fatal(err)
	}
	eventWhere, eventArgs := pgUsageBenchmarkWindow(
		"COALESCE(ue.occurred_at, s.started_at)", filter)
	eventWindow := strings.TrimPrefix(eventWhere, " WHERE ")
	if eventWindow == "" {
		eventWindow = "TRUE"
	}
	var eventRows int
	if err := store.DB().QueryRowContext(ctx,
		`SELECT count(*)
		 FROM usage_events ue
		 JOIN sessions s ON s.id = ue.session_id
		 WHERE ue.model != ''
		   AND s.deleted_at IS NULL
		   AND COALESCE(ue.occurred_at, s.started_at) IS NOT NULL
		   AND `+eventWindow, eventArgs...,
	).Scan(&eventRows); err != nil {
		t.Fatal(err)
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
	ctx := context.Background()
	b.Run("ColdPush", func(b *testing.B) {
		fixture := openPGUsageBenchmarkFixture(b)
		var sessionsPushed, messagesPushed int
		iterations := 0
		for b.Loop() {
			b.StopTimer()
			fixture.resetRemoteEmpty(b)
			b.StartTimer()
			result, err := fixture.sync.Push(ctx, true, nil)
			b.StopTimer()
			if err != nil {
				b.Fatal(err)
			}
			require.Equal(b, 5, result.SessionsPushed)
			require.Equal(b, 4, result.MessagesPushed)
			sessions, messages := pgUsageRemoteCardinality(b, fixture.remote)
			require.Equal(b, 5, sessions)
			require.Equal(b, 4, messages)
			sessionsPushed += result.SessionsPushed
			messagesPushed += result.MessagesPushed
			iterations++
			b.StartTimer()
		}
		b.StopTimer()
		require.Equal(b, 5*iterations, sessionsPushed)
		require.Equal(b, 4*iterations, messagesPushed)
		b.ReportMetric(5, "sessions_pushed")
		b.ReportMetric(4, "messages_pushed")
	})
	b.Run("DeltaPush", func(b *testing.B) {
		fixture := openPGUsageBenchmarkFixture(b)
		fixture.prime(b)
		var sessionsPushed, messagesPushed int
		iterations := 0
		for b.Loop() {
			b.StopTimer()
			payload := `{"input_tokens":20,"output_tokens":10}`
			if iterations%2 == 1 {
				payload = `{"input_tokens":21,"output_tokens":10}`
			}
			fixture.replaceDeltaMessage(b, payload)
			b.StartTimer()
			result, err := fixture.sync.Push(ctx, false, nil)
			b.StopTimer()
			if err != nil {
				b.Fatal(err)
			}
			require.Equal(b, 1, result.SessionsPushed)
			require.Equal(b, 1, result.MessagesPushed)
			fixture.requireFixedCardinality(b)
			sessionsPushed += result.SessionsPushed
			messagesPushed += result.MessagesPushed
			iterations++
			b.StartTimer()
		}
		b.StopTimer()
		require.Equal(b, iterations, sessionsPushed)
		require.Equal(b, iterations, messagesPushed)
		b.ReportMetric(1, "sessions_pushed")
		b.ReportMetric(1, "messages_pushed")
	})
	b.Run("CatalogProbe", func(b *testing.B) {
		fixture := openPGUsageBenchmarkFixture(b)
		var tables int
		for b.Loop() {
			err := fixture.remote.DB().QueryRowContext(ctx, `
				SELECT count(*) FROM pg_catalog.pg_class c
				JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
				WHERE n.nspname = $1 AND c.relkind IN ('r', 'i')`, fixture.schema).Scan(&tables)
			if err != nil {
				b.Fatal(err)
			}
		}
		require.NotZero(b, tables)
		b.ReportMetric(float64(tables), "catalog_rows")
	})
}
