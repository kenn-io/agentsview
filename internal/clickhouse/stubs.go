package clickhouse

import (
	"context"
	"fmt"

	"go.kenn.io/agentsview/internal/activity"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/export"
)

func notImplemented(method string) error {
	return fmt.Errorf("clickhouse: %s is not implemented yet: %w", method, errNotImplemented)
}

func (s *Store) InsertInsight(_ db.Insight) (int64, error) { return 0, db.ErrReadOnly }
func (s *Store) DeleteInsight(_ int64) error               { return db.ErrReadOnly }
func (s *Store) ListInsights(_ context.Context, _ db.InsightFilter) ([]db.Insight, error) {
	return []db.Insight{}, nil
}
func (s *Store) GetInsight(_ context.Context, _ int64) (*db.Insight, error) { return nil, nil }
func (s *Store) GetCachedInsight(_ context.Context, _ string) (*db.Insight, error) {
	return nil, nil
}

func (s *Store) RenameSession(_ string, _ *string) error        { return db.ErrReadOnly }
func (s *Store) SoftDeleteSession(_ string) error               { return db.ErrReadOnly }
func (s *Store) SoftDeleteSessions(_ []string) (int, error)     { return 0, db.ErrReadOnly }
func (s *Store) RestoreSession(_ string) (int64, error)         { return 0, db.ErrReadOnly }
func (s *Store) DeleteSessionIfTrashed(_ string) (int64, error) { return 0, db.ErrReadOnly }
func (s *Store) EmptyTrash() (int, error)                       { return 0, db.ErrReadOnly }
func (s *Store) UpsertSession(_ db.Session) error               { return db.ErrReadOnly }
func (s *Store) ReplaceSessionMessages(_ string, _ []db.Message) error {
	return db.ErrReadOnly
}
func (s *Store) WriteSessionBatchAtomic(
	_ []db.SessionBatchWrite, _ ...func() error,
) (db.SessionBatchResult, error) {
	return db.SessionBatchResult{}, db.ErrReadOnly
}

func (s *Store) ListProjectIdentityObservations(
	_ context.Context, _ []string,
) ([]export.ProjectIdentityObservation, error) {
	return nil, notImplemented("ListProjectIdentityObservations")
}

func (s *Store) BuildProjectIdentityMap(
	_ context.Context, _ []string,
) (map[string]export.ProjectMapEntry, error) {
	return nil, notImplemented("BuildProjectIdentityMap")
}

func (s *Store) GetProjectInventory(
	_ context.Context, _ db.ProjectDateFilter,
) (db.ProjectInventory, error) {
	return db.ProjectInventory{}, notImplemented("GetProjectInventory")
}

func (s *Store) ListProjectRules(_ context.Context, _ string) (db.ProjectRules, error) {
	return db.ProjectRules{}, notImplemented("ListProjectRules")
}

func (s *Store) ListArchiveWorktreeCandidates(
	_ context.Context, _ db.ArchiveWorktreeCandidateRequest,
) ([]db.WorktreeReclassificationCandidate, error) {
	return nil, notImplemented("ListArchiveWorktreeCandidates")
}

func (s *Store) GetAnalyticsSummary(
	_ context.Context, _ db.AnalyticsFilter,
) (db.AnalyticsSummary, error) {
	return db.AnalyticsSummary{}, notImplemented("GetAnalyticsSummary")
}

func (s *Store) GetAnalyticsActivity(
	_ context.Context, _ db.AnalyticsFilter, _ string,
) (db.ActivityResponse, error) {
	return db.ActivityResponse{}, notImplemented("GetAnalyticsActivity")
}

func (s *Store) GetAnalyticsHeatmap(
	_ context.Context, _ db.AnalyticsFilter, _ string,
) (db.HeatmapResponse, error) {
	return db.HeatmapResponse{}, notImplemented("GetAnalyticsHeatmap")
}

func (s *Store) GetAnalyticsProjects(
	_ context.Context, _ db.AnalyticsFilter,
) (db.ProjectsAnalyticsResponse, error) {
	return db.ProjectsAnalyticsResponse{}, notImplemented("GetAnalyticsProjects")
}

