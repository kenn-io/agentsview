package db

import (
	"context"

	"go.kenn.io/agentsview/internal/kept"
)

// DailyUsage keeps full counts and returns an independent result for each caller.
func DailyUsage(ctx context.Context, reads *kept.Reads[DailyUsageResult], slot, version string, f UsageFilter, load func(context.Context, UsageFilter) (DailyUsageResult, error)) (DailyUsageResult, error) {
	result, err := reads.Do(ctx, slot, version, func(ctx context.Context) (DailyUsageResult, error) {
		full := f
		full.SkipSessionCounts = false
		return load(ctx, full)
	})
	result = result.Clone()
	if f.SkipSessionCounts {
		result.SessionCounts = UsageSessionCounts{}
	}
	return result, err
}

// PlanUsageCounts applies each caller's aliases to a shared raw summary.
func PlanUsageCounts(ctx context.Context, reads *kept.Reads[*PlanUsageCountSummary], slot, version string, f UsageFilter, load func(context.Context, UsageFilter) (*PlanUsageCountSummary, error)) (PlanUsageSessionCounts, error) {
	tally, err := reads.Do(ctx, slot, version, func(ctx context.Context) (*PlanUsageCountSummary, error) {
		raw := f
		raw.ModelAliases = nil
		return load(ctx, raw)
	})
	if err != nil {
		return PlanUsageSessionCounts{}, err
	}
	return tally.CountsFor(f.ModelAliases), nil
}

// TopSessionsBytes sizes a finished list for the kept read's memory bound.
func TopSessionsBytes(entries []TopSessionEntry) int64 {
	var size int64
	for _, entry := range entries {
		size += TopSessionBytes(entry)
	}
	return size
}
