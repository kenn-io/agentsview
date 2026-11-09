//go:build chtest

package clickhouse

import (
	"strings"
	"testing"
	"time"

	chdriver "github.com/ClickHouse/clickhouse-go/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/money"
	"go.kenn.io/agentsview/internal/service"
)

func TestUsagePairwiseRankingReadCounts(t *testing.T) {
	t.Parallel()
	for _, scenario := range []string{"default", "selected", "fallback"} {
		t.Run(scenario, func(t *testing.T) {
			t.Parallel()
			store, syncer, local := newUsagePriceStore(t)
			comment := "pairwise-ranking-" + syncer.target.Database
			ctx := chdriver.Context(t.Context(), chdriver.WithSettings(chdriver.Settings{"log_comment": comment}))
			backend := service.NewReadOnlyBackend(store)
			req := service.UsagePairwiseComparisonRequest{UsageRequest: service.UsageRequest{From: "2026-01-12", To: "2026-01-12", Timezone: "UTC", Agent: "hermes", IncludeOneShot: true}}
			wantQueries := uint64(1)
			if scenario == "fallback" {
				seedUsagePairwiseDuplicates(t, local)
				_, err := syncer.Push(t.Context(), false, nil)
				require.NoError(t, err)
				req.Agent = ""
				wantQueries = 2
			}
			if scenario != "default" {
				req.LeftDimension, req.LeftValue = "model", "round-test"
				req.RightDimension, req.RightValue = "model", "tier-test"
				req.FallbackToDefault = true
			}
			_, err := backend.UsagePairwiseComparison(ctx, req)
			require.NoError(t, err)
			_, err = store.conn.ExecContext(t.Context(), "SYSTEM FLUSH LOGS")
			require.NoError(t, err)
			var queries uint64
			require.NoError(t, store.conn.QueryRowContext(t.Context(), `
				SELECT count() FROM system.query_log
				WHERE type = 'QueryFinish' AND log_comment = ?
				AND startsWith(trimLeft(query, ' \n\t\r'), 'WITH usage_raw AS')
				AND position(query, 'FROM usage_classified') > 0`, comment).Scan(&queries))
			assert.Equal(t, wantQueries, queries)
		})
	}
}

func TestUsageReadersSkipPairwiseEvidence(t *testing.T) {
	t.Parallel()
	for _, reader := range []string{"summary", "friction"} {
		t.Run(reader, func(t *testing.T) {
			t.Parallel()
			store, syncer, _ := newUsagePriceStore(t)
			comment := reader + "-without-pairwise-" + syncer.target.Database
			ctx := chdriver.Context(t.Context(), chdriver.WithSettings(chdriver.Settings{"log_comment": comment}))
			var err error
			if reader == "summary" {
				_, err = service.NewReadOnlyBackend(store).UsageSummary(ctx, service.UsageRequest{Timezone: "UTC", Breakdowns: new(true), IncludeOneShot: true})
			} else {
				_, err = db.FrictionArchiveSpendFrom(ctx, store, "", "", time.UTC)
			}
			require.NoError(t, err)
			_, err = store.conn.ExecContext(t.Context(), "SYSTEM FLUSH LOGS")
			require.NoError(t, err)
			var queries, evidenceQueries uint64
			require.NoError(t, store.conn.QueryRowContext(t.Context(), `
				SELECT count(), countIf(position(query, 'inexact_models') > 0)
				FROM system.query_log WHERE type = 'QueryFinish' AND log_comment = ?
				AND startsWith(trimLeft(query, ' \n\t\r'), 'WITH usage_raw AS')`, comment).Scan(&queries, &evidenceQueries))
			assert.Equal(t, uint64(1), queries)
			assert.Zero(t, evidenceQueries)
		})
	}
}

