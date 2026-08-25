//go:build pgtest

package postgres

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/money"
)

const pgUsageBenchmarkSchemaPrefix = "agentsview_pg_usage_bench"

const (
	pgUsageBenchmarkBulkSessions           = 500
	pgUsageBenchmarkBulkMessagesPerSession = 200
	pgUsageBenchmarkBulkDateBuckets        = 4
)

var (
	pgUsageBenchmarkBulkProjects = []string{
		"agentsview", "quokka", "arrow-rs", "side-quests", "infrastructure",
		"blog", "experiments", "docs", "dotfiles", "playground",
	}
	pgUsageBenchmarkBulkAgents = []string{"claude", "codex", "openhands"}
	pgUsageBenchmarkBulkModels = []string{
		"model-priced", "model-reported", "model-bulk-cheap", "model-bulk-premium",
	}
)

type pgUsageBenchmarkCase struct {
	name                      string
	from, to                  string
	breakdowns                bool
	expectedEligibleInputRows int
}

func pgUsageBenchmarkCases() []pgUsageBenchmarkCase {
	return []pgUsageBenchmarkCase{
		{name: "one-day/no-breakdowns", from: "2026-08-12", to: "2026-08-12", expectedEligibleInputRows: 25_004},
		{name: "one-day/breakdowns", from: "2026-08-12", to: "2026-08-12", breakdowns: true, expectedEligibleInputRows: 25_004},
		{name: "seven-day/no-breakdowns", from: "2026-08-06", to: "2026-08-12", expectedEligibleInputRows: 50_004},
		{name: "seven-day/breakdowns", from: "2026-08-06", to: "2026-08-12", breakdowns: true, expectedEligibleInputRows: 50_004},
		{name: "thirty-day/no-breakdowns", from: "2026-07-14", to: "2026-08-12", expectedEligibleInputRows: 75_005},
		{name: "thirty-day/breakdowns", from: "2026-07-14", to: "2026-08-12", breakdowns: true, expectedEligibleInputRows: 75_005},
		{name: "all-history/no-breakdowns", expectedEligibleInputRows: 100_005},
		{name: "all-history/breakdowns", breakdowns: true, expectedEligibleInputRows: 100_005},
	}
}

func pgUsageBenchmarkFilter(tc pgUsageBenchmarkCase) db.UsageFilter {
	return db.UsageFilter{From: tc.from, To: tc.to, Timezone: "UTC", Breakdowns: tc.breakdowns}
}

func pgUsageBenchmarkValidationFilter() db.UsageFilter {
	return db.UsageFilter{From: "2026-08-12", To: "2026-08-12", Timezone: "UTC"}
}

type pgUsageBenchmarkFixture struct {
	pgURL            string
	schema           string
	local            *db.DB
	remote           *Store
	sync             *Sync
	expectedSessions int
	expectedMessages int
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
	t.Logf("pg_version=%s fixture_baseline_sessions=%d fixture_baseline_messages=%d fixture_usage_events=%d", version, 6, 5, 1)
	return &pgUsageBenchmarkFixture{
		pgURL: pgURL, schema: schema,
		local: local, remote: remote, sync: syncer,
		expectedSessions: 6, expectedMessages: 5,
	}
}

func (f *pgUsageBenchmarkFixture) addBulk(t testing.TB, count int) {
	t.Helper()
	seedPGUsageBenchmarkBulkFixture(t, f.local, count)
	f.expectedSessions += count
	f.expectedMessages += count * pgUsageBenchmarkBulkMessagesPerSession
}

