package db

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"strconv"
	"strings"
)

func keptAnalytics[T any](ctx context.Context, db *DB, panel string, f AnalyticsFilter, load func(context.Context, AnalyticsFilter) (T, error)) (T, error) {
	if db.readOnly || TerminationUsesTime(f.Termination) {
		return load(ctx, f)
	}
	// Copy pointer-valued filters before a shared load outlives its caller.
	if f.DayOfWeek != nil {
		f.DayOfWeek = new(*f.DayOfWeek)
	}
	if f.Hour != nil {
		f.Hour = new(*f.Hour)
	}
	key, err := json.Marshal(f, json.Deterministic(true))
	if err != nil {
		return load(ctx, f)
	}
	var id string
	var rawRevision string
	err = db.getReader().QueryRowContext(ctx, `SELECT
		COALESCE((SELECT value FROM archive_metadata WHERE key = ?), ''),
		COALESCE((SELECT value FROM archive_metadata WHERE key = ?), '0')`,
		archiveMetadataDatabaseIDKey, archiveMetadataUsageChangeRevisionKey).Scan(&id, &rawRevision)
	if err != nil || strings.TrimSpace(id) == "" {
		return load(ctx, f)
	}
	revision, err := strconv.ParseInt(strings.TrimSpace(rawRevision), 10, 64)
	if err != nil || revision < 0 {
		return load(ctx, f)
	}
	db.analyticsKeptOnce.Do(func() {
		db.analyticsKept.Size = func(value []byte) int64 { return int64(len(value)) }
	})
	encoded, err := db.analyticsKept.Do(ctx, panel+"|"+string(key), fmt.Sprintf("%s|%d", id, revision), func(ctx context.Context) ([]byte, error) {
		value, err := load(ctx, f)
		if err != nil {
			return nil, err
		}
		return json.Marshal(value, json.FormatNilSliceAsNull(true), json.FormatNilMapAsNull(true))
	})
	if err != nil {
		var zero T
		return zero, err
	}
	var value T
	if err := json.Unmarshal(encoded, &value); err != nil {
		return value, err
	}
	return value, nil
}
