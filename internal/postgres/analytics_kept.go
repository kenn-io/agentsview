package postgres

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"go.kenn.io/agentsview/internal/db"
)

func keptAnalytics[T any](ctx context.Context, s *Store, panel string, f db.AnalyticsFilter, load func(context.Context, db.AnalyticsFilter) (T, error)) (T, error) {
	return keptAnalyticsWith(ctx, s, panel, f, s.keptAnalyticsVersion, load)
}

// keptAnalyticsVersion is the archive's report version. A standby reads fresh,
// and a primary without a usage cache falls back to the direct change revision.
func (s *Store) keptAnalyticsVersion(ctx context.Context, slot string) string {
	version := s.keptUsageVersion(ctx, slot)
	if version == standbyUsageMarker {
		return ""
	}
	if version == "" && s.readTx == nil && !s.hostedRelations {
		var standby bool
		if err := s.pg.QueryRowContext(ctx, `SELECT pg_is_in_recovery()`).Scan(&standby); err == nil && !standby {
			version = s.directUsageVersion(ctx, slot)
		}
	}
	return version
}

func keptAnalyticsWith[T any](ctx context.Context, s *Store, panel string, f db.AnalyticsFilter, versionFor func(context.Context, string) string, load func(context.Context, db.AnalyticsFilter) (T, error)) (T, error) {
	if f.ActiveSince != "" || f.Timezone == "Local" || db.TerminationUsesTime(f.Termination) {
		return load(ctx, f)
	}
	key, err := json.Marshal(f)
	if err != nil {
		return load(ctx, f)
	}
	slot := panel + "|" + string(key)
	version := versionFor(ctx, slot)
	if version == "" {
		return load(ctx, f)
	}
	var savedVersion string
	if panelVersion := s.keptDashboardVersion(ctx, panel, f); panelVersion != "" {
		version, savedVersion = panelVersion, panelVersion+"|"+db.TrendsTimezoneKey(f)
	}
	encoded, err := s.runtimeState().analytics.DoSaved(ctx, &s.trendsDisk, slot, version, savedVersion, func(b []byte) ([]byte, error) { return b, nil }, func(b []byte) ([]byte, error) {
		var result T
		return b, json.Unmarshal(b, &result)
	}, func(ctx context.Context) ([]byte, error) {
		result, err := load(ctx, f)
		if err != nil {
			return nil, err
		}
		return json.Marshal(result, json.FormatNilSliceAsNull(true), json.FormatNilMapAsNull(true))
	})
	var result T
	if err != nil {
		return result, err
	}
	if err := json.Unmarshal(encoded, &result); err != nil {
		return result, fmt.Errorf("decoding kept analytics: %w", err)
	}
	return result, nil
}

// keptDashboardVersion names a saved Dashboard panel by the random tokens its
// inputs rotate, so unrelated writes leave it reusable.
func (s *Store) keptDashboardVersion(ctx context.Context, panel string, f db.AnalyticsFilter) string {
	if !db.SavedPanel(panel) || s.readTx != nil || s.hostedRelations {
		return ""
	}
	var common, token, scope string
	err := s.pg.QueryRowContext(ctx, `SELECT
 COALESCE((SELECT value FROM sync_metadata WHERE key='dashboard_common'),''),
 COALESCE((SELECT value FROM sync_metadata WHERE key=$1),''),
 COALESCE((SELECT value FROM sync_metadata WHERE key='dashboard_scope'),'')
 WHERE NOT pg_is_in_recovery() AND EXISTS (SELECT 1 FROM sync_metadata WHERE key='usage_change_journal_version' AND value=$2)`, "dashboard_"+panel, strconv.Itoa(usageChangeJournalVersionPG)).Scan(&common, &token, &scope)
	if err != nil {
		return ""
	}
	tokens := []string{common, token}
	if db.DashboardUsesMessageScope(f) {
		tokens = append(tokens, scope)
	}
	if slices.Contains(tokens, "") {
		return ""
	}
	return strings.Join(tokens, "|")
}

// frustrationGeneration invalidates prompt counts when message edits change
// the Signals token without changing session versions.
func (s *Store) frustrationGeneration(ctx context.Context, hasRows bool) (uint64, error) {
	if !hasRows {
		return 0, nil
	}
	var token string
	err := s.pg.QueryRowContext(ctx, `SELECT value FROM sync_metadata WHERE key='dashboard_signals'`).Scan(&token)
	if err == nil && token != "" {
		_, err = s.runtimeState().analytics.Do(ctx, "\x00signals-memo", token, func(context.Context) ([]byte, error) {
			s.runtimeState().frustrationMarkers.Swap(func() {})
			return []byte{}, nil
		})
		if err != nil {
			return 0, err
		}
	}
	return s.runtimeState().frustrationMarkers.Generation(), nil
}