func seedPGUsageBenchmarkBulkFixture(t testing.TB, local *db.DB, count int) {
	t.Helper()
	require.Greater(t, count, 0)
	require.Zero(t, count%pgUsageBenchmarkBulkDateBuckets, "bulk sessions must divide evenly across date buckets")
	sessionsPerDate := count / pgUsageBenchmarkBulkDateBuckets
	sessions := make([]db.Session, 0, count)
	messages := make([]db.Message, 0, count*pgUsageBenchmarkBulkMessagesPerSession)
	for i := 0; i < count; i++ {
		day := "2026-06-20"
		switch i / sessionsPerDate {
		case 0:
			day = "2026-08-12"
		case 1:
			day = "2026-08-08"
		case 2:
			day = "2026-07-20"
		}
		sessionID := "benchmark-bulk-" + strconv.Itoa(i)
		startedAt := day + "T09:00:00Z"
		startTime, err := time.Parse(time.RFC3339, startedAt)
		require.NoError(t, err)
		sessions = append(sessions, db.Session{
			ID: sessionID, Project: pgUsageBenchmarkBulkProjects[i%len(pgUsageBenchmarkBulkProjects)],
			Machine: "bench-machine", Agent: pgUsageBenchmarkBulkAgents[i%len(pgUsageBenchmarkBulkAgents)],
			StartedAt: &startedAt, MessageCount: pgUsageBenchmarkBulkMessagesPerSession,
			UserMessageCount: 2,
		})
		for ordinal := 0; ordinal < pgUsageBenchmarkBulkMessagesPerSession; ordinal++ {
			inputTokens := 1200 + (i % 17) + ordinal
			outputTokens := 480 + (i % 13) + (ordinal % 11)
			messages = append(messages, db.Message{
				SessionID: sessionID, Ordinal: ordinal, Role: "assistant",
				Timestamp: startTime.Add(time.Duration(ordinal) * time.Minute).Format(time.RFC3339),
				Model:     pgUsageBenchmarkBulkModels[(i+ordinal)%len(pgUsageBenchmarkBulkModels)],
				TokenUsage: json.RawMessage(`{"input_tokens":` + strconv.Itoa(inputTokens) +
					`,"output_tokens":` + strconv.Itoa(outputTokens) +
					`,"cache_creation_input_tokens":300,"cache_read_input_tokens":2400}`),
				ClaudeMessageID: "benchmark-bulk-message-" + strconv.Itoa(i) + "-" + strconv.Itoa(ordinal),
				ClaudeRequestID: "benchmark-bulk-request-" + strconv.Itoa(i) + "-" + strconv.Itoa(ordinal),
			})
		}
	}
	require.NoError(t, local.UpsertModelPricing([]db.ModelPricing{
		{ModelPattern: "model-bulk-cheap", InputPerMTok: money.Money{Microdollars: 500_000}, OutputPerMTok: money.Money{Microdollars: 1_000_000}},
		{ModelPattern: "model-bulk-premium", InputPerMTok: money.Money{Microdollars: 2_000_000}, OutputPerMTok: money.Money{Microdollars: 3_000_000}},
	}))
	for i := range sessions {
		require.NoError(t, local.UpsertSession(sessions[i]))
	}
	require.NoError(t, local.InsertMessages(messages))
}

func (f *pgUsageBenchmarkFixture) prime(t testing.TB) {
	t.Helper()
	result, err := f.sync.Push(t.Context(), true, nil)
	require.NoError(t, err)
	require.Equal(t, f.expectedSessions, result.SessionsPushed)
	require.Equal(t, f.expectedMessages, result.MessagesPushed)
	sessions, messages := pgUsageRemoteCardinality(t, f.remote)
	require.Equal(t, f.expectedSessions, sessions)
	require.Equal(t, f.expectedMessages, messages)
}

func (f *pgUsageBenchmarkFixture) analyzeUsageTables(t testing.TB) {
	t.Helper()
	_, err := f.remote.DB().ExecContext(t.Context(),
		"ANALYZE sessions, messages, usage_events, cursor_usage_events, model_pricing, model_pricing_bands")
	require.NoError(t, err)
	t.Logf("analyzed_usage_tables=sessions,messages,usage_events,cursor_usage_events,model_pricing,model_pricing_bands")
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
	require.Equal(t, f.expectedSessions, sessions)
	require.Equal(t, f.expectedMessages, messages)
}

func (f *pgUsageBenchmarkFixture) replaceDeltaMessage(t testing.TB, payload string) {
	f.replaceDeltaSessionMessage(t, "snapshot-winner", payload, 1)
}

func (f *pgUsageBenchmarkFixture) replaceDeltaBulkMessage(t testing.TB, payload string) {
	f.replaceDeltaSessionMessage(t, "benchmark-bulk-0", payload, pgUsageBenchmarkBulkMessagesPerSession)
}

