package db

import (
	"context"
	"encoding/json/v2"
	"fmt"
)

type analyticsReport interface {
	ToolsAnalyticsResponse | VelocityResponse | SignalsAnalyticsResponse | TrendsTermsResponse
}

// SavedPanel identifies the Dashboard reports retained across restarts.
func SavedPanel(panel string) bool {
	return panel == "tools" || panel == "velocity" || panel == "signals"
}

func keptAnalytics[T analyticsReport](ctx context.Context, db *DB, panel string, f AnalyticsFilter, load func(context.Context, AnalyticsFilter) (T, error)) (T, error) {
	if db.readOnly || TerminationUsesTime(f.Termination) {
		return load(ctx, f)
	}
	key, err := json.Marshal(f, json.Deterministic(true))
	if err != nil {
		return load(ctx, f)
	}
	id, err := db.GetDatabaseID(ctx)
	if err != nil {
		return load(ctx, f)
	}
	revision, err := db.UsageChangeRevision(ctx)
	if err != nil {
		return load(ctx, f)
	}
	analyticsRevision, err := db.archiveJournalRevision(ctx, archiveMetadataAnalyticsChangeRevisionKey, "analytics change")
	if err != nil {
		return load(ctx, f)
	}
	slot := panel + "|" + string(key)
	version := fmt.Sprintf("%s|%d|%d", id, revision, analyticsRevision)
	var savedVersion string
	if SavedPanel(panel) && f.ActiveSince == "" {
		var generation string
		if err := db.getReader().QueryRowContext(ctx, `SELECT value FROM archive_metadata WHERE key = 'dashboard_change_generation'`).Scan(&generation); err == nil && generation != "" {
			db.trendsPartsDisk()
			version += "|" + generation + "|" + TrendsTimezoneKey(f)
			savedVersion = version
		}
	}
	result, err := db.analyticsKept.DoSaved(ctx, &db.trendsDisk, slot, version, savedVersion, func(value any) ([]byte, error) {
		return json.Marshal(value, json.FormatNilSliceAsNull(true), json.FormatNilMapAsNull(true))
	}, func(data []byte) (any, error) {
		var value T
		err := json.Unmarshal(data, &value)
		return value, err
	}, func(ctx context.Context) (any, error) {
		return load(ctx, f)
	})
	if err != nil {
		var zero T
		return zero, err
	}
	return result.(T), nil
}