func (s *Store) GetAnalyticsHourOfWeek(
	_ context.Context, _ db.AnalyticsFilter,
) (db.HourOfWeekResponse, error) {
	return db.HourOfWeekResponse{}, notImplemented("GetAnalyticsHourOfWeek")
}

func (s *Store) GetAnalyticsSessionShape(
	_ context.Context, _ db.AnalyticsFilter,
) (db.SessionShapeResponse, error) {
	return db.SessionShapeResponse{}, notImplemented("GetAnalyticsSessionShape")
}

func (s *Store) GetAnalyticsTools(
	_ context.Context, _ db.AnalyticsFilter,
) (db.ToolsAnalyticsResponse, error) {
	return db.ToolsAnalyticsResponse{}, notImplemented("GetAnalyticsTools")
}

func (s *Store) GetAnalyticsSkills(
	_ context.Context, _ db.AnalyticsFilter, _ string,
) (db.SkillsAnalyticsResponse, error) {
	return db.SkillsAnalyticsResponse{}, notImplemented("GetAnalyticsSkills")
}

func (s *Store) GetAnalyticsVelocity(
	_ context.Context, _ db.AnalyticsFilter,
) (db.VelocityResponse, error) {
	return db.VelocityResponse{}, notImplemented("GetAnalyticsVelocity")
}

func (s *Store) GetAnalyticsTopSessions(
	_ context.Context, _ db.AnalyticsFilter, _ string,
) (db.TopSessionsResponse, error) {
	return db.TopSessionsResponse{}, notImplemented("GetAnalyticsTopSessions")
}

func (s *Store) GetAnalyticsSignals(
	_ context.Context, _ db.AnalyticsFilter,
) (db.SignalsAnalyticsResponse, error) {
	return db.SignalsAnalyticsResponse{}, notImplemented("GetAnalyticsSignals")
}

func (s *Store) GetAnalyticsSignalSessions(
	_ context.Context, _ db.AnalyticsFilter, _ string, _ int,
) (db.SignalSessionsResponse, error) {
	return db.SignalSessionsResponse{}, notImplemented("GetAnalyticsSignalSessions")
}

func (s *Store) GetTrendsTerms(
	_ context.Context, _ db.AnalyticsFilter, _ []db.TrendTermInput, _ string,
) (db.TrendsTermsResponse, error) {
	return db.TrendsTermsResponse{}, notImplemented("GetTrendsTerms")
}

func (s *Store) GetActivityReport(
	_ context.Context, _ db.AnalyticsFilter, _ activity.Query,
) (activity.Report, error) {
	return activity.Report{}, notImplemented("GetActivityReport")
}

func (s *Store) RecentEdits(_ context.Context, _ db.RecentEditsParams) (db.RecentEditsResult, error) {
	return db.RecentEditsResult{}, notImplemented("RecentEdits")
}

func (s *Store) GetDailyUsage(_ context.Context, _ db.UsageFilter) (db.DailyUsageResult, error) {
	return db.DailyUsageResult{}, notImplemented("GetDailyUsage")
}

func (s *Store) GetTopSessionsByCost(
	_ context.Context, _ db.UsageFilter, _ int,
) ([]db.TopSessionEntry, error) {
	return nil, notImplemented("GetTopSessionsByCost")
}

func (s *Store) GetUsageSessionCounts(
	_ context.Context, _ db.UsageFilter,
) (db.UsageSessionCounts, error) {
	return db.UsageSessionCounts{}, notImplemented("GetUsageSessionCounts")
}

func (s *Store) GetUsageMatchingSessionCount(_ context.Context, _ db.UsageFilter) (int, error) {
	return 0, notImplemented("GetUsageMatchingSessionCount")
}

func (s *Store) GetSessionUsage(
	_ context.Context, _ string, _ bool,
) (*db.SessionUsage, error) {
	return nil, notImplemented("GetSessionUsage")
}
