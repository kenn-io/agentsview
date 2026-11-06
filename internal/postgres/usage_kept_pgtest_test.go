//go:build pgtest

package postgres

import (
	"context"
	"database/sql"
	"maps"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/money"
	"go.kenn.io/agentsview/internal/service"
)

type usageReadCounter struct {
	storeExecutor
	mu      sync.Mutex
	queries map[string]int
}

func (c *usageReadCounter) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	if strings.Contains(query, "usage_events") && !strings.Contains(query, "usage_session_changes") {
		c.mu.Lock()
		c.queries[query]++
		c.mu.Unlock()
	}
	return c.storeExecutor.QueryContext(ctx, query, args...)
}

func keptUsageFixture(t *testing.T) (*db.DB, *Sync, *Store, *usageReadCounter) {
	t.Helper()
	local, syncer, store := newUsageJournalFixture(t)
	ctx := t.Context()
	for _, id := range []string{"paid-a", "paid-b", "plan"} {
		require.NoError(t, local.UpsertSession(ctx, db.Session{ID: id, Project: "project-a", Machine: "test-machine", Agent: "codex", StartedAt: new("2026-01-01T09:00:00Z"), EndedAt: new("2026-01-01T10:00:00Z")}))
		require.NoError(t, local.InsertMessages(ctx, []db.Message{{SessionID: id, Ordinal: 0, Role: "assistant", Content: "answer", Model: id, Timestamp: "2026-01-01T09:00:00Z"}}))
	}
	for _, id := range []string{"paid-a", "paid-b"} {
		require.NoError(t, local.ReplaceSessionUsageEvents(ctx, id, []db.UsageEvent{{SessionID: id, Source: "provider", Model: id, OutputTokens: 500, DedupKey: "event", OccurredAt: "2026-01-01T09:00:00Z"}}))
	}
	require.NoError(t, local.ReplaceSessionPlanUsage(ctx, "plan", fullPlanUsage()))
	require.NoError(t, local.UpsertModelPricing([]db.ModelPricing{{ModelPattern: "paid-a", OutputPerMTok: money.MustParseDollars("10")}, {ModelPattern: "paid-b", OutputPerMTok: money.MustParseDollars("5")}}))
	result, err := syncer.Push(ctx, false, nil)
	require.NoError(t, err)
	require.Empty(t, result.Errors)
	counter := &usageReadCounter{storeExecutor: store.pg, queries: map[string]int{}}
	store.pg = counter
	return local, syncer, store, counter
}

func TestUsageKeptPushBetweenReads(t *testing.T) {
	local, syncer, store, counter := keptUsageFixture(t)
	ctx := t.Context()
	f := db.UsageFilter{From: "2026-01-01", To: "2026-01-01", Timezone: "UTC", Breakdowns: true}
	read := func(s *Store) []any {
		daily, err := s.GetDailyUsage(ctx, f)
		require.NoError(t, err)
		counts, err := s.GetPlanUsageSessionCounts(ctx, f)
		require.NoError(t, err)
		top, err := s.GetTopSessionsByCost(ctx, f, 10)
		require.NoError(t, err)
		return []any{daily, counts, top}
	}
	previous := read(store)
	require.Equal(t, 1000, previous[0].(db.DailyUsageResult).Totals.OutputTokens)
	require.Equal(t, 1, previous[1].(db.PlanUsageSessionCounts).PlanOnly)
	require.Len(t, previous[2], 2)
	require.Len(t, counter.queries, 3)
	queries := maps.Clone(counter.queries)
	require.Equal(t, previous, read(store))
	require.Equal(t, queries, counter.queries)
	for _, tc := range []struct {
		name   string
		change func()
	}{
		{"usage", func() {
			require.NoError(t, local.ReplaceSessionUsageEvents(ctx, "paid-a", []db.UsageEvent{{SessionID: "paid-a", Source: "provider", Model: "paid-a", OutputTokens: 900, DedupKey: "new", OccurredAt: "2026-01-01T09:00:00Z"}}))
		}},
		{"rename", func() { require.NoError(t, local.RenameSession(ctx, "paid-a", new("Renamed session"))) }},
		{"pricing", func() {
			require.NoError(t, local.UpsertModelPricing([]db.ModelPricing{{ModelPattern: "paid-a", OutputPerMTok: money.MustParseDollars("20")}}))
		}},
		{"custom pricing", func() {
			store.SetCustomPricing(map[string]config.CustomModelRate{"paid-a": {OutputMicrodollarsPerMTok: 30_000_000}})
		}},
		{"soft delete", func() { require.NoError(t, local.SoftDeleteSession(ctx, "paid-a")) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.change()
			result, err := syncer.Push(ctx, false, nil)
			require.NoError(t, err)
			require.Empty(t, result.Errors)
			fresh := &Store{pg: counter.storeExecutor}
			fresh.SetCustomPricing(store.runtimeState().customPricing)
			got := read(store)
			assert.NotEqual(t, previous, got)
			assert.Equal(t, read(fresh), got)
			for query, count := range queries {
				assert.Equal(t, count+1, counter.queries[query])
			}
			previous, queries = got, maps.Clone(counter.queries)
			assert.Equal(t, got, read(store))
			assert.Equal(t, queries, counter.queries)
		})
	}
}