func TestUsagePairwiseSidesMatchFilteredReads(t *testing.T) {
	t.Parallel()
	for _, raw := range []bool{false, true} {
		t.Run(map[bool]string{false: "prepared", true: "raw"}[raw], func(t *testing.T) {
			local, syncer, store := newPreparedUsageStore(t)
			if raw {
				execPreparedUsage(t, store, "TRUNCATE TABLE prepared_usage")
			}
			for _, phase := range []string{"first", "after push", "custom"} {
				t.Run(phase, func(t *testing.T) {
					if phase == "after push" {
						require.NoError(t, local.ReplaceSessionUsageEvents(t.Context(), usagePriceRoundID, []db.UsageEvent{{
							Source: "hermes", Model: "round-test", InputTokens: 2, DedupKey: "added", OccurredAt: "2026-01-12T04:00:00Z",
						}}))
						_, err := syncer.Push(t.Context(), false, nil)
						require.NoError(t, err)
					}
					if phase == "custom" {
						store.SetCustomPricing(map[string]config.CustomModelRate{"round-test": {InputMicrodollarsPerMTok: 800_000}})
					}
					filters := usagePriceFilters()
					filters["local window"] = db.UsageFilter{Timezone: "America/New_York", From: "2026-01-11", To: "2026-01-11"}
					for name, f := range filters {
						t.Run(name, func(t *testing.T) {
							f.Breakdowns, f.PairwiseSides = true, true
							ranking, err := store.GetDailyUsage(t.Context(), f)
							require.NoError(t, err)
							require.NotNil(t, ranking.Pairwise)
							for _, selection := range []struct {
								dimension string
								values    []string
							}{
								{"model", []string{"round-test"}}, {"model", []string{"tier-test"}},
								{"model", []string{"claude-test"}}, {"model", []string{"round-test", "tier-test"}},
								{"project", []string{"gamma"}}, {"project", []string{"delta"}},
							} {
								got, ok := ranking.Pairwise.Side(selection.dimension, selection.values)
								require.True(t, ok)
								filtered := f
								filtered.Breakdowns = false
								if selection.dimension == "model" {
									filtered.Model = strings.Join(selection.values, ",")
									// A base model selection intersects the side.
									if f.HasModelFilter() && f.Model != filtered.Model {
										continue
									}
								} else {
									if f.Project != "" && f.Project != selection.values[0] {
										continue
									}
									filtered.Project, filtered.ProjectLabels = "", selection.values
								}
								want, err := store.GetDailyUsage(t.Context(), filtered)
								require.NoError(t, err)
								assert.Equal(t, want.Totals.InputTokens, got.Totals.InputTokens)
								assert.Equal(t, want.Totals.OutputTokens, got.Totals.OutputTokens)
								assert.Equal(t, want.Totals.CacheCreationTokens, got.Totals.CacheCreationTokens)
								assert.Equal(t, want.Totals.CacheReadTokens, got.Totals.CacheReadTokens)
								assert.Equal(t, want.Totals.TotalCost, got.Totals.TotalCost)
								assert.Equal(t, want.SessionCounts.Total, got.SessionCounts.Total)
							}
						})
					}
				})
			}
		})
	}
}

func TestUsagePairwiseInexactSides(t *testing.T) {
	t.Parallel()
	for _, scenario := range []string{"duplicates", "overflow"} {
		t.Run(scenario, func(t *testing.T) {
			t.Parallel()
			store, syncer, local := newUsagePriceStore(t)
			selections := []struct{ dimension, value string }{{"model", "tier-test"}, {"model", "only-dropped"}, {"project", "loser,raw"}, {"project", "absent"}}
			filter := db.UsageFilter{Timezone: "UTC", Breakdowns: true, PairwiseSides: true}
			if scenario == "duplicates" {
				seedUsagePairwiseDuplicates(t, local)
			} else {
				cost := money.Money{Microdollars: 9_100_000_000_000_000_000}
				for _, id := range []string{"large-a", "large-b"} {
					session := fixtureSession(id, "large", "large", "2026-01-12T06:00:00Z", 0)
					session.Agent = "copilot"
					_, err := local.WriteSessionBatchAtomic(t.Context(), []db.SessionBatchWrite{{Session: session, DataVersion: 1, ReplaceMessages: true, UsageEvents: []db.UsageEvent{
						{Source: "provider", Model: "large", Cost: &cost, CostSource: "reported", OccurredAt: "2026-01-12T06:00:00Z", DedupKey: "estimate"},
						{Source: "shutdown", Model: "large", Cost: new(money.MustParseDollars("0.5")), CostSource: db.CopilotReportedCostSource, OccurredAt: "2026-01-12T06:01:00Z", DedupKey: "total"},
					}}})
					require.NoError(t, err)
				}
				filter.Project = "large"
				selections = []struct{ dimension, value string }{{"model", "large"}}
			}
			_, err := syncer.Push(t.Context(), false, nil)
			require.NoError(t, err)
			ranking, err := store.GetDailyUsage(t.Context(), filter)
			require.NoError(t, err)
			require.NotNil(t, ranking.Pairwise)
			for _, selection := range selections {
				_, ok := ranking.Pairwise.Side(selection.dimension, []string{selection.value})
				assert.False(t, ok)
			}
		})
	}
}

func seedUsagePairwiseDuplicates(t *testing.T, local *db.DB) {
	t.Helper()
	for i, project := range []string{"winner", "loser,raw", "absent"} {
		stamp := "2026-01-12T05:00:00Z"
		m := usagePriceMessage(project, 0, stamp, []string{"round-test", "tier-test", "only-dropped"}[i], `{"input_tokens":2000}`)
		m.SourceUUID = "shared-source"
		// Order the winner before each losing row.
		m.Timestamp = []string{stamp, "2026-01-12T05:01:00Z", "2026-01-12T05:02:00Z"}[i]
		_, err := local.WriteSessionBatchAtomic(t.Context(), []db.SessionBatchWrite{{Session: fixtureSession(project, project, "duplicate", stamp, 1), Messages: []db.Message{m}, DataVersion: 1, ReplaceMessages: true}})
		require.NoError(t, err)
	}
}
