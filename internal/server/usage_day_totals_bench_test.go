//go:build fts5

package server

import (
	"context"
	"crypto/sha256"
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
	"go.kenn.io/agentsview/internal/db/testdata/usagefixture"
	"go.kenn.io/agentsview/internal/service"
)

var usageDayTotalsPageWindows = []struct {
	name string
	from string
}{
	{"7-days", "2026-08-04"},
	{"30-days", "2026-07-12"},
	{"90-days", "2026-05-13"},
	{"all-history", "2026-05-01"},
}

// The browser starts these three requests together. Pairwise owns its ranking
// and default side reads; Comparison owns the prior-period range.
func readUsageDayTotalsPage(ctx context.Context, s *Server, from string) ([3][]byte, error) {
	input := UsageFilterInput{From: from, To: "2026-08-10", Timezone: "UTC", IncludeOneShot: true, Breakdowns: true, SessionCounts: true}
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
		out, err := s.humaUsagePairwiseComparison(ctx, &usagePairwiseComparisonInput{UsageFilterInput: input})
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

// Page reads measure timing; filter parity belongs to the database tests.
func checkUsageDayTotalsPage(tb testing.TB, name string, got [3][]byte) {
	tb.Helper()
	for i := range got {
		file := fmt.Sprintf("%s-%d.json", name, i)
		if dir := os.Getenv("AGENTSVIEW_USAGE_BASE_RESULTS_DIR"); dir != "" {
			base, err := os.ReadFile(filepath.Join(dir, file))
			require.NoError(tb, err)
			require.Equal(tb, string(base), string(got[i]), "base/head %s", file)
		}
		if dir := os.Getenv("AGENTSVIEW_USAGE_RESULTS_DIR"); dir != "" {
			require.NoError(tb, os.WriteFile(filepath.Join(dir, file), got[i], 0o600))
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
	database := usagefixture.SeedPage(b, count)
	s := &Server{db: database, sessions: service.NewReadOnlyBackend(database)}
	for _, window := range usageDayTotalsPageWindows {
		name := window.name
		b.Run(name, func(b *testing.B) {
			_, err := readUsageDayTotalsPage(b.Context(), s, window.from)
			require.NoError(b, err)
			runtime.GC()
			b.ResetTimer()
			for range b.N {
				got, err := readUsageDayTotalsPage(b.Context(), s, window.from)
				b.StopTimer()
				require.NoError(b, err)
				checkUsageDayTotalsPage(b, name, got)
				b.StartTimer()
			}
		})
	}
	ordinal := 3
	b.Run("first-totals-read-after-append", func(b *testing.B) {
		b.StopTimer()
		filter := db.UsageFilter{From: "2026-08-04", To: "2026-08-10", Timezone: "UTC", Breakdowns: true}
		before, err := database.GetDailyUsage(b.Context(), filter)
		require.NoError(b, err)
		b.ResetTimer()
		for i := range b.N {
			require.NoError(b, database.InsertMessages(b.Context(), []db.Message{{SessionID: "journal-00000", Ordinal: ordinal, Role: "assistant", Timestamp: "2026-08-04T12:03:00Z", Model: "journal-model", TokenUsage: []byte(`{"input_tokens":9}`)}}))
			ordinal++
			b.StartTimer()
			got, err := database.GetDailyUsage(b.Context(), filter)
			b.StopTimer()
			require.NoError(b, err)
			require.Equal(b, before.Totals.InputTokens+9*(i+1), got.Totals.InputTokens)
		}
	})
}

func TestUsageDayTotalsPageBudget(t *testing.T) {
	if os.Getenv("AGENTSVIEW_USAGE_PAGE_GATE") == "" {
		t.Skip("set AGENTSVIEW_USAGE_PAGE_GATE for the 30k-session timing gate")
	}
	database := usagefixture.SeedPage(t, 30000)
	s := &Server{db: database, sessions: service.NewReadOnlyBackend(database)}
	for _, window := range usageDayTotalsPageWindows {
		name := window.name
		t.Run(name, func(t *testing.T) {
			_, err := readUsageDayTotalsPage(t.Context(), s, window.from)
			require.NoError(t, err)
			runtime.GC()
			start := time.Now()
			got, err := readUsageDayTotalsPage(t.Context(), s, window.from)
			elapsed := time.Since(start)
			require.NoError(t, err)
			t.Logf("warm page=%s", elapsed)
			assert.Less(t, elapsed, time.Second/2)
			checkUsageDayTotalsPage(t, name, got)
		})
	}
}
