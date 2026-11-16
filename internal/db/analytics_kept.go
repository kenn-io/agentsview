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
		var common, panelToken, scope string
		err := db.getReader().QueryRowContext(ctx, `SELECT
 COALESCE((SELECT value FROM archive_metadata WHERE key='dashboard_common'), ''),
 COALESCE((SELECT value FROM archive_metadata WHERE key=?), ''),
 COALESCE((SELECT value FROM archive_metadata WHERE key='dashboard_scope'), '')`, "dashboard_"+panel).Scan(&common, &panelToken, &scope)
		valid := func(token string) bool {
			decoded, err := hex.DecodeString(token)
			return err == nil && len(decoded) == 16
		}
		if err != nil || !valid(common) || !valid(panelToken) || (DashboardUsesMessageScope(f) && !valid(scope)) {
			return load(ctx, f)
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

// frustrationGeneration invalidates prompt counts when direct message edits
// change the Signals token without changing transcript versions.
func (db *DB) frustrationGeneration(ctx context.Context, hasRows bool) (uint64, error) {
	if !hasRows {
		return 0, nil
	}
	var token string
	err := db.getReader().QueryRowContext(ctx, `SELECT value FROM archive_metadata WHERE key='dashboard_signals'`).Scan(&token)
	decoded, decodeErr := hex.DecodeString(token)
	if err == nil && decodeErr == nil && len(decoded) == 16 {
		_, err = db.analyticsKept.Do(ctx, "\x00signals-memo", token, func(context.Context) (any, error) {
			db.frustrationMemo.Swap(func() {})
			return struct{}{}, nil
		})
		if err != nil {
			return 0, err
		}
	} else if !db.readOnly {
		db.frustrationMemo.Swap(func() {})
	}
	return db.frustrationMemo.Generation(), nil
}