func TestUsageKeptServicePairwiseSharesSummary(t *testing.T) {
	_, _, store, counter := keptUsageFixture(t)
	groups := service.NewModelGroups(map[string]string{"paid-a": "group-a", "paid-b": "group-b", "plan": "plan-group"})
	backend := service.NewReadOnlyBackend(store, service.WithModelGroups(groups))
	req := service.UsageRequest{From: "2026-01-01", To: "2026-01-01", Timezone: "UTC", IncludeOneShot: true, GroupModels: true}
	summary, err := backend.UsageSummary(t.Context(), req)
	require.NoError(t, err)
	require.Equal(t, 1, summary.PlanOnlySessions)
	require.Len(t, counter.queries, 2)
	queries := maps.Clone(counter.queries)
	pairReq := service.UsagePairwiseComparisonRequest{UsageRequest: req, LeftDimension: "model", LeftValue: "missing", RightDimension: "model", RightValue: "group-a", FallbackToDefault: true}
	got, err := backend.UsagePairwiseComparison(t.Context(), pairReq)
	require.NoError(t, err)
	require.ElementsMatch(t, []string{"group-a", "group-b"}, []string{got.Left.Value, got.Right.Value})
	require.Equal(t, 1000, got.Left.OutputTokens+got.Right.OutputTokens)
	for query, count := range queries {
		assert.Equal(t, count, counter.queries[query])
	}
	fresh := service.NewReadOnlyBackend(&Store{pg: counter.storeExecutor}, service.WithModelGroups(groups))
	want, err := fresh.UsagePairwiseComparison(t.Context(), pairReq)
	require.NoError(t, err)
	assert.Equal(t, want, got)
	raw, err := store.GetDailyUsage(t.Context(), db.UsageFilter{From: req.From, To: req.To, Timezone: "UTC", Breakdowns: true})
	require.NoError(t, err)
	require.NotEmpty(t, raw.Daily)
	assert.Contains(t, raw.Daily[0].ModelsUsed, "paid-a")
}

func TestUsageKeptBypasses(t *testing.T) {
	for _, mode := range []string{"active", "hosted"} {
		t.Run(mode, func(t *testing.T) {
			_, _, store, counter := keptUsageFixture(t)
			f := db.UsageFilter{}
			if mode == "hosted" {
				store.hostedRelations = true
			} else {
				f.Termination = "active"
			}
			var firstReads map[string]int
			for i := range 2 {
				_, err := store.GetDailyUsage(t.Context(), f)
				require.NoError(t, err)
				_, err = store.GetPlanUsageSessionCounts(t.Context(), f)
				require.NoError(t, err)
				_, err = store.GetTopSessionsByCost(t.Context(), f, 10)
				require.NoError(t, err)
				if i == 0 {
					firstReads = maps.Clone(counter.queries)
				}
			}
			require.NotEmpty(t, firstReads)
			for query, count := range firstReads {
				assert.Equal(t, 2*count, counter.queries[query])
			}
		})
	}
}
