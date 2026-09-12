package storetest

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"
	"go.kenn.io/agentsview/internal/activity"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/db/bunmodel"
)

// AnalyticsStore is the Task 8 analytics/reporting surface owned by BunStore.
type AnalyticsStore interface {
	GetAnalyticsSummary(context.Context, db.AnalyticsFilter) (db.AnalyticsSummary, error)
	GetAnalyticsActivity(context.Context, db.AnalyticsFilter, string) (db.ActivityResponse, error)
	GetAnalyticsHeatmap(context.Context, db.AnalyticsFilter, string) (db.HeatmapResponse, error)
	GetAnalyticsProjects(context.Context, db.AnalyticsFilter) (db.ProjectsAnalyticsResponse, error)
	GetAnalyticsHourOfWeek(context.Context, db.AnalyticsFilter) (db.HourOfWeekResponse, error)
	GetAnalyticsSessionShape(context.Context, db.AnalyticsFilter) (db.SessionShapeResponse, error)
	GetAnalyticsTools(context.Context, db.AnalyticsFilter) (db.ToolsAnalyticsResponse, error)
	GetAnalyticsSkills(context.Context, db.AnalyticsFilter, string) (db.SkillsAnalyticsResponse, error)
	GetAnalyticsVelocity(context.Context, db.AnalyticsFilter) (db.VelocityResponse, error)
	GetAnalyticsTopSessions(context.Context, db.AnalyticsFilter, string) (db.TopSessionsResponse, error)
	GetAnalyticsSignals(context.Context, db.AnalyticsFilter) (db.SignalsAnalyticsResponse, error)
	GetAnalyticsSignalSessions(context.Context, db.AnalyticsFilter, string, int) (db.SignalSessionsResponse, error)
	GetTrendsTerms(context.Context, db.AnalyticsFilter, []db.TrendTermInput, string) (db.TrendsTermsResponse, error)
	GetActivityReport(context.Context, db.AnalyticsFilter, activity.Query) (activity.Report, error)
	RecentEdits(context.Context, db.RecentEditsParams) (db.RecentEditsResult, error)
}

type AnalyticsBackend struct {
	Name string
	Open func(*testing.T) AnalyticsStore
}

// RunAnalyticsContract protects the literal cross-engine behavior of every
// Task 8 Store method. The fixture intentionally spans projects, agents,
// hours, automation states, outcomes, tools, skills, and edits.
func RunAnalyticsContract(t *testing.T, backend AnalyticsBackend) {
	t.Helper()
	t.Run(backend.Name, func(t *testing.T) {
		store := backend.Open(t)
		filter := db.AnalyticsFilter{
			From: "2026-08-02", To: "2026-08-02", Timezone: "UTC",
		}

		assertAnalyticsSummary(t, store, filter)
		assertAnalyticsTimeline(t, store, filter)
		assertAnalyticsBreakdowns(t, store, filter)
		assertAnalyticsSignals(t, store, filter)
		assertAnalyticsReports(t, store, filter)
	})
}

