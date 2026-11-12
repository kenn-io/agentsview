package db

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
)

type analyticsReport interface {
	ToolsAnalyticsResponse | VelocityResponse | SignalsAnalyticsResponse
}

func keptAnalytics[T analyticsReport](ctx context.Context, db *DB, panel string, f AnalyticsFilter, load func(context.Context, AnalyticsFilter) (T, error)) (T, error) {
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
	var rawRevision, rawAnalyticsRevision string
	err = db.getReader().QueryRowContext(ctx, `SELECT
		COALESCE((SELECT value FROM archive_metadata WHERE key = ?), ''),
		COALESCE((SELECT value FROM archive_metadata WHERE key = ?), '0'),
		COALESCE((SELECT value FROM archive_metadata WHERE key = ?), '0')`,
		archiveMetadataDatabaseIDKey, archiveMetadataUsageChangeRevisionKey, archiveMetadataAnalyticsChangeRevisionKey).Scan(&id, &rawRevision, &rawAnalyticsRevision)
	if err != nil || strings.TrimSpace(id) == "" {
		return load(ctx, f)
	}
	revision, err := strconv.ParseInt(strings.TrimSpace(rawRevision), 10, 64)
	if err != nil {
		return load(ctx, f)
	}
	analyticsRevision, err := strconv.ParseInt(strings.TrimSpace(rawAnalyticsRevision), 10, 64)
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
	return cloneAnalytics(result.(T)), nil
}

func cloneAnalytics[T analyticsReport](value T) T {
	switch result := any(value).(type) {
	case ToolsAnalyticsResponse:
		result.ByCategory = slices.Clone(result.ByCategory)
		result.ByAgent = slices.Clone(result.ByAgent)
		for i := range result.ByAgent {
			result.ByAgent[i].Categories = slices.Clone(result.ByAgent[i].Categories)
		}
		result.ByTool = slices.Clone(result.ByTool)
		for i := range result.ByTool {
			tool := &result.ByTool[i]
			tool.EmptyRate = cloneAnalyticsFloat(tool.EmptyRate)
			tool.RepeatRate = cloneAnalyticsFloat(tool.RepeatRate)
			tool.RecoveryRate = cloneAnalyticsFloat(tool.RecoveryRate)
		}
		result.Trend = slices.Clone(result.Trend)
		for i := range result.Trend {
			result.Trend[i].ByCat = maps.Clone(result.Trend[i].ByCat)
		}
		return any(result).(T)
	case VelocityResponse:
		result.ByAgent = slices.Clone(result.ByAgent)
		result.ByComplexity = slices.Clone(result.ByComplexity)
		return any(result).(T)
	case SignalsAnalyticsResponse:
		result.GradeDistribution = maps.Clone(result.GradeDistribution)
		result.OutcomeDistribution = maps.Clone(result.OutcomeDistribution)
		result.OutcomeConfidenceDistribution = maps.Clone(result.OutcomeConfidenceDistribution)
		result.AvgHealthScore = cloneAnalyticsFloat(result.AvgHealthScore)
		result.ContextHealth.AvgContextPressure = cloneAnalyticsFloat(result.ContextHealth.AvgContextPressure)
		result.Trend = slices.Clone(result.Trend)
		for i := range result.Trend {
			result.Trend[i].AvgHealthScore = cloneAnalyticsFloat(result.Trend[i].AvgHealthScore)
		}
		result.ByAgent = slices.Clone(result.ByAgent)
		for i := range result.ByAgent {
			result.ByAgent[i].AvgHealthScore = cloneAnalyticsFloat(result.ByAgent[i].AvgHealthScore)
		}
		result.ByProject = slices.Clone(result.ByProject)
		for i := range result.ByProject {
			result.ByProject[i].AvgHealthScore = cloneAnalyticsFloat(result.ByProject[i].AvgHealthScore)
		}
		result.Calibration = maps.Clone(result.Calibration)
		for key, calibration := range result.Calibration {
			calibration.IncompleteLift = cloneAnalyticsFloat(calibration.IncompleteLift)
			calibration.AvgScoreDelta = cloneAnalyticsFloat(calibration.AvgScoreDelta)
			result.Calibration[key] = calibration
		}
		return any(result).(T)
	default:
		panic("unsupported analytics report")
	}
}

func cloneAnalyticsFloat(value *float64) *float64 {
	if value == nil {
		return nil
	}
	return new(*value)
}
