package kept

import (
	"context"
	"encoding/json/v2"
	"maps"
	"slices"
	"sync"

	"go.kenn.io/agentsview/internal/db"
)

// Usage keeps finished usage reads for a store's immutable version.
// Its zero value is ready to use.
type Usage struct {
	once  sync.Once
	daily usageMemo[db.DailyUsageResult]
	plan  usageMemo[db.PlanUsageSessionCounts]
	top   usageMemo[db.TopSessionEntry]
}

type usageMemo[T any] struct {
	Memo[T]
	flights Flights[[]T]
}

func (u *Usage) init() {
	u.once.Do(func() {
		u.daily.Size = dailyUsageBytes
		u.plan.Size = planUsageBytes
		u.top.Size = TopSessionBytes
	})
}

// Clear drops the finished results of all three reads.
func (u *Usage) Clear() {
	u.daily.DeleteSlots(func(string) bool { return true })
	u.plan.DeleteSlots(func(string) bool { return true })
	u.top.DeleteSlots(func(string) bool { return true })
}

func usageRows[T any](ctx context.Context, memo *usageMemo[T], f db.UsageFilter, version string, load func(context.Context, db.UsageFilter) ([]T, error)) ([]T, error) {
	key, err := json.Marshal(f, json.Deterministic(true))
	if err != nil {
		return load(ctx, f)
	}
	slot := string(key)
	if rows, ok := memo.Get(slot, version); ok {
		return rows, nil
	}
	return memo.flights.Do(ctx, version+"|"+slot, func(ctx context.Context) ([]T, error) {
		if rows, ok := memo.Get(slot, version); ok {
			return rows, nil
		}
		rows, err := load(ctx, f)
		if err == nil {
			memo.Put(slot, version, rows)
		}
		return rows, err
	})
}

func usageFilter(f db.UsageFilter) db.UsageFilter {
	f.Progress = nil
	f.TopSessionsByGroup = false
	f.TopSessionsSort = ""
	f.TopSessionsTokenTypes = 0
	f.SkipSessionCounts = false
	return f
}

// Daily always builds counts so Summary and Pairwise can share either arrival order.
func (u *Usage) Daily(ctx context.Context, f db.UsageFilter, version string, load func(context.Context, db.UsageFilter) (db.DailyUsageResult, error)) (db.DailyUsageResult, error) {
	u.init()
	// Handles without a version retain their ordinary read behavior.
	if version == "" || db.TerminationUsesTime(f.Termination) {
		return load(ctx, f)
	}
	rows, err := usageRows(ctx, &u.daily, usageFilter(f), version, func(ctx context.Context, f db.UsageFilter) ([]db.DailyUsageResult, error) {
		result, err := load(ctx, f)
		return []db.DailyUsageResult{result}, err
	})
	if err != nil {
		return db.DailyUsageResult{}, err
	}
	result := cloneDaily(rows[0])
	if f.SkipSessionCounts {
		result.SessionCounts = db.UsageSessionCounts{}
	}
	return result, nil
}

// Plan keeps plan-only and partly captured session counts.
func (u *Usage) Plan(ctx context.Context, f db.UsageFilter, version string, load func(context.Context, db.UsageFilter) (db.PlanUsageSessionCounts, error)) (db.PlanUsageSessionCounts, error) {
	u.init()
	if version == "" || db.TerminationUsesTime(f.Termination) {
		return load(ctx, f)
	}
	f = usageFilter(f)
	f.Breakdowns = false
	rows, err := usageRows(ctx, &u.plan, f, version, func(ctx context.Context, f db.UsageFilter) ([]db.PlanUsageSessionCounts, error) {
		result, err := load(ctx, f)
		return []db.PlanUsageSessionCounts{result}, err
	})
	if err != nil {
		return db.PlanUsageSessionCounts{}, err
	}
	result := rows[0]
	result.PlanOnlyByModel = maps.Clone(result.PlanOnlyByModel)
	return result, nil
}

// TopTotals keeps all session totals before grouping, sorting, or limiting.
func (u *Usage) TopTotals(ctx context.Context, f db.UsageFilter, version string, load func(context.Context, db.UsageFilter) ([]db.TopSessionEntry, error)) ([]db.TopSessionEntry, error) {
	u.init()
	if version == "" || db.TerminationUsesTime(f.Termination) {
		return load(ctx, f)
	}
	f = usageFilter(f)
	f.Breakdowns = false
	rows, err := usageRows(ctx, &u.top, f, version, load)
	return slices.Clone(rows), err
}

func cloneDaily(result db.DailyUsageResult) db.DailyUsageResult {
	result.Projects = maps.Clone(result.Projects)
	result.SessionCounts.ByProject = maps.Clone(result.SessionCounts.ByProject)
	result.SessionCounts.ByAgent = maps.Clone(result.SessionCounts.ByAgent)
	result.Daily = slices.Clone(result.Daily)
	for i := range result.Daily {
		day := &result.Daily[i]
		day.ModelsUsed = slices.Clone(day.ModelsUsed)
		day.ModelBreakdowns = slices.Clone(day.ModelBreakdowns)
		day.ProjectBreakdowns = slices.Clone(day.ProjectBreakdowns)
		day.AgentBreakdowns = slices.Clone(day.AgentBreakdowns)
		day.MachineBreakdowns = slices.Clone(day.MachineBreakdowns)
	}
	return result
}
