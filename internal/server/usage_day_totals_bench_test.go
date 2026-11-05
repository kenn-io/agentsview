//go:build fts5

package server

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/money"
	"go.kenn.io/agentsview/internal/service"
)

func seedUsagePageJournalBenchmark(tb testing.TB, count, dailyRows int) (*db.DB, db.UsageFilter, db.DailyUsageResult) {
	tb.Helper()
	path := filepath.Join(tb.TempDir(), "test.db")
	database, err := db.OpenIsolated(tb.Context(), path)
	require.NoError(tb, err)
	tb.Cleanup(func() { require.NoError(tb, database.Close()) })
	// Project keys include archive identity, so separate runs need the same scope.
	require.NoError(tb, database.SetArchiveIdentityForTest(tb.Context(), "usage-page-fixture", "0000000000000000000000000000000000000000000000000000000000000001"))

	require.NoError(tb, database.UpsertModelPricing([]db.ModelPricing{{ModelPattern: "journal-model", InputPerMTok: money.Money{Microdollars: 1_000_000}, OutputPerMTok: money.Money{Microdollars: 2_000_000}}, {ModelPattern: "journal-other", InputPerMTok: money.Money{Microdollars: 1_000_000}, OutputPerMTok: money.Money{Microdollars: 2_000_000}}}))
	// Stable catalog timestamps make separate base/head runs byte-comparable.
	conn, err := sql.Open("sqlite3", path)
	require.NoError(tb, err)
	_, err = conn.ExecContext(tb.Context(), `UPDATE model_pricing SET updated_at='2026-08-01T00:00:00Z' WHERE model_pattern IN ('journal-model', 'journal-other')`)
	require.NoError(tb, err)
	require.NoError(tb, conn.Close())

	for start := 0; start < count; start += 256 {
		var writes []db.SessionBatchWrite
		for i := start; i < min(start+256, count); i++ {
			id := fmt.Sprintf("journal-%05d", i)
			// Keep 40% in the last seven days and spread the rest over
			// the preceding months, including the comparison window.
			day := time.Date(2026, 8, 4+i%5, 12, 0, 0, 0, time.UTC)
			if i%10 >= 4 {
				day = time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC).AddDate(0, 0, -4-i/10%80)
			}
			started := day.Format(time.RFC3339)
			write := db.SessionBatchWrite{Session: db.Session{ID: id, Project: fmt.Sprintf("project-%d", i%4), Machine: fmt.Sprintf("machine-%d", i/4%2), Agent: []string{"codex", "claude"}[i/8%2], GitBranch: fmt.Sprintf("branch-%d", i%2), StartedAt: &started, UserMessageCount: 1 + i%3, IsAutomated: i%7 == 0}}
			for j := range 3 {
				offset := 0
				if dailyRows == 3 {
					offset = j
				} else if i%33 == 0 && j == 2 {
					offset = 1
				}
				write.Messages = append(write.Messages, db.Message{SessionID: id, Ordinal: j, Role: "assistant", Timestamp: day.AddDate(0, 0, offset).Add(time.Duration(j) * time.Minute).Format(time.RFC3339), Model: []string{"journal-model", "journal-other"}[i/16%2], TokenUsage: []byte(`{"input_tokens":100,"output_tokens":20}`)})
			}
			writes = append(writes, write)
		}
		_, err := database.WriteSessionBatchAtomic(tb.Context(), writes)
		require.NoError(tb, err)
	}
	filter := db.UsageFilter{From: "2026-08-04", To: "2026-08-10", Timezone: "UTC", SkipSessionCounts: true}
	_, err = database.GetDailyUsage(tb.Context(), filter)
	require.NoError(tb, err)
	require.NoError(tb, database.StartUsageCacheBackfill(tb.Context()))
	require.NoError(tb, database.WaitUsageCacheBackfill(tb.Context()))
	want, err := database.GetDailyUsage(tb.Context(), filter)
	require.NoError(tb, err)
	recent := count/10*4 + min(count%10, 4)
	require.Equal(tb, recent*300, want.Totals.InputTokens)
	require.Equal(tb, recent*60, want.Totals.OutputTokens)
	require.Equal(tb, int64(recent*420), want.Totals.TotalCost.Microdollars)
	return database, filter, want
}

func seedUsageDayTotalsPage(tb testing.TB, count int) *db.DB {
	tb.Helper()
	database, _, _ := seedUsagePageJournalBenchmark(tb, count, 1)
	// A prior-period row makes Comparison exercise real data too.
	started := "2026-08-03T23:30:00Z"
	_, err := database.WriteSessionBatchAtomic(tb.Context(), []db.SessionBatchWrite{
		{Session: db.Session{ID: "reported", Project: "project-0", Machine: "machine-0", Agent: "copilot", StartedAt: &started, UserMessageCount: 2}, Messages: []db.Message{
			{SessionID: "reported", Ordinal: 0, Role: "assistant", Timestamp: started, Model: "journal-model", TokenUsage: []byte(`{"input_tokens":100,"output_tokens":20}`)},
			{SessionID: "reported", Ordinal: 1, Role: "assistant", Timestamp: "2026-08-04T00:30:00Z", Model: "journal-model", TokenUsage: []byte(`{"input_tokens":200,"output_tokens":30}`)},
		}},
		{Session: db.Session{ID: "duplicate-a", Project: "project-0", Agent: "claude", StartedAt: &started, UserMessageCount: 1}, Messages: []db.Message{{SessionID: "duplicate-a", Ordinal: 0, Role: "assistant", Timestamp: started, Model: "journal-model", TokenUsage: []byte(`{"output_tokens":10}`), ClaudeMessageID: "shared", ClaudeRequestID: "request"}}},
		{Session: db.Session{ID: "duplicate-b", Project: "project-1", Agent: "claude", StartedAt: &started, UserMessageCount: 2}, Messages: []db.Message{{SessionID: "duplicate-b", Ordinal: 0, Role: "assistant", Timestamp: started, Model: "journal-model", TokenUsage: []byte(`{"output_tokens":20}`), ClaudeMessageID: "shared", ClaudeRequestID: "request"}}},
	})
	require.NoError(tb, err)
	cost := money.Money{Microdollars: 1234567}
	require.NoError(tb, database.ReplaceSessionUsageEvents(tb.Context(), "reported", []db.UsageEvent{{Source: "shutdown", Model: "journal-model", InputTokens: 10, Cost: &cost, CostStatus: "exact", CostSource: db.CopilotReportedCostSource, OccurredAt: "2026-08-04T01:00:00Z", DedupKey: "reported"}}))
	require.NoError(tb, database.StartUsageCacheBackfill(tb.Context()))
	require.NoError(tb, database.WaitUsageCacheBackfill(tb.Context()))
	database.StopUsageCacheBackfill()
	return database
}

