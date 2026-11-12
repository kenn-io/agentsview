package postgres

import (
	"context"
	"encoding/json/v2"
	"fmt"

	"go.kenn.io/agentsview/internal/db"
)

func keptAnalytics[T any](ctx context.Context, s *Store, panel string, f db.AnalyticsFilter, load func(context.Context, db.AnalyticsFilter) (T, error)) (T, error) {
	if f.ActiveSince != "" || f.Timezone == "Local" || db.TerminationUsesTime(f.Termination) {
		return load(ctx, f)
	}
	key, err := json.Marshal(f)
	if err != nil {
		return load(ctx, f)
	}
	slot := panel + "|" + string(key)
	version := s.keptUsageVersion(ctx, slot)
	if version == "" {
		return load(ctx, f)
	}
	encoded, err := s.runtimeState().analytics.Do(ctx, slot, version, func(ctx context.Context) ([]byte, error) {
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
