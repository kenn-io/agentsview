package db

import (
	"context"
	"encoding/hex"
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
	if panel == "signals" {
		original := load
		load = func(ctx context.Context, f AnalyticsFilter) (T, error) {
			db.frustrationMemo.Swap(func() {})
			return original(ctx, f)
		}
	}
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
	slot := panel + "|" + string(key)
	var version string
	var savedVersion string
	if SavedPanel(panel) {
		var common, panelToken, scope, velocityJoin string
		err := db.getReader().QueryRowContext(ctx, `SELECT
 COALESCE((SELECT value FROM archive_metadata WHERE key='dashboard_common'), ''),
 COALESCE((SELECT value FROM archive_metadata WHERE key=?), ''),
 COALESCE((SELECT value FROM archive_metadata WHERE key='dashboard_scope'), ''),
 COALESCE((SELECT value FROM archive_metadata WHERE key='dashboard_velocity_join'), '')`, "dashboard_"+panel).Scan(&common, &panelToken, &scope, &velocityJoin)
		valid := func(token string) bool {
			decoded, err := hex.DecodeString(token)
			return err == nil && len(decoded) == 16
		}
		if err != nil || !valid(common) || !valid(panelToken) || (DashboardUsesMessageScope(f) && !valid(scope)) || (panel == "velocity" && f.Model != "" && !valid(velocityJoin)) {
			return load(ctx, f)
		}
		version = id + "|" + common + "|" + panelToken
		if DashboardUsesMessageScope(f) {
			version += "|" + scope
		}
		if panel == "velocity" && f.Model != "" {
			version += "|" + velocityJoin
		}
		if f.ActiveSince == "" {
			db.trendsPartsDisk()
			savedVersion = version + "|" + TrendsTimezoneKey(f)
		}
	} else {
		revision, err := db.UsageChangeRevision(ctx)
		if err != nil {
			return load(ctx, f)
		}
		analyticsRevision, err := db.archiveJournalRevision(ctx, archiveMetadataAnalyticsChangeRevisionKey, "analytics change")
		if err != nil {
			return load(ctx, f)
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
		return load(ctx, f)
	})
	if err != nil {
		var zero T
		return zero, err
	}
	return result.(T), nil
}
