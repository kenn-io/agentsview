//go:build fts5

package db

import (
	"fmt"
	"os"
	"strconv"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db/testdata/usagefixture"
	"go.kenn.io/agentsview/internal/money"
)

// This fixture uses public archive/cache calls so the same benchmark can
// measure the pre-journal implementation. About 3% of one-row sessions span
// midnight, giving roughly 30,900 daily rows at 30,000 sessions.
func BenchmarkUsageRollupJournaled(b *testing.B) {
	count := 300
	if raw := os.Getenv("AGENTSVIEW_BENCH_SESSIONS"); raw != "" {
		var err error
		count, err = strconv.Atoi(raw)
		require.NoError(b, err)
		require.Positive(b, count)
	}
	for _, dailyRows := range []int{1, 3} {
		b.Run(fmt.Sprintf("rows-%d", dailyRows), func(b *testing.B) {
			database, filter, want := seedUsageJournalBenchmark(b, count, dailyRows)
			for _, readers := range []int{1, 7} {
				b.Run(fmt.Sprintf("readers-%d", readers), func(b *testing.B) {
					b.ReportAllocs()
					for range b.N {
						if readers == 1 {
							got, err := database.GetDailyUsage(b.Context(), filter)
							require.NoError(b, err)
							require.Equal(b, want, got)
							continue
						}
						var wg sync.WaitGroup
						errors := make(chan error, readers)
						for range readers {
							wg.Go(func() {
								got, err := database.GetDailyUsage(b.Context(), filter)
								// Fatal assertions belong to the benchmark goroutine.
								if err != nil {
									errors <- err
									return
								}
								assert.Equal(b, want, got)
							})
						}
						wg.Wait()
						close(errors)
						for err := range errors {
							require.NoError(b, err)
						}
					}
				})
			}
		})
	}
}

func seedUsageJournalBenchmark(tb testing.TB, count, dailyRows int) (*DB, UsageFilter, DailyUsageResult) {
	tb.Helper()
	database := testDB(tb)
	// Project keys include archive identity, so separate runs need the same scope.
	require.NoError(tb, database.SetArchiveIdentityForTest(tb.Context(), usagefixture.ArchiveID, usagefixture.ArchiveKey))

	var prices []ModelPricing
	for _, model := range usagefixture.Models {
		prices = append(prices, ModelPricing{ModelPattern: model, InputPerMTok: money.Money{Microdollars: 1_000_000}, OutputPerMTok: money.Money{Microdollars: 2_000_000}})
	}
	require.NoError(tb, database.UpsertModelPricing(prices))
	// Stable catalog timestamps make separate base/head runs byte-comparable.
	_, err := database.getWriter().ExecContext(tb.Context(), `UPDATE model_pricing SET updated_at=? WHERE model_pattern IN ('journal-model', 'journal-other')`, usagefixture.PricingTimestamp)
	require.NoError(tb, err)

	for start := 0; start < count; start += 256 {
		var writes []SessionBatchWrite
		for i := start; i < min(start+256, count); i++ {
			writes = append(writes, usageFixtureWrite(usagefixture.Journal(i, dailyRows)))
		}
		_, err := database.WriteSessionBatchAtomic(tb.Context(), writes)
		require.NoError(tb, err)
	}
	filter := UsageFilter{From: "2026-08-04", To: "2026-08-10", Timezone: "UTC", SkipSessionCounts: true}
	want, err := database.GetDailyUsage(tb.Context(), filter)
	require.NoError(tb, err)
	require.NoError(tb, database.StartUsageCacheBackfill(tb.Context()))
	require.NoError(tb, database.WaitUsageCacheBackfill(tb.Context()))
	recent := count/10*4 + min(count%10, 4)
	require.Equal(tb, recent*300, want.Totals.InputTokens)
	require.Equal(tb, recent*60, want.Totals.OutputTokens)
	require.Equal(tb, int64(recent*420), want.Totals.TotalCost.Microdollars)
	return database, filter, want
}

func usageFixtureWrite(input usagefixture.Session) SessionBatchWrite {
	write := SessionBatchWrite{Session: Session{ID: input.ID, Project: input.Project, Machine: input.Machine, Agent: input.Agent, GitBranch: input.GitBranch, StartedAt: &input.StartedAt, UserMessageCount: input.UserMessageCount, IsAutomated: input.IsAutomated}}
	for _, message := range input.Messages {
		write.Messages = append(write.Messages, Message{SessionID: input.ID, Ordinal: message.Ordinal, Role: "assistant", Timestamp: message.Timestamp, Model: message.Model, TokenUsage: []byte(message.TokenUsage), ClaudeMessageID: message.ClaudeMessageID, ClaudeRequestID: message.ClaudeRequestID})
	}
	return write
}
