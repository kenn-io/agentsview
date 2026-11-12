package db

import (
	"context"
	"encoding/json/v2"
	"fmt"
)

type analyticsReport interface {
	ToolsAnalyticsResponse | VelocityResponse | SignalsAnalyticsResponse
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
	result, err := db.analyticsKept.Do(ctx, panel+"|"+string(key), fmt.Sprintf("%s|%d|%d", id, revision, analyticsRevision), func(ctx context.Context) (any, error) {
		return load(ctx, f)
	})
	if err != nil {
		var zero T
		return zero, err
	}
	return result.(T), nil
}
