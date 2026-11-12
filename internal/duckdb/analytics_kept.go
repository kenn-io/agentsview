package duckdb

import (
	"context"
	"encoding/json/v2"
	"fmt"

	"go.kenn.io/agentsview/internal/db"
)

func keptAnalytics[T any](ctx context.Context, s *Store, panel string, f db.AnalyticsFilter, granularity string, load func(context.Context) (T, error)) (T, error) {
	version := s.keptVersion()
	if version == "" || db.TerminationUsesTime(f.Termination) {
		return load(ctx)
	}
	key, err := json.Marshal(struct {
		Panel       string
		Filter      db.AnalyticsFilter
		Granularity string
	}{panel, f, granularity})
	if err != nil {
		var zero T
		return zero, fmt.Errorf("encoding analytics selection: %w", err)
	}
	encoded, err := s.analyticsReads.Do(ctx, string(key), version, func(ctx context.Context) ([]byte, error) {
		value, err := load(ctx)
		if err != nil {
			return nil, err
		}
		return json.Marshal(value, json.FormatNilSliceAsNull(true), json.FormatNilMapAsNull(true))
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
