package db

import (
	"context"

	"go.kenn.io/agentsview/internal/activity"
	"go.kenn.io/agentsview/internal/export"
)

// ErrReadOnly is returned by write methods on read-only store
// implementations (e.g. the PostgreSQL reader).
var ErrReadOnly = errReadOnly{}

type errReadOnly struct{}

func (errReadOnly) Error() string { return "not available in remote mode" }

// Store is the interface the HTTP server uses for all data access.
// Any new server-visible query or mutation belongs here, not only on
// the SQLite *DB type, so PostgreSQL and DuckDB fail compilation until
// they implement the same capability surface. The backendcontract package
// centralizes compile-time assertions for every concrete provider.
type Store interface {
	// Cursor pagination.
	SetCursorSecret(secret []byte)
	EncodeCursor(c SessionCursor) string
	DecodeCursor(s string) (SessionCursor, error)

	// Sessions.
	ListSessions(ctx context.Context, f SessionFilter) (SessionPage, error)
	GetSidebarSessionIndex(ctx context.Context, f SessionFilter) (SidebarSessionIndex, error)
	GetSession(ctx context.Context, id string) (*Session, error)
	GetSessionFull(ctx context.Context, id string) (*Session, error)
	// FindSessionIDsByPartial uses literal, case-sensitive substring matching.
	FindSessionIDsByPartial(ctx context.Context, partial string, limit int) ([]string, error)
	GetChildSessions(ctx context.Context, parentID string) ([]Session, error)

	// Messages.
	GetMessages(ctx context.Context, sessionID string, from, limit int, asc bool) ([]Message, error)
	GetAllMessages(ctx context.Context, sessionID string) ([]Message, error)
	GetSessionActivity(ctx context.Context, sessionID string) (*SessionActivityResponse, error)

	// Timing.
	GetSessionTiming(ctx context.Context, sessionID string) (*SessionTiming, error)

	// Search.
	HasFTS() bool
	HasSemantic() bool
	Search(ctx context.Context, f SearchFilter) (SearchPage, error)
	SearchSession(ctx context.Context, sessionID, query string) ([]int, error)
	SearchContent(ctx context.Context, f ContentSearchFilter) (ContentSearchPage, error)
	ListSecretFindings(ctx context.Context, f SecretFindingFilter) (SecretFindingPage, error)
	SecretFindingSource(ctx context.Context, f SecretFinding) (string, bool, error)

	// SSE change detection.
	GetSessionVersion(id string) (count int, version int64, ok bool)

	// Metadata.
	GetStats(ctx context.Context, excludeOneShot, excludeAutomated bool) (Stats, error)
	GetProjects(ctx context.Context, excludeOneShot, excludeAutomated bool) ([]ProjectInfo, error)
	GetAgents(ctx context.Context, excludeOneShot, excludeAutomated bool) ([]AgentInfo, error)
	GetMachines(ctx context.Context, excludeOneShot, excludeAutomated bool) ([]string, error)
	GetBranches(ctx context.Context, excludeOneShot, excludeAutomated bool) ([]BranchInfo, error)
	ListProjectIdentityObservations(ctx context.Context, labels []string) ([]export.ProjectIdentityObservation, error)
	BuildProjectIdentityMap(ctx context.Context, labels []string) (map[string]export.ProjectMapEntry, error)

	// Analytics.
	GetAnalyticsSummary(ctx context.Context, f AnalyticsFilter) (AnalyticsSummary, error)
	GetAnalyticsActivity(ctx context.Context, f AnalyticsFilter, granularity string) (ActivityResponse, error)
	GetAnalyticsHeatmap(ctx context.Context, f AnalyticsFilter, metric string) (HeatmapResponse, error)
	GetAnalyticsProjects(ctx context.Context, f AnalyticsFilter) (ProjectsAnalyticsResponse, error)
	GetAnalyticsHourOfWeek(ctx context.Context, f AnalyticsFilter) (HourOfWeekResponse, error)
	GetAnalyticsSessionShape(ctx context.Context, f AnalyticsFilter) (SessionShapeResponse, error)
	GetAnalyticsTools(ctx context.Context, f AnalyticsFilter) (ToolsAnalyticsResponse, error)
	GetAnalyticsSkills(ctx context.Context, f AnalyticsFilter) (SkillsAnalyticsResponse, error)
	GetAnalyticsVelocity(ctx context.Context, f AnalyticsFilter) (VelocityResponse, error)
	GetAnalyticsTopSessions(ctx context.Context, f AnalyticsFilter, metric string) (TopSessionsResponse, error)
	GetAnalyticsSignals(ctx context.Context, f AnalyticsFilter) (SignalsAnalyticsResponse, error)
	GetAnalyticsSignalSessions(ctx context.Context, f AnalyticsFilter, signal string, limit int) (SignalSessionsResponse, error)
	GetTrendsTerms(ctx context.Context, f AnalyticsFilter, terms []TrendTermInput, granularity string) (TrendsTermsResponse, error)
	GetActivityReport(ctx context.Context, f AnalyticsFilter, q activity.Query) (activity.Report, error)
	RecentEdits(ctx context.Context, p RecentEditsParams) (RecentEditsResult, error)

	// Usage (token cost).
	GetDailyUsage(ctx context.Context, f UsageFilter) (DailyUsageResult, error)
	GetTopSessionsByCost(ctx context.Context, f UsageFilter, limit int) ([]TopSessionEntry, error)
	GetUsageSessionCounts(ctx context.Context, f UsageFilter) (UsageSessionCounts, error)
	GetUsageMatchingSessionCount(ctx context.Context, f UsageFilter) (int, error)
	GetSessionUsage(ctx context.Context, sessionID string) (*SessionUsage, error)

	// Stars.
	StarSession(sessionID string) (bool, error)
	UnstarSession(sessionID string) error
	ListStarredSessionIDs(ctx context.Context) ([]string, error)
	BulkStarSessions(sessionIDs []string) error

	// Pins.
	PinMessage(sessionID string, messageID int64, note *string) (int64, error)
	UnpinMessage(sessionID string, messageID int64) error
	ListPinnedMessages(ctx context.Context, sessionID string, project string) ([]PinnedMessage, error)

	// Insights.
	ListInsights(ctx context.Context, f InsightFilter) ([]Insight, error)
	GetInsight(ctx context.Context, id int64) (*Insight, error)
	GetCachedInsight(ctx context.Context, cacheKey string) (*Insight, error)
	InsertInsight(s Insight) (int64, error)
	DeleteInsight(id int64) error

	// Session management.
	RenameSession(id string, displayName *string) error
	SoftDeleteSession(id string) error
	SoftDeleteSessions(ids []string) (int, error)
	RestoreSession(id string) (int64, error)
	DeleteSessionIfTrashed(id string) (int64, error)
	ListTrashedSessions(ctx context.Context) ([]Session, error)
	EmptyTrash() (int, error)

	// Upload (local-only; PG returns ErrReadOnly).
	UpsertSession(s Session) error
	ReplaceSessionMessages(sessionID string, msgs []Message) error
	WriteSessionBatchAtomic(
		writes []SessionBatchWrite,
		beforeCommit ...func() error,
	) (SessionBatchResult, error)

	// ReadOnly returns true for remote/PG-backed stores.
	ReadOnly() bool
}

// Compile-time check: *DB satisfies Store.
var _ Store = (*DB)(nil)
