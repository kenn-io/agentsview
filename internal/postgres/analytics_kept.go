package postgres

import (
	"context"
	"encoding/json/v2"
	"fmt"

	"go.kenn.io/agentsview/internal/db"
)

func keptAnalytics[T any](ctx context.Context, s *Store, panel string, f db.AnalyticsFilter, load func(context.Context, db.AnalyticsFilter) (T, error)) (T, error) {
	return keptAnalyticsWith(ctx, s, panel, f, s.keptAnalyticsVersion, load)
}

// keptAnalyticsVersion is the archive's report version. A standby reads fresh,
// and a primary without a usage cache falls back to the direct change revision.
func (s *Store) keptAnalyticsVersion(ctx context.Context, slot string) (string, string) {
	version := s.keptUsageDataVersion(ctx, slot)
	if version == standbyUsageMarker {
		return "", ""
	}
	if version == "" && s.readTx == nil && !s.hostedRelations {
		var standby bool
		if err := s.pg.QueryRowContext(ctx, `SELECT pg_is_in_recovery()`).Scan(&standby); err == nil && !standby {
			return s.directUsageVersion(ctx, slot), ""
		}
	}
	if version == "" {
		return "", ""
	}
	runtime := s.runtimeState()
	runtime.pricingMu.Lock()
	defer runtime.pricingMu.Unlock()
	return fmt.Sprintf("%s|%d", version, runtime.pricingGeneration), version
}

func keptAnalyticsWith[T any](ctx context.Context, s *Store, panel string, f db.AnalyticsFilter, versionFor func(context.Context, string) (string, string), load func(context.Context, db.AnalyticsFilter) (T, error)) (T, error) {
	if panel == "signals" {
		original := load
		load = func(ctx context.Context, f db.AnalyticsFilter) (T, error) {
			s.runtimeState().frustrationMarkers.Swap(func() {})
			return original(ctx, f)
		}
	}
	if f.ActiveSince != "" || f.Timezone == "Local" || db.TerminationUsesTime(f.Termination) {
		return load(ctx, f)
	}
	key, err := json.Marshal(f)
	if err != nil {
		return load(ctx, f)
	}
	slot := panel + "|" + string(key)
	version, savedVersion := versionFor(ctx, slot)
	if db.SavedPanel(panel) && version != "" && !s.hostedRelations && s.readTx == nil {
		panelVersion := s.keptDashboardVersion(ctx, panel, f)
		if panelVersion != "" {
			version, savedVersion = panelVersion, panelVersion
		} else {
			savedVersion = ""
		}
	} else {
		savedVersion = ""
	}
	if savedVersion != "" {
		savedVersion += "|" + db.TrendsTimezoneKey(f)
	}
	if version == "" {
		return load(ctx, f)
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