func assertAnalyticsSummary(t *testing.T, store AnalyticsStore, filter db.AnalyticsFilter) {
	t.Helper()
	got, err := store.GetAnalyticsSummary(t.Context(), filter)
	require.NoError(t, err)
	assert.Equal(t, 3, got.TotalSessions)
	assert.Equal(t, 7, got.TotalMessages)
	assert.Equal(t, 35, got.TotalOutputTokens)
	assert.Equal(t, 3, got.TokenReportingSessions)
	assert.Equal(t, []string{"model-a", "model-b"}, got.Models)
	assert.Equal(t, 2, got.ActiveProjects)
	assert.Equal(t, 1, got.ActiveDays)
	assert.Equal(t, 2.3, got.AvgMessages)
	assert.Equal(t, 2, got.MedianMessages)
	assert.Equal(t, 3, got.P90Messages)
	assert.Equal(t, "alpha", got.MostActive)
	assert.Equal(t, 1.0, got.Concentration)
	codex, ok := got.Agents["codex"]
	require.True(t, ok)
	require.NotNil(t, codex)
	assert.Equal(t, db.AgentSummary{Sessions: 2, Messages: 5}, *codex)
	claude, ok := got.Agents["claude"]
	require.True(t, ok)
	require.NotNil(t, claude)
	assert.Equal(t, db.AgentSummary{Sessions: 1, Messages: 2}, *claude)

	projects, err := store.GetAnalyticsProjects(t.Context(), filter)
	require.NoError(t, err)
	require.Len(t, projects.Projects, 2)
	assert.Equal(t, "alpha", projects.Projects[0].Name)
	assert.Equal(t, 2, projects.Projects[0].Sessions)
	assert.Equal(t, 5, projects.Projects[0].Messages)
	assert.Equal(t, "beta", projects.Projects[1].Name)
	assert.Equal(t, 1, projects.Projects[1].Sessions)
	assert.Equal(t, 2, projects.Projects[1].Messages)
}

func assertAnalyticsTimeline(t *testing.T, store AnalyticsStore, filter db.AnalyticsFilter) {
	t.Helper()
	activityResult, err := store.GetAnalyticsActivity(t.Context(), filter, "day")
	require.NoError(t, err)
	require.Len(t, activityResult.Series, 1)
	assert.Equal(t, db.ActivityEntry{
		Date: "2026-08-02", Sessions: 3, Messages: 7,
		UserMessages: 4, AssistantMessages: 3, ToolCalls: 3,
		ThinkingMessages: 1, ByAgent: map[string]int{"codex": 5, "claude": 2},
	}, activityResult.Series[0])

	heatmap, err := store.GetAnalyticsHeatmap(t.Context(), filter, "messages")
	require.NoError(t, err)
	assert.Equal(t, "2026-08-02", heatmap.EntriesFrom)
	require.Len(t, heatmap.Entries, 1)
	assert.Equal(t, db.HeatmapEntry{Date: "2026-08-02", Value: 7, Level: 1}, heatmap.Entries[0])

	hours, err := store.GetAnalyticsHourOfWeek(t.Context(), filter)
	require.NoError(t, err)
	require.Len(t, hours.Cells, 168)
	assert.Equal(t, 3, hours.Cells[6*24+10].Messages)
	assert.Equal(t, 2, hours.Cells[6*24+11].Messages)
	assert.Equal(t, 2, hours.Cells[6*24+12].Messages)
}

func assertAnalyticsBreakdowns(t *testing.T, store AnalyticsStore, filter db.AnalyticsFilter) {
	t.Helper()
	shape, err := store.GetAnalyticsSessionShape(t.Context(), filter)
	require.NoError(t, err)
	assert.Equal(t, 3, shape.Count)
	assert.Equal(t, []db.DistributionBucket{{Label: "1-5", Count: 3}}, shape.LengthDistribution)
	assert.Equal(t, []db.DistributionBucket{{Label: "<5m", Count: 2}, {Label: "5-15m", Count: 1}}, shape.DurationDistribution)
	assert.Equal(t, []db.DistributionBucket{{Label: "0.5-1", Count: 1}, {Label: "1-2", Count: 2}}, shape.AutonomyDistribution)

	tools, err := store.GetAnalyticsTools(t.Context(), filter)
	require.NoError(t, err)
	assert.Equal(t, 3, tools.TotalCalls)
	assert.Equal(t, []db.ToolCategoryCount{
		{Category: "Bash", Count: 1, Pct: 33.3},
		{Category: "Edit", Count: 1, Pct: 33.3},
		{Category: "Write", Count: 1, Pct: 33.3},
	}, tools.ByCategory)

	skills, err := store.GetAnalyticsSkills(t.Context(), filter, "day")
	require.NoError(t, err)
	assert.Equal(t, 2, skills.TotalSkillCalls)
	assert.Equal(t, 1, skills.DistinctSkills)
	require.Len(t, skills.BySkill, 1)
	assert.Equal(t, "testing", skills.BySkill[0].SkillName)
	assert.Equal(t, 2, skills.BySkill[0].CallCount)

	velocity, err := store.GetAnalyticsVelocity(t.Context(), filter)
	require.NoError(t, err)
	assert.Equal(t, db.Percentiles{P50: 60, P90: 120}, velocity.Overall.TurnCycleSec)
	assert.Equal(t, db.Percentiles{P50: 60, P90: 120}, velocity.Overall.FirstResponseSec)

	top, err := store.GetAnalyticsTopSessions(t.Context(), filter, "messages")
	require.NoError(t, err)
	require.Len(t, top.Sessions, 3)
	assert.Equal(t, []string{"analytics-a", "analytics-b", "analytics-c"}, []string{
		top.Sessions[0].ID, top.Sessions[1].ID, top.Sessions[2].ID,
	})
}

