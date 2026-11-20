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
	return keptAnalyticsMode(ctx, s, panel, f, versionFor, load, true)
}

func keptAnalyticsMode[T any](ctx context.Context, s *Store, panel string, f db.AnalyticsFilter, versionFor func(context.Context, string) string, load func(context.Context, db.AnalyticsFilter) (T, error), share bool) (T, error) {
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
	if !share {
		var result T
		if err := ctx.Err(); err != nil {
			return result, err
		}
		cache := &s.runtimeState().analytics
		if encoded, ok := cache.Get(slot, version); ok && json.Unmarshal(encoded, &result) == nil {
			return result, nil
		}
		if savedVersion != "" {
			if encoded, ok := s.trendsDisk.Get(slot, savedVersion); ok && json.Unmarshal(encoded, &result) == nil {
				cache.Put(slot, version, encoded)
				return result, nil
			}
		}
		result, err := load(ctx, f)
		if err != nil {
			return result, err
		}
		if encoded, err := json.Marshal(result, json.FormatNilSliceAsNull(true), json.FormatNilMapAsNull(true)); err == nil {
			cache.Put(slot, version, encoded)
			if savedVersion != "" {
				s.trendsDisk.Put(slot, savedVersion, encoded)
			}
		}
		return result, nil
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
	if !db.SavedPanel(panel) {
		return ""
	}
	var common, token, scope string
	err := s.pg.QueryRowContext(ctx, `SELECT
 COALESCE((SELECT value FROM sync_metadata WHERE key='dashboard_common'),''),
 COALESCE((SELECT value FROM sync_metadata WHERE key=$1),''),
 COALESCE((SELECT value FROM sync_metadata WHERE key='dashboard_scope'),'')
 WHERE EXISTS (SELECT 1 FROM sync_metadata WHERE key='usage_change_journal_version' AND value=$2)`, "dashboard_"+panel, strconv.Itoa(usageChangeJournalVersionPG)).Scan(&common, &token, &scope)
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
