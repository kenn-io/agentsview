package postgres

import (
	"context"
	"io"
	"time"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/export"
)

type semanticAvailability interface {
	SemanticAvailable(context.Context) (bool, error)
}

func (h *HostedStore) HasFTS() bool { return h.physical.HasFTS() }
func (h *HostedStore) HasSemantic() bool {
	searcher := h.physical.getVectorSearcher()
	if searcher == nil {
		return false
	}
	availability, ok := searcher.(semanticAvailability)
	if !ok {
		return true
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	available, err := availability.SemanticAvailable(ctx)
	return err == nil && available
}
func (h *HostedStore) GetStats(ctx context.Context, excludeOneShot, excludeAutomated bool) (db.Stats, error) {
	return h.physical.GetStats(ctx, excludeOneShot, excludeAutomated)
}
func (h *HostedStore) GetProjects(ctx context.Context, excludeOneShot, excludeAutomated bool) ([]db.ProjectInfo, error) {
	return h.physical.GetProjects(ctx, excludeOneShot, excludeAutomated)
}
func (h *HostedStore) GetActiveProjectLabels(ctx context.Context) ([]string, error) {
	return h.physical.GetActiveProjectLabels(ctx)
}
func (h *HostedStore) GetAgents(ctx context.Context, excludeOneShot, excludeAutomated bool) ([]db.AgentInfo, error) {
	return h.physical.GetAgents(ctx, excludeOneShot, excludeAutomated)
}
func (h *HostedStore) GetMachines(ctx context.Context, excludeOneShot, excludeAutomated bool) ([]string, error) {
	return h.physical.GetMachines(ctx, excludeOneShot, excludeAutomated)
}
func (h *HostedStore) GetBranches(ctx context.Context, excludeOneShot, excludeAutomated bool) ([]db.BranchInfo, error) {
	return h.physical.GetBranches(ctx, excludeOneShot, excludeAutomated)
}
func (h *HostedStore) ListProjectIdentityObservations(ctx context.Context, labels []string) ([]export.ProjectIdentityObservation, error) {
	return h.physical.ListProjectIdentityObservations(ctx, labels)
}
func (h *HostedStore) BuildProjectIdentityMap(ctx context.Context, labels []string) (map[string]export.ProjectMapEntry, error) {
	return h.physical.BuildProjectIdentityMap(ctx, labels)
}
func (h *HostedStore) GetProjectInventory(ctx context.Context) (db.ProjectInventory, error) {
	return h.physical.GetProjectInventory(ctx)
}
func (h *HostedStore) ListProjectRules(ctx context.Context, machine string) (db.ProjectRules, error) {
	return h.physical.ListProjectRules(ctx, machine)
}
func (h *HostedStore) GetAnalyticsSummary(ctx context.Context, f db.AnalyticsFilter) (db.AnalyticsSummary, error) {
	return h.physical.GetAnalyticsSummary(ctx, f)
}
func (h *HostedStore) GetAnalyticsActivity(ctx context.Context, f db.AnalyticsFilter, granularity string) (db.ActivityResponse, error) {
	return h.physical.GetAnalyticsActivity(ctx, f, granularity)
}
func (h *HostedStore) GetAnalyticsHeatmap(ctx context.Context, f db.AnalyticsFilter, metric string) (db.HeatmapResponse, error) {
	return h.physical.GetAnalyticsHeatmap(ctx, f, metric)
}
func (h *HostedStore) GetAnalyticsProjects(ctx context.Context, f db.AnalyticsFilter) (db.ProjectsAnalyticsResponse, error) {
	return h.physical.GetAnalyticsProjects(ctx, f)
}
func (h *HostedStore) GetAnalyticsHourOfWeek(ctx context.Context, f db.AnalyticsFilter) (db.HourOfWeekResponse, error) {
	return h.physical.GetAnalyticsHourOfWeek(ctx, f)
}
func (h *HostedStore) GetAnalyticsSessionShape(ctx context.Context, f db.AnalyticsFilter) (db.SessionShapeResponse, error) {
	return h.physical.GetAnalyticsSessionShape(ctx, f)
}
func (h *HostedStore) GetAnalyticsTools(ctx context.Context, f db.AnalyticsFilter) (db.ToolsAnalyticsResponse, error) {
	return h.physical.GetAnalyticsTools(ctx, f)
}
func (h *HostedStore) GetAnalyticsSkills(ctx context.Context, f db.AnalyticsFilter, granularity string) (db.SkillsAnalyticsResponse, error) {
	return h.physical.GetAnalyticsSkills(ctx, f, granularity)
}
func (h *HostedStore) GetAnalyticsVelocity(ctx context.Context, f db.AnalyticsFilter) (db.VelocityResponse, error) {
	return h.physical.GetAnalyticsVelocity(ctx, f)
}
func (h *HostedStore) GetAnalyticsSignals(ctx context.Context, f db.AnalyticsFilter) (db.SignalsAnalyticsResponse, error) {
	return h.physical.GetAnalyticsSignals(ctx, f)
}
func (h *HostedStore) GetTrendsTerms(ctx context.Context, f db.AnalyticsFilter, terms []db.TrendTermInput, granularity string) (db.TrendsTermsResponse, error) {
	return h.physical.GetTrendsTerms(ctx, f, terms, granularity)
}
func (h *HostedStore) GetDailyUsage(ctx context.Context, f db.UsageFilter) (db.DailyUsageResult, error) {
	return h.physical.GetDailyUsage(ctx, f)
}
func (h *HostedStore) GetUsageSessionCounts(ctx context.Context, f db.UsageFilter) (db.UsageSessionCounts, error) {
	return h.physical.GetUsageSessionCounts(ctx, f)
}
func (h *HostedStore) GetUsageMatchingSessionCount(ctx context.Context, f db.UsageFilter) (int, error) {
	return h.physical.GetUsageMatchingSessionCount(ctx, f)
}
func (h *HostedStore) ListInsights(ctx context.Context, f db.InsightFilter) ([]db.Insight, error) {
	return h.physical.ListInsights(ctx, f)
}
func (h *HostedStore) GetInsight(ctx context.Context, id int64) (*db.Insight, error) {
	return h.physical.GetInsight(ctx, id)
}
func (h *HostedStore) GetCachedInsight(ctx context.Context, cacheKey string) (*db.Insight, error) {
	return h.physical.GetCachedInsight(ctx, cacheKey)
}
func (h *HostedStore) InsertInsight(s db.Insight) (int64, error) { return h.physical.InsertInsight(s) }
func (h *HostedStore) DeleteInsight(id int64) error              { return h.physical.DeleteInsight(id) }
func (h *HostedStore) ListRecallEntries(ctx context.Context, q db.RecallQuery) ([]db.RecallEntry, error) {
	return h.physical.ListRecallEntries(ctx, q)
}
func (h *HostedStore) GetRecallEntry(ctx context.Context, id string) (*db.RecallEntry, error) {
	return h.physical.GetRecallEntry(ctx, id)
}
func (h *HostedStore) QueryRecallEntries(ctx context.Context, q db.RecallQuery) (db.RecallPage, error) {
	return h.physical.QueryRecallEntries(ctx, q)
}
func (h *HostedStore) RecordRecallQueryEvent(
	ctx context.Context, event db.RecallQueryEvent,
) (string, error) {
	return h.physical.RecordRecallQueryEvent(ctx, event)
}
func (h *HostedStore) InsertRecallEntry(m db.RecallEntry) (string, error) {
	return h.physical.InsertRecallEntry(m)
}
func (h *HostedStore) ImportAcceptedRecallEntriesJSONL(ctx context.Context, r io.Reader) (db.RecallImportResult, error) {
	return h.physical.ImportAcceptedRecallEntriesJSONL(ctx, r)
}
func (h *HostedStore) ImportAcceptedRecallEntriesJSONLWithOptions(
	ctx context.Context, r io.Reader, opts db.RecallImportOptions,
) (db.RecallImportResult, error) {
	return h.physical.ImportAcceptedRecallEntriesJSONLWithOptions(ctx, r, opts)
}
func (h *HostedStore) IngestEvalTrajectory(
	ctx context.Context, in db.EvalTrajectoryIngest,
) (db.EvalTrajectoryIngestResult, error) {
	return h.physical.IngestEvalTrajectory(ctx, in)
}
func (h *HostedStore) UpsertSession(s db.Session) error { return h.physical.UpsertSession(s) }
func (h *HostedStore) ReplaceSessionMessages(sessionID string, msgs []db.Message) error {
	return h.physical.ReplaceSessionMessages(sessionID, msgs)
}
func (h *HostedStore) WriteSessionBatchAtomic(
	writes []db.SessionBatchWrite,
	beforeCommit ...func() error,
) (db.SessionBatchResult, error) {
	return h.physical.WriteSessionBatchAtomic(writes, beforeCommit...)
}
func (h *HostedStore) ReadOnly() bool { return h.physical.ReadOnly() }