func assertAnalyticsSignals(t *testing.T, store AnalyticsStore, filter db.AnalyticsFilter) {
	t.Helper()
	signals, err := store.GetAnalyticsSignals(t.Context(), filter)
	require.NoError(t, err)
	assert.Equal(t, 3, signals.ScoredSessions)
	require.NotNil(t, signals.AvgHealthScore)
	assert.Equal(t, 66.7, *signals.AvgHealthScore)
	assert.Equal(t, map[string]int{"A": 1, "C": 1, "D": 1}, signals.GradeDistribution)
	assert.Equal(t, map[string]int{"completed": 1, "errored": 1, "abandoned": 1}, signals.OutcomeDistribution)
	assert.Equal(t, 2, signals.ToolHealth.TotalFailureSignals)
	assert.Equal(t, 1, signals.ToolHealth.TotalRetries)
	assert.Equal(t, 1, signals.ToolHealth.SessionsWithFailures)

	examples, err := store.GetAnalyticsSignalSessions(
		t.Context(), filter, "tool_failure_signals", 10,
	)
	require.NoError(t, err)
	require.Len(t, examples.Sessions, 1)
	assert.Equal(t, "analytics-b", examples.Sessions[0].SessionID)
	assert.Equal(t, 2, examples.Sessions[0].SignalTotal)
}

func assertAnalyticsReports(t *testing.T, store AnalyticsStore, filter db.AnalyticsFilter) {
	t.Helper()
	terms, err := db.ParseTrendTerms([]string{"seam"})
	require.NoError(t, err)
	trends, err := store.GetTrendsTerms(t.Context(), filter, terms, "day")
	require.NoError(t, err)
	assert.Equal(t, 7, trends.MessageCount)
	require.Len(t, trends.Series, 1)
	assert.Equal(t, 4, trends.Series[0].Total)
	assert.Equal(t, []db.TrendPoint{{Date: "2026-08-02", Count: 4}}, trends.Series[0].Points)

	query := activity.Query{
		Timezone: "UTC", Loc: time.UTC,
		RangeStart:    time.Date(2026, 8, 2, 9, 0, 0, 0, time.UTC),
		RangeEnd:      time.Date(2026, 8, 2, 13, 0, 0, 0, time.UTC),
		EffectiveEnd:  time.Date(2026, 8, 2, 13, 0, 0, 0, time.UTC),
		Bucket:        activity.BucketSpec{Unit: activity.BucketHour, NominalSeconds: 3600},
		GapCapSeconds: 300,
	}
	report, err := store.GetActivityReport(t.Context(), filter, query)
	require.NoError(t, err)
	assert.Equal(t, 3, report.Totals.Sessions)
	assert.Equal(t, 1, report.Totals.AutomatedSessions)
	assert.Equal(t, 2, report.Totals.InteractiveSessions)
	assert.Equal(t, 2, report.Totals.DistinctProjects)
	assert.Equal(t, 1, report.Peak.Agents)

	edits, err := store.RecentEdits(t.Context(), db.RecentEditsParams{})
	require.NoError(t, err)
	require.Len(t, edits.Files, 2)
	assert.Equal(t, "beta.go", edits.Files[0].FilePath)
	assert.Equal(t, "alpha.go", edits.Files[1].FilePath)
	assert.False(t, edits.HasMore)
}

