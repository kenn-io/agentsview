//go:build pgtest

package postgres

import (
	"context"
	"database/sql"
	"maps"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/money"
	"go.kenn.io/agentsview/internal/service"
)

type usageReadCounter struct {
	storeExecutor
	mu             sync.Mutex
	queries        map[string]int
	revisionReads  int
	revisionErrors []error
}

// Hold the first pricing query after PostgreSQL has taken its statement snapshot.
type heldUsagePricing struct {
	storeExecutor
	held    atomic.Bool
	started chan struct{}
	release chan struct{}
}

func (h *heldUsagePricing) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	rows, err := h.storeExecutor.QueryContext(ctx, query, args...)
	if err == nil && query == pgModelPricingSelect && h.held.CompareAndSwap(false, true) {
		close(h.started)
		select {
		case <-h.release:
		case <-ctx.Done():
		}
	}
	return rows, err
}

func TestUsageKeptMissAfterPricingPush(t *testing.T) {
	for _, tc := range []struct {
		name string
		want string
	}{
		{"daily", "0.0125"},
		{"top", "0.01"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			local, syncer, store, _ := keptUsageFixture(t)
			ctx := t.Context()
			held := &heldUsagePricing{storeExecutor: store.pg, started: make(chan struct{}), release: make(chan struct{})}
			store.pg = held
			release := sync.OnceFunc(func() { close(held.release) })
			old := store.startPricingLoad()
			var finished chan struct{}
			defer func() {
				release()
				old.cancel()
				<-old.done
				if finished != nil {
					<-finished
				}
			}()
			<-held.started
			require.NoError(t, local.UpsertModelPricing([]db.ModelPricing{{ModelPattern: "paid-a", OutputPerMTok: money.MustParseDollars("20")}}))
			pushed, err := syncer.Push(ctx, false, nil)
			require.NoError(t, err)
			require.Empty(t, pushed.Errors)
			f := db.UsageFilter{From: "2026-01-01", To: "2026-01-01", Timezone: "UTC"}
			var cost money.Money
			var readErr error
			finished = make(chan struct{})
			go func() {
				defer close(finished)
				if tc.name == "daily" {
					result, err := store.GetDailyUsage(ctx, f)
					cost, readErr = result.Totals.TotalCost, err
				} else {
					result, err := store.GetTopSessionsByCost(ctx, f, 1)
					readErr = err
					if len(result) == 1 {
						cost = result[0].Cost
					}
				}
			}()
			// The new read either replaces the older flight or joins it.
			runtime := store.runtimeState()
			require.Eventually(t, func() bool {
				runtime.pricingLoadMu.Lock()
				defer runtime.pricingLoadMu.Unlock()
				return runtime.pricingLoad != old || old.waiters > 1
			}, 5*time.Second, 10*time.Millisecond)
			release()
			<-finished
			require.NoError(t, readErr)
			assert.Equal(t, money.MustParseDollars(tc.want), cost)
		})
	}
}

func (c *usageReadCounter) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	rows, err := c.storeExecutor.QueryContext(ctx, query, args...)
	if strings.Contains(query, "usage_change_revision") {
		c.mu.Lock()
		c.revisionReads++
		if err != nil {
			c.revisionErrors = append(c.revisionErrors, err)
		}
		c.mu.Unlock()
	}
	if strings.Contains(query, "usage_events") && !strings.Contains(query, "usage_session_changes") {
		c.mu.Lock()
		c.queries[query]++
		c.mu.Unlock()
	}
	return rows, err
}

func (c *usageReadCounter) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	row := c.storeExecutor.QueryRowContext(ctx, query, args...)
	if strings.Contains(query, "usage_change_revision") {
		c.mu.Lock()
		c.revisionReads++
		if err := row.Err(); err != nil {
			c.revisionErrors = append(c.revisionErrors, err)
		}
		c.mu.Unlock()
	}
	return row
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

