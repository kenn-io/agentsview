//go:build fts5

package usagefixture

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/money"
)

// SeedJournal creates and warms the archive used by journal and page benchmarks.
func SeedJournal(tb testing.TB, count, dailyRows int) (*db.DB, db.UsageFilter, db.DailyUsageResult) {
	tb.Helper()
	path := filepath.Join(tb.TempDir(), "test.db")
	database, err := db.OpenIsolated(tb.Context(), path)
	require.NoError(tb, err)
	tb.Cleanup(func() { require.NoError(tb, database.Close()) })
	// Project keys include archive identity, so separate runs need the same scope.
	require.NoError(tb, database.SetArchiveIdentityForTest(tb.Context(), ArchiveID, ArchiveKey))

	var prices []db.ModelPricing
	for _, model := range Models {
		prices = append(prices, db.ModelPricing{ModelPattern: model, InputPerMTok: money.Money{Microdollars: 1_000_000}, OutputPerMTok: money.Money{Microdollars: 2_000_000}})
	}
	require.NoError(tb, database.UpsertModelPricing(prices))
	// Stable catalog timestamps make separate base/head runs byte-comparable.
	conn, err := sql.Open("sqlite3", path)
	require.NoError(tb, err)
	_, err = conn.ExecContext(tb.Context(), `UPDATE model_pricing SET updated_at=? WHERE model_pattern IN ('journal-model', 'journal-other')`, PricingTimestamp)
	require.NoError(tb, err)
	require.NoError(tb, conn.Close())

	for start := 0; start < count; start += 256 {
		var writes []db.SessionBatchWrite
		for i := start; i < min(start+256, count); i++ {
			writes = append(writes, sessionWrite(Journal(i, dailyRows)))
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

// SeedPage adds authoritative-cost and shared-identity sessions to the journal fixture.
func SeedPage(tb testing.TB, count int) *db.DB {
	tb.Helper()
	database, _, _ := SeedJournal(tb, count, 1)
	var writes []db.SessionBatchWrite
	for _, input := range PageExceptions() {
		writes = append(writes, sessionWrite(input))
	}
	_, err := database.WriteSessionBatchAtomic(tb.Context(), writes)
	require.NoError(tb, err)
	cost := money.Money{Microdollars: ReportedCostMicrodollars}
	require.NoError(tb, database.ReplaceSessionUsageEvents(tb.Context(), "reported", []db.UsageEvent{{Source: "shutdown", Model: "journal-model", InputTokens: ReportedInputTokens, Cost: &cost, CostStatus: "exact", CostSource: db.CopilotReportedCostSource, OccurredAt: ReportedTimestamp, DedupKey: "reported"}}))
	require.NoError(tb, database.StartUsageCacheBackfill(tb.Context()))
	require.NoError(tb, database.WaitUsageCacheBackfill(tb.Context()))
	database.StopUsageCacheBackfill()
	return database
}

func sessionWrite(input Session) db.SessionBatchWrite {
	write := db.SessionBatchWrite{Session: db.Session{ID: input.ID, Project: input.Project, Machine: input.Machine, Agent: input.Agent, GitBranch: input.GitBranch, StartedAt: &input.StartedAt, UserMessageCount: input.UserMessageCount, IsAutomated: input.IsAutomated}}
	for _, message := range input.Messages {
		write.Messages = append(write.Messages, db.Message{SessionID: input.ID, Ordinal: message.Ordinal, Role: "assistant", Timestamp: message.Timestamp, Model: message.Model, TokenUsage: []byte(message.TokenUsage), ClaudeMessageID: message.ClaudeMessageID, ClaudeRequestID: message.ClaudeRequestID})
	}
	return write
}