// InsertBunAnalyticsFixture inserts the canonical Task 8 four-session matrix.
func InsertBunAnalyticsFixture(
	ctx context.Context, store bun.IDB, archiveID, generation string,
) error {
	if _, err := store.NewInsert().Model(&bunmodel.SourceArchive{
		SourceArchiveID: archiveID, SourceArchiveSalt: "analytics-contract-salt",
	}).On("CONFLICT (source_archive_id) DO NOTHING").Exec(ctx); err != nil {
		return err
	}

	timestamp := func(raw string) bunmodel.Timestamp {
		parsed, err := time.Parse(time.RFC3339Nano, raw)
		if err != nil {
			panic(err)
		}
		return bunmodel.NewTimestamp(parsed)
	}
	timestampPtr := func(raw string) *bunmodel.Timestamp {
		value := timestamp(raw)
		return &value
	}
	stringPtr := func(value string) *string { return &value }
	intPtr := func(value int) *int { return &value }
	floatPtr := func(value float64) *float64 { return &value }

	sessions := []bunmodel.Session{
		{
			ID: "analytics-a", Project: "alpha", Machine: "host-a", Agent: "codex",
			DisplayName: stringPtr("Alpha work"), StartedAt: timestampPtr("2026-08-02T10:00:00Z"),
			EndedAt: timestampPtr("2026-08-02T10:06:00Z"), MessageCount: 3, UserMessageCount: 2,
			TotalOutputTokens: 20, HasTotalOutputTokens: true, Outcome: "completed",
			OutcomeConfidence: "high", HealthScore: intPtr(90), HealthGrade: stringPtr("A"),
			QualitySignalVersion: 1, CreatedAt: timestamp("2026-08-02T10:00:00Z"),
			SourceArchiveID: archiveID, SourceDatabaseGeneration: generation,
		},
		{
			ID: "analytics-b", Project: "alpha", Machine: "host-b", Agent: "claude",
			FirstMessage: stringPtr("retry command"), StartedAt: timestampPtr("2026-08-02T11:00:00Z"),
			EndedAt: timestampPtr("2026-08-02T11:03:00Z"), MessageCount: 2, UserMessageCount: 1,
			TotalOutputTokens: 10, HasTotalOutputTokens: true, IsAutomated: true,
			Outcome: "errored", OutcomeConfidence: "high", ToolFailureSignalCount: 2,
			ToolRetryCount: 1, HealthScore: intPtr(40), HealthGrade: stringPtr("D"),
			QualitySignalVersion: 1, MissingVerificationCount: 1,
			CreatedAt: timestamp("2026-08-02T11:00:00Z"), SourceArchiveID: archiveID,
			SourceDatabaseGeneration: generation,
		},
		{
			ID: "analytics-c", Project: "beta", Machine: "host-a", Agent: "codex",
			StartedAt: timestampPtr("2026-08-02T12:00:00Z"), EndedAt: timestampPtr("2026-08-02T12:00:30Z"),
			MessageCount: 2, UserMessageCount: 1, TotalOutputTokens: 5,
			HasTotalOutputTokens: true, Outcome: "abandoned", OutcomeConfidence: "medium",
			HealthScore: intPtr(70), HealthGrade: stringPtr("C"), ContextPressureMax: floatPtr(0.5),
			QualitySignalVersion: 1, CreatedAt: timestamp("2026-08-02T12:00:00Z"),
			SourceArchiveID: archiveID, SourceDatabaseGeneration: generation,
		},
		{
			ID: "analytics-d", Project: "beta", Machine: "host-b", Agent: "claude",
			StartedAt: timestampPtr("2026-08-03T09:00:00Z"), EndedAt: timestampPtr("2026-08-03T09:01:00Z"),
			MessageCount: 1, UserMessageCount: 1, Outcome: "completed", OutcomeConfidence: "low",
			CreatedAt: timestamp("2026-08-03T09:00:00Z"), SourceArchiveID: archiveID,
			SourceDatabaseGeneration: generation,
		},
	}
	for i := range sessions {
		if _, err := store.NewInsert().Model(&sessions[i]).Exec(ctx); err != nil {
			return err
		}
	}

	messages := []bunmodel.Message{
		{ID: new(int64(101)), SessionID: "analytics-a", Ordinal: 0, Role: "user", Content: "seam plan", ContentLength: 9, Timestamp: timestampPtr("2026-08-02T10:00:00Z")},
		{ID: new(int64(102)), SessionID: "analytics-a", Ordinal: 1, Role: "assistant", Content: "seam done", ContentLength: 9, Timestamp: timestampPtr("2026-08-02T10:01:00Z"), Model: "model-a", HasThinking: true, HasToolUse: true, OutputTokens: 20, HasOutputTokens: true},
		{ID: new(int64(103)), SessionID: "analytics-a", Ordinal: 2, Role: "user", Content: "verify seam", ContentLength: 11, Timestamp: timestampPtr("2026-08-02T10:05:00Z")},
		{ID: new(int64(201)), SessionID: "analytics-b", Ordinal: 0, Role: "user", Content: "retry command", ContentLength: 13, Timestamp: timestampPtr("2026-08-02T11:00:00Z")},
		{ID: new(int64(202)), SessionID: "analytics-b", Ordinal: 1, Role: "assistant", Content: "failed", ContentLength: 6, Timestamp: timestampPtr("2026-08-02T11:02:00Z"), Model: "model-b", HasToolUse: true, OutputTokens: 10, HasOutputTokens: true},
		{ID: new(int64(301)), SessionID: "analytics-c", Ordinal: 0, Role: "user", Content: "seam beta", ContentLength: 9, Timestamp: timestampPtr("2026-08-02T12:00:00Z")},
		{ID: new(int64(302)), SessionID: "analytics-c", Ordinal: 1, Role: "assistant", Content: "ok", ContentLength: 2, Timestamp: timestampPtr("2026-08-02T12:00:20Z"), Model: "model-a", HasToolUse: true, OutputTokens: 5, HasOutputTokens: true},
		{ID: new(int64(401)), SessionID: "analytics-d", Ordinal: 0, Role: "user", Content: "outside", ContentLength: 7, Timestamp: timestampPtr("2026-08-03T09:00:00Z")},
	}
	for i := range messages {
		if _, err := store.NewInsert().Model(&messages[i]).Exec(ctx); err != nil {
			return err
		}
	}

	toolCalls := []bunmodel.ToolCall{
		{ID: new(int64(1001)), MessageID: new(int64(102)), SessionID: "analytics-a", MessageOrdinal: 1, ToolName: "Edit", Category: "Edit", CallIndex: 0, SkillName: stringPtr("testing"), FilePath: stringPtr("alpha.go")},
		{ID: new(int64(1002)), MessageID: new(int64(202)), SessionID: "analytics-b", MessageOrdinal: 1, ToolName: "Bash", Category: "Bash", CallIndex: 0},
		{ID: new(int64(1003)), MessageID: new(int64(302)), SessionID: "analytics-c", MessageOrdinal: 1, ToolName: "Write", Category: "Write", CallIndex: 0, SkillName: stringPtr("testing"), FilePath: stringPtr("beta.go")},
	}
	for i := range toolCalls {
		if _, err := store.NewInsert().Model(&toolCalls[i]).Exec(ctx); err != nil {
			return err
		}
	}
	return nil
}

//go:fix inline