func TestUsageKeptWithoutJournal(t *testing.T) {
	_, syncer, store, counter := keptUsageFixture(t)
	ctx := t.Context()
	// Optional journal installation leaves no version row when it cannot run.
	_, err := syncer.pg.ExecContext(ctx, dropUsageChangeJournalSQL)
	require.NoError(t, err)
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
	want := read(&Store{pg: counter.storeExecutor})
	require.Equal(t, 1000, want[0].(db.DailyUsageResult).Totals.OutputTokens)
	require.Equal(t, 1, want[1].(db.PlanUsageSessionCounts).PlanOnly)
	require.Len(t, want[2], 2)
	for range 2 {
		assert.Equal(t, want, read(store))
	}
	assert.Equal(t, 6, counter.revisionReads)
	assert.Empty(t, counter.revisionErrors, "Usage revision probes must succeed without a journal")
	for _, count := range counter.queries {
		assert.Equal(t, 2, count, "archives without a journal must read fresh")
	}
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

func TestUsageKeptSchemaRebuild(t *testing.T) {
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
	before := read(store)
	require.Equal(t, 1000, before[0].(db.DailyUsageResult).Totals.OutputTokens)
	require.Equal(t, 1, before[1].(db.PlanUsageSessionCounts).PlanOnly)
	require.Len(t, before[2], 2)
	queries := maps.Clone(counter.queries)
	require.Equal(t, before, read(store))
	require.Equal(t, queries, counter.queries)
	revision, err := store.UsageChangeRevision(ctx)
	require.NoError(t, err)

	require.NoError(t, local.DeleteSession(ctx, "paid-b"))
	require.NoError(t, local.DeleteSession(ctx, "plan"))
	require.NoError(t, local.RenameSession(ctx, "paid-a", new("Rebuilt session")))
	require.NoError(t, local.ReplaceSessionUsageEvents(ctx, "paid-a", []db.UsageEvent{{SessionID: "paid-a", Source: "provider", Model: "paid-a", OutputTokens: 900, DedupKey: "rebuilt", OccurredAt: "2026-01-01T09:00:00Z"}}))
	require.NoError(t, local.UpsertModelPricing([]db.ModelPricing{{ModelPattern: "paid-a", OutputPerMTok: money.MustParseDollars("20")}}))
	_, err = syncer.pg.ExecContext(ctx, `DROP SCHEMA `+syncer.schema+` CASCADE`)
	require.NoError(t, err)
	require.NoError(t, EnsureSchema(ctx, syncer.pg, syncer.schema))
	pushed, err := syncer.Push(ctx, true, nil)
	require.NoError(t, err)
	require.Empty(t, pushed.Errors)
	afterRevision, err := store.UsageChangeRevision(ctx)
	require.NoError(t, err)
	require.NotEqual(t, revision, afterRevision)

	fresh, err := NewStore(ctx, testPGURL(t), syncer.schema, true)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, fresh.Close()) })
	want := read(fresh)
	require.Equal(t, 900, want[0].(db.DailyUsageResult).Totals.OutputTokens)
	require.Zero(t, want[1].(db.PlanUsageSessionCounts).PlanOnly)
	require.Len(t, want[2], 1)
	got := read(store)
	for i, name := range []string{"daily usage", "plan counts", "top sessions"} {
		assert.NotEqual(t, before[i], got[i], name)
		assert.Equal(t, want[i], got[i], name)
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
	f, err := service.BuildUsageFilter(req, groups)
	require.NoError(t, err)
	raw, err := store.GetDailyUsage(t.Context(), f)
	require.NoError(t, err)
	require.NotEmpty(t, raw.Daily)
	assert.Contains(t, raw.Daily[0].ModelsUsed, "paid-a")
}

func TestUsageKeptBypasses(t *testing.T) {
	t.Run("active", func(t *testing.T) {
		_, _, store, counter := keptUsageFixture(t)
		f := db.UsageFilter{Termination: "active"}
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
		assert.Zero(t, counter.revisionReads)
		for query, count := range firstReads {
			assert.Equal(t, 2*count, counter.queries[query])
		}
	})
}