func (f *pgUsageBenchmarkFixture) replaceDeltaSessionMessage(
	t testing.TB, sessionID, payload string, expectedCount int,
) {
	t.Helper()
	f.requireFixedCardinality(t)
	messages, err := f.local.GetMessages(t.Context(), sessionID, 0, expectedCount, true)
	require.NoError(t, err)
	require.Len(t, messages, expectedCount)
	messages[0].TokenUsage = []byte(payload)
	require.NoError(t, f.local.ReplaceSessionMessages(sessionID, messages))
	messageCount, err := f.local.MessageCount(sessionID)
	require.NoError(t, err)
	require.Equal(t, expectedCount, messageCount)
}

func TestPGUsageBenchmarkFixture(t *testing.T) {
	fixture := openPGUsageBenchmarkFixture(t)
	sessions, messages := pgUsageRemoteCardinality(t, fixture.remote)
	require.Zero(t, sessions)
	require.Zero(t, messages)
	fixture.prime(t)
	eligibleUsageInputRows, tokenUsageBytes := measurePGUsageFixture(t, fixture.remote, db.UsageFilter{
		From: "2026-08-12", To: "2026-08-12", Timezone: "UTC",
	})
	require.Equal(t, 4, eligibleUsageInputRows,
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

func TestPGUsageBenchmarkActivityOnlySessionCoverage(t *testing.T) {
	fixture := openPGUsageBenchmarkFixture(t)
	fixture.prime(t)
	filter := db.UsageFilter{Timezone: "UTC"}
	for _, backend := range []struct {
		name  string
		store db.Store
	}{
		{name: "sqlite", store: fixture.local},
		{name: "postgres", store: fixture.remote},
	} {
		t.Run(backend.name, func(t *testing.T) {
			counts, err := backend.store.GetUsageSessionCounts(t.Context(), filter)
			require.NoError(t, err)
			matching, err := backend.store.GetUsageMatchingSessionCount(t.Context(), filter)
			require.NoError(t, err)
			require.NotContains(t, counts.ByProject, "project-e")
			require.Equal(t, 4, counts.Total)
			require.Equal(t, 6, matching,
				"tokenless activity-only session matches without normal usage rows")
		})
	}
}

type pgUsageCountsOverride struct {
	db.Store
	counts db.UsageSessionCounts
}

func (s pgUsageCountsOverride) GetUsageSessionCounts(
	context.Context, db.UsageFilter,
) (db.UsageSessionCounts, error) {
	return s.counts, nil
}

func TestPGUsageBenchmarkReadPreflightRejectsMismatchedCompleteResult(t *testing.T) {
	fixture := openPGUsageBenchmarkFixture(t)
	fixture.prime(t)
	filter := pgUsageBenchmarkFilter(pgUsageBenchmarkCases()[0])
	counts, err := fixture.remote.GetUsageSessionCounts(t.Context(), filter)
	require.NoError(t, err)
	counts.Total++
	remote := pgUsageCountsOverride{Store: fixture.remote, counts: counts}
	require.Error(t, completeUsageParity(t.Context(), fixture.local, remote, filter),
		"complete preflight must reject a non-empty PostgreSQL count mismatch")

	recorded := &pgUsageMethodRecorder{Store: fixture.remote}
	requireCompleteUsageParity(t, fixture.local, recorded, filter)
	require.ElementsMatch(t, []string{
		"GetDailyUsage", "GetTopSessionsByCost", "GetUsageSessionCounts",
		"GetUsageMatchingSessionCount", "GetSessionUsage",
	}, recorded.calls)
}

type pgUsageMethodRecorder struct {
	db.Store
	calls []string
}

func (s *pgUsageMethodRecorder) GetDailyUsage(
	ctx context.Context, filter db.UsageFilter,
) (db.DailyUsageResult, error) {
	s.calls = append(s.calls, "GetDailyUsage")
	return s.Store.GetDailyUsage(ctx, filter)
}

func (s *pgUsageMethodRecorder) GetTopSessionsByCost(
	ctx context.Context, filter db.UsageFilter, limit int,
) ([]db.TopSessionEntry, error) {
	s.calls = append(s.calls, "GetTopSessionsByCost")
	return s.Store.GetTopSessionsByCost(ctx, filter, limit)
}

func (s *pgUsageMethodRecorder) GetUsageSessionCounts(
	ctx context.Context, filter db.UsageFilter,
) (db.UsageSessionCounts, error) {
	s.calls = append(s.calls, "GetUsageSessionCounts")
	return s.Store.GetUsageSessionCounts(ctx, filter)
}

func (s *pgUsageMethodRecorder) GetUsageMatchingSessionCount(
	ctx context.Context, filter db.UsageFilter,
) (int, error) {
	s.calls = append(s.calls, "GetUsageMatchingSessionCount")
	return s.Store.GetUsageMatchingSessionCount(ctx, filter)
}

func (s *pgUsageMethodRecorder) GetSessionUsage(
	ctx context.Context, sessionID string, includeBreakdown bool,
) (*db.SessionUsage, error) {
	s.calls = append(s.calls, "GetSessionUsage")
	return s.Store.GetSessionUsage(ctx, sessionID, includeBreakdown)
}

func BenchmarkPGUsageRead(b *testing.B) {
	fixture := openPGUsageBenchmarkFixture(b)
	fixture.addBulk(b, pgUsageBenchmarkBulkSessions)
	fixture.prime(b)
	fixture.analyzeUsageTables(b)
	loadedSessions, loadedMessages := pgUsageRemoteCardinality(b, fixture.remote)
	require.Equal(b, 6+pgUsageBenchmarkBulkSessions, loadedSessions)
	require.Equal(b, 5+pgUsageBenchmarkBulkSessions*pgUsageBenchmarkBulkMessagesPerSession, loadedMessages)
	ctx := context.Background()
	for _, tc := range pgUsageBenchmarkCases() {
		b.Run(tc.name, func(b *testing.B) {
			filter := pgUsageBenchmarkFilter(tc)
			b.Logf("fixture_scale=bulk_sessions=%d bulk_messages=%d messages_per_session=%d projects=%d agents=%d models=%d dates=%d loaded_table_rows=sessions:%d messages:%d",
				pgUsageBenchmarkBulkSessions,
				pgUsageBenchmarkBulkSessions*pgUsageBenchmarkBulkMessagesPerSession,
				pgUsageBenchmarkBulkMessagesPerSession,
				len(pgUsageBenchmarkBulkProjects), len(pgUsageBenchmarkBulkAgents),
				len(pgUsageBenchmarkBulkModels), pgUsageBenchmarkBulkDateBuckets,
				loadedSessions, loadedMessages)
			eligibleUsageInputRows, tokenUsageBytes := measurePGUsageFixture(b, fixture.remote, filter)
			require.Equal(b, tc.expectedEligibleInputRows, eligibleUsageInputRows)
			requireCompleteUsageParity(b, fixture.local, fixture.remote, filter)
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
					ctx, "benchmark-bulk-0", tc.breakdowns)
				if err != nil {
					b.Fatal(err)
				}
				if len(daily.Daily) == 0 || len(top) == 0 || counts.Total == 0 || matching == 0 || session == nil {
					b.Fatal("usage benchmark returned an empty result")
				}
			}
			b.ReportMetric(float64(eligibleUsageInputRows), "eligible_usage_input_rows")
			b.ReportMetric(float64(tokenUsageBytes), "bytes_token_usage")
		})
	}
}