// The browser starts these three requests together. Pairwise owns its ranking
// and default side reads; Comparison owns the prior-period range.
func readUsageDayTotalsPage(ctx context.Context, s *Server, all bool) ([3][]byte, error) {
	input := UsageFilterInput{From: "2026-08-04", To: "2026-08-10", Timezone: "UTC", IncludeOneShot: true, Breakdowns: true, SessionCounts: true}
	if all {
		// Cover the fixture's whole history with the concrete dates the page sends.
		input.From = "2026-05-01"
	}
	var results [3][]byte
	var errs [3]error
	var wg sync.WaitGroup
	wg.Go(func() {
		out, err := s.humaUsageSummary(ctx, &UsageSummaryInput{UsageFilterInput: input})
		if err != nil {
			errs[0] = err
			return
		}
		results[0], errs[0] = json.Marshal(out.Body)
	})
	wg.Go(func() {
		out, err := s.humaUsagePairwiseComparison(ctx, &usagePairwiseComparisonInput{UsageSummaryInput: UsageSummaryInput{UsageFilterInput: input}})
		if err != nil {
			errs[1] = err
			return
		}
		results[1], errs[1] = json.Marshal(out.Body)
	})
	wg.Go(func() {
		out, err := s.humaUsageComparison(ctx, &usageComparisonInput{UsageFilterInput: input, CurrentMicrodollars: 0})
		if err != nil {
			errs[2] = err
			return
		}
		results[2], errs[2] = json.Marshal(out.Body)
	})
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			return results, err
		}
	}
	return results, nil
}

func checkUsageDayTotalsPage(tb testing.TB, name string, want, got [3][]byte) {
	tb.Helper()
	for i := range got {
		require.Equal(tb, string(want[i]), string(got[i]))
		file := fmt.Sprintf("%s-%d.json", name, i)
		if dir := os.Getenv("AGENTSVIEW_USAGE_BASE_RESULTS_DIR"); dir != "" {
			base, err := os.ReadFile(filepath.Join(dir, file))
			require.NoError(tb, err)
			require.Equal(tb, string(base), string(got[i]), "base/head %s", file)
		}
		if dir := os.Getenv("AGENTSVIEW_USAGE_RESULTS_DIR"); dir != "" {
			require.NoError(tb, os.WriteFile(filepath.Join(dir, file), got[i], 0600))
		}
		tb.Logf("%s response-%d sha256=%x", name, i, sha256.Sum256(got[i]))
	}
}

func BenchmarkUsageDayTotalsPage(b *testing.B) {
	count := 300
	if raw := os.Getenv("AGENTSVIEW_BENCH_SESSIONS"); raw != "" {
		var err error
		count, err = strconv.Atoi(raw)
		require.NoError(b, err)
		require.Positive(b, count)
	}
	database := seedUsageDayTotalsPage(b, count)
	s := &Server{db: database, sessions: service.NewReadOnlyBackend(database)}
	for _, all := range []bool{false, true} {
		name := "7-days"
		if all {
			name = "all-history"
		}
		b.Run(name, func(b *testing.B) {
			want, err := readUsageDayTotalsPage(b.Context(), s, all)
			require.NoError(b, err)
			runtime.GC()
			b.ResetTimer()
			for range b.N {
				got, err := readUsageDayTotalsPage(b.Context(), s, all)
				b.StopTimer()
				require.NoError(b, err)
				checkUsageDayTotalsPage(b, name, want, got)
				b.StartTimer()
			}
		})
	}
}

func TestUsageDayTotalsPageBudget(t *testing.T) {
	if os.Getenv("AGENTSVIEW_USAGE_PAGE_GATE") == "" {
		t.Skip("set AGENTSVIEW_USAGE_PAGE_GATE for the 30k-session timing gate")
	}
	database := seedUsageDayTotalsPage(t, 30000)
	s := &Server{db: database, sessions: service.NewReadOnlyBackend(database)}
	for _, all := range []bool{false, true} {
		name := "7-days"
		if all {
			name = "all-history"
		}
		t.Run(name, func(t *testing.T) {
			want, err := readUsageDayTotalsPage(t.Context(), s, all)
			require.NoError(t, err)
			runtime.GC()
			start := time.Now()
			got, err := readUsageDayTotalsPage(t.Context(), s, all)
			elapsed := time.Since(start)
			require.NoError(t, err)
			t.Logf("warm page=%s", elapsed)
			assert.Less(t, elapsed, time.Second/2)
			checkUsageDayTotalsPage(t, name, want, got)
		})
	}
}
