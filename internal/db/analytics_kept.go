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
	return keptAnalyticsWithIdentity(ctx, db, panel, f, func(ctx context.Context, f AnalyticsFilter, _ string) (T, error) {
		return load(ctx, f)
	})
}

func keptAnalyticsWithIdentity[T analyticsReport](ctx context.Context, db *DB, panel string, f AnalyticsFilter, load func(context.Context, AnalyticsFilter, string) (T, error)) (T, error) {
	if db.readOnly || TerminationUsesTime(f.Termination) {
		return load(ctx, f, "")
	}
	key, err := json.Marshal(f, json.Deterministic(true))
	if err != nil {
		return load(ctx, f, "")
	}
	id, err := db.GetDatabaseID(ctx)
	if err != nil {
		return load(ctx, f, "")
	}
	slot := panel + "|" + string(key)
	var version, savedVersion string
	if SavedPanel(panel) {
		var common, panelToken, scope string
		err := db.getReader().QueryRowContext(ctx, `SELECT
 COALESCE((SELECT value FROM archive_metadata WHERE key='dashboard_common'), ''),
 COALESCE((SELECT value FROM archive_metadata WHERE key=?), ''),
 COALESCE((SELECT value FROM archive_metadata WHERE key='dashboard_scope'), '')`, "dashboard_"+panel).Scan(&common, &panelToken, &scope)
		if err != nil || common == "" || panelToken == "" || (DashboardUsesMessageScope(f) && scope == "") {
			return load(ctx, f, id)
		}
		version = id + "|" + common + "|" + panelToken
		if DashboardUsesMessageScope(f) {
			version += "|" + scope
		}
		if f.ActiveSince == "" {
			db.trendsPartsDisk()
			savedVersion = version + "|" + TrendsTimezoneKey(f)
		}
	} else {
		revision, err := db.UsageChangeRevision(ctx)
		if err != nil {
			return load(ctx, f, id)
		}
		analyticsRevision, err := db.archiveJournalRevision(ctx, archiveMetadataAnalyticsChangeRevisionKey, "analytics change")
		if err != nil {
			return load(ctx, f, id)
		}
		version = fmt.Sprintf("%s|%d|%d", id, revision, analyticsRevision)
	}
	result, err := db.analyticsKept.DoSaved(ctx, &db.trendsDisk, slot, version, savedVersion, func(value any) ([]byte, error) {
		return json.Marshal(value, json.FormatNilSliceAsNull(true), json.FormatNilMapAsNull(true))
	}, func(data []byte) (any, error) {
		var value T
		err := json.Unmarshal(data, &value)
		return value, err
	}, func(ctx context.Context) (any, error) {
		return load(ctx, f, id)
	})
	if err != nil {
		var zero T
		return zero, err
	}
	return result.(T), nil
}
