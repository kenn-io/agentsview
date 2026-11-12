package db

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"go.kenn.io/agentsview/internal/kept"
)

func newDailyUsageMemo() kept.Memo[DailyUsageResult] {
	return kept.Memo[DailyUsageResult]{Size: DailyUsageResult.Bytes}
}

// dailyUsageVersion reads the archive change tokens from one SQLite snapshot
// after the generation that pricing changes and reopens advance.
func (db *DB) dailyUsageVersion(ctx context.Context) string {
	var generation uint64
	if db.sessionStatsFacts != nil {
		generation = db.sessionStatsFacts.Generation()
	}
	var id, usage, project string
	var cursor int64
	err := db.getReader().QueryRowContext(ctx, `SELECT
		COALESCE((SELECT value FROM archive_metadata WHERE key = ?), ''),
		COALESCE((SELECT value FROM archive_metadata WHERE key = ?), '0'),
		COALESCE((SELECT value FROM archive_metadata WHERE key = ?), '0'),
		COALESCE((SELECT MAX(id) FROM cursor_usage_events), 0)`,
		archiveMetadataDatabaseIDKey, archiveMetadataUsageChangeRevisionKey,
		archiveMetadataProjectIdentityRevisionKey,
	).Scan(&id, &usage, &project, &cursor)
	if err != nil || strings.TrimSpace(id) == "" || cursor < 0 {
		return ""
	}
	for _, raw := range []string{usage, project} {
		n, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
		if err != nil || n < 0 {
			return ""
		}
	}
	return fmt.Sprintf("%q:%s:%s:%d:%d", id, usage, project, cursor, generation)
}

func (db *DB) keptDailyUsage(ctx context.Context, filter UsageFilter) (DailyUsageResult, error) {
	slot := UsageSlot(filter, true)
	version := ""
	if slot != "" {
		version = db.dailyUsageVersion(ctx)
	}
	if err := ctx.Err(); err != nil {
		return DailyUsageResult{}, err
	}
	if version == "" {
		return db.getDailyUsageUncached(ctx, filter)
	}
	rows, ok := db.usageKept.Get(slot, version)
	if !ok {
		full := filter
		full.SkipSessionCounts = false
		full.PairwiseSides = filter.PairwiseSides || filter.Breakdowns
		loadCtx := context.WithValue(ctx, usageWindowVersionKey{}, version)
		result, err := db.getDailyUsageUncached(loadCtx, full)
		if err != nil {
			return result, err
		}
		rows = []DailyUsageResult{result}
		if db.dailyUsageVersion(ctx) == version {
			db.usageKept.Put(slot, version, rows)
		}
	}
	result := rows[0].Clone()
	if filter.SkipSessionCounts {
		result.SessionCounts = UsageSessionCounts{}
	}
	return result, nil
}

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