func measurePGUsageFixture(
	t testing.TB, store *Store, filter db.UsageFilter,
) (int, int64) {
	t.Helper()
	// This counts eligible fixture inputs outside timing, not PostgreSQL execution-plan rows.
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
			require.Equal(b, 6, result.SessionsPushed)
			require.Equal(b, 5, result.MessagesPushed)
			sessions, messages := pgUsageRemoteCardinality(b, fixture.remote)
			require.Equal(b, 6, sessions)
			require.Equal(b, 5, messages)
			requireCompleteUsageParity(b, fixture.local, fixture.remote,
				pgUsageBenchmarkValidationFilter())
			sessionsPushed += result.SessionsPushed
			messagesPushed += result.MessagesPushed
			iterations++
			b.StartTimer()
		}
		b.StopTimer()
		require.Equal(b, 6*iterations, sessionsPushed)
		require.Equal(b, 5*iterations, messagesPushed)
		b.ReportMetric(6, "sessions_pushed")
		b.ReportMetric(5, "messages_pushed")
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
			requireCompleteUsageParity(b, fixture.local, fixture.remote,
				pgUsageBenchmarkValidationFilter())
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

func BenchmarkPGUsageRefreshLarge(b *testing.B) {
	ctx := context.Background()
	b.Run("ColdPush", func(b *testing.B) {
		fixture := openPGUsageBenchmarkFixture(b)
		fixture.addBulk(b, pgUsageBenchmarkBulkSessions)
		b.Logf("fixture_scale=bulk_sessions=%d bulk_messages=%d messages_per_session=%d configured_table_rows=sessions:%d messages:%d",
			pgUsageBenchmarkBulkSessions,
			pgUsageBenchmarkBulkSessions*pgUsageBenchmarkBulkMessagesPerSession,
			pgUsageBenchmarkBulkMessagesPerSession,
			fixture.expectedSessions,
			fixture.expectedMessages)
		for b.Loop() {
			b.StopTimer()
			fixture.resetRemoteEmpty(b)
			b.StartTimer()
			result, err := fixture.sync.Push(ctx, true, nil)
			b.StopTimer()
			require.NoError(b, err)
			require.Equal(b, fixture.expectedSessions, result.SessionsPushed)
			require.Equal(b, fixture.expectedMessages, result.MessagesPushed)
			sessions, messages := pgUsageRemoteCardinality(b, fixture.remote)
			require.Equal(b, fixture.expectedSessions, sessions)
			require.Equal(b, fixture.expectedMessages, messages)
			b.Logf("loaded_table_rows=sessions:%d messages:%d", sessions, messages)
			requireCompleteUsageParity(b, fixture.local, fixture.remote,
				pgUsageBenchmarkValidationFilter())
			b.StartTimer()
		}
		b.ReportMetric(float64(fixture.expectedSessions), "sessions_pushed")
		b.ReportMetric(float64(fixture.expectedMessages), "messages_pushed")
	})

	b.Run("DeltaPush", func(b *testing.B) {
		fixture := openPGUsageBenchmarkFixture(b)
		fixture.addBulk(b, pgUsageBenchmarkBulkSessions)
		fixture.prime(b)
		fixture.analyzeUsageTables(b)
		loadedSessions, loadedMessages := pgUsageRemoteCardinality(b, fixture.remote)
		b.Logf("fixture_scale=bulk_sessions=%d bulk_messages=%d messages_per_session=%d loaded_table_rows=sessions:%d messages:%d",
			pgUsageBenchmarkBulkSessions,
			pgUsageBenchmarkBulkSessions*pgUsageBenchmarkBulkMessagesPerSession,
			pgUsageBenchmarkBulkMessagesPerSession,
			loadedSessions,
			loadedMessages)
		iterations := 0
		for b.Loop() {
			b.StopTimer()
			payload := `{"input_tokens":2000,"output_tokens":800}`
			if iterations%2 == 1 {
				payload = `{"input_tokens":2001,"output_tokens":801}`
			}
			fixture.replaceDeltaBulkMessage(b, payload)
			b.StartTimer()
			result, err := fixture.sync.Push(ctx, false, nil)
			b.StopTimer()
			require.NoError(b, err)
			require.Equal(b, 1, result.SessionsPushed)
			require.Equal(b, pgUsageBenchmarkBulkMessagesPerSession, result.MessagesPushed)
			fixture.requireFixedCardinality(b)
			requireCompleteUsageParity(b, fixture.local, fixture.remote,
				pgUsageBenchmarkValidationFilter())
			iterations++
			b.StartTimer()
		}
		b.ReportMetric(1, "sessions_pushed")
		b.ReportMetric(pgUsageBenchmarkBulkMessagesPerSession, "messages_pushed")
	})
}
