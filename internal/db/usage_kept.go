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

// dailyUsageVersion reads the archive change tokens from one SQLite snapshot.
func (db *DB) dailyUsageVersion(ctx context.Context) string {
	var id, usage, pricing, project string
	var cursor int64
	err := db.getReader().QueryRowContext(ctx, `SELECT
		COALESCE((SELECT value FROM archive_metadata WHERE key = ?), ''),
		COALESCE((SELECT value FROM archive_metadata WHERE key = ?), '0'),
		COALESCE((SELECT value FROM archive_metadata WHERE key = ?), '0'),
		COALESCE((SELECT value FROM archive_metadata WHERE key = ?), '0'),
		COALESCE((SELECT MAX(id) FROM cursor_usage_events), 0)`,
		archiveMetadataDatabaseIDKey, archiveMetadataUsageChangeRevisionKey,
		archiveMetadataUsagePricingRevisionKey, archiveMetadataProjectIdentityRevisionKey,
	).Scan(&id, &usage, &pricing, &project, &cursor)
	if err != nil || strings.TrimSpace(id) == "" || cursor < 0 {
		return ""
	}
	for _, raw := range []string{usage, pricing, project} {
		n, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
		if err != nil || n < 0 {
			return ""
		}
	}
	return fmt.Sprintf("%q:%s:%s:%s:%d", id, usage, pricing, project, cursor)
}

func (db *DB) keptDailyUsage(ctx context.Context, filter UsageFilter) (DailyUsageResult, error) {
	if err := ctx.Err(); err != nil {
		return DailyUsageResult{}, err
	}
	slot := UsageSlot(filter, true)
	version := ""
	if slot != "" {
		epoch := db.usageKeptEpoch.Load()
		version = db.dailyUsageVersion(ctx)
		if version != "" {
			version = fmt.Sprintf("%s:%d", version, epoch)
		}
	}
	if err := ctx.Err(); err != nil {
		return DailyUsageResult{}, err
	}
	if version == "" {
		return db.getDailyUsageUncached(ctx, filter)
	}
	if rows, ok := db.usageKept.Get(slot, version); ok {
		result := rows[0].Clone()
		if filter.SkipSessionCounts {
			result.SessionCounts = UsageSessionCounts{}
		}
		return result, nil
	}
	full := filter
	full.SkipSessionCounts = false
	result, err := db.getDailyUsageUncached(ctx, full)
	if err != nil {
		return result, err
	}
	current := db.dailyUsageVersion(ctx)
	if current != "" && fmt.Sprintf("%s:%d", current, db.usageKeptEpoch.Load()) == version {
		db.usageKept.Put(slot, version, []DailyUsageResult{result})
	}
	result = result.Clone()
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
