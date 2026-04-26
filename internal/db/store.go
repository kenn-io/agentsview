package db

import "context"

// ErrReadOnly is returned by write methods on read-only store
// implementations (e.g. the PostgreSQL reader).
var ErrReadOnly = errReadOnly{}

type errReadOnly struct{}

func (errReadOnly) Error() string { return "not available in remote mode" }

// Store is the interface the HTTP server uses for all data access.
// The concrete *DB (SQLite) satisfies it implicitly. The pgdb
// package provides a read-only PostgreSQL implementation.
type Store interface {
	// Cursor pagination.
	SetCursorSecret(secret []byte)
	EncodeCursor(endedAt, id string, total ...int) string
	DecodeCursor(s string) (SessionCursor, error)

	// Sessions.
	ListSessions(ctx context.Context, f SessionFilter) (SessionPage, error)
	GetSession(ctx context.Context, id string) (*Session, error)
	GetSessionFull(ctx context.Context, id string) (*Session, error)
	GetChildSessions(ctx context.Context, parentID string) ([]Session, error)

	// Messages.
	GetMessages(ctx context.Context, sessionID string, from, limit int, asc bool) ([]Message, error)
	GetAllMessages(ctx context.Context, sessionID string) ([]Message, error)
	GetSessionActivity(ctx context.Context, sessionID string) (*SessionActivityResponse, error)

	// Timing.
	GetSessionTiming(ctx context.Context, sessionID string) (*SessionTiming, error)

	// Search.
	HasFTS() bool
	Search(ctx context.Context, f SearchFilter) (SearchPage, error)
	SearchSession(ctx context.Context, sessionID, query string) ([]int, error)

	// SSE change detection.
	GetSessionVersion(id string) (count int, fileMtime int64, ok bool)

	// Metadata.
	GetStats(ctx context.Context, excludeOneShot, excludeAutomated bool) (Stats, error)
	GetProjects(ctx context.Context, excludeOneShot, excludeAutomated bool) ([]ProjectInfo, error)
	GetAgents(ctx context.Context, excludeOneShot, excludeAutomated bool) ([]AgentInfo, error)
	GetMachines(ctx context.Context, excludeOneShot, excludeAutomated bool) ([]string, error)

	// Analytics.
	GetAnalyticsSummary(ctx context.Context, f AnalyticsFilter) (AnalyticsSummary, error)
	GetAnalyticsActivity(ctx context.Context, f AnalyticsFilter, granularity string) (ActivityResponse, error)
	GetAnalyticsHeatmap(ctx context.Context, f AnalyticsFilter, metric string) (HeatmapResponse, error)
	GetAnalyticsProjects(ctx context.Context, f AnalyticsFilter) (ProjectsAnalyticsResponse, error)
	GetAnalyticsHourOfWeek(ctx context.Context, f AnalyticsFilter) (HourOfWeekResponse, error)
	GetAnalyticsSessionShape(ctx context.Context, f AnalyticsFilter) (SessionShapeResponse, error)
	GetAnalyticsTools(ctx context.Context, f AnalyticsFilter) (ToolsAnalyticsResponse, error)
	GetAnalyticsVelocity(ctx context.Context, f AnalyticsFilter) (VelocityResponse, error)
	GetAnalyticsTopSessions(ctx context.Context, f AnalyticsFilter, metric string) (TopSessionsResponse, error)
	GetAnalyticsSignals(ctx context.Context, f AnalyticsFilter) (SignalsAnalyticsResponse, error)
	GetTrendsTerms(ctx context.Context, f AnalyticsFilter, terms []TrendTermInput, granularity string) (TrendsTermsResponse, error)

	// Usage (token cost).
	GetDailyUsage(ctx context.Context, f UsageFilter) (DailyUsageResult, error)
	GetTopSessionsByCost(ctx context.Context, f UsageFilter, limit int) ([]TopSessionEntry, error)
	GetUsageSessionCounts(ctx context.Context, f UsageFilter) (UsageSessionCounts, error)

	// Stars (local-only; PG returns ErrReadOnly).
	StarSession(sessionID string) (bool, error)
	UnstarSession(sessionID string) error
	ListStarredSessionIDs(ctx context.Context) ([]string, error)
	BulkStarSessions(sessionIDs []string) error

	// Pins (local-only; PG returns ErrReadOnly).
	PinMessage(sessionID string, messageID int64, note *string) (int64, error)
	UnpinMessage(sessionID string, messageID int64) error
	ListPinnedMessages(ctx context.Context, sessionID string, project string) ([]PinnedMessage, error)

	// Insights (local-only; PG returns ErrReadOnly).
	ListInsights(ctx context.Context, f InsightFilter) ([]Insight, error)
	GetInsight(ctx context.Context, id int64) (*Insight, error)
	InsertInsight(s Insight) (int64, error)
	DeleteInsight(id int64) error

	// Session management (local-only; PG returns ErrReadOnly).
	RenameSession(id string, displayName *string) error
	SoftDeleteSession(id string) error
	RestoreSession(id string) (int64, error)
	DeleteSessionIfTrashed(id string) (int64, error)
	ListTrashedSessions(ctx context.Context) ([]Session, error)
	EmptyTrash() (int, error)

	// Upload (local-only; PG returns ErrReadOnly).
	UpsertSession(s Session) error
	ReplaceSessionMessages(sessionID string, msgs []Message) error

	// ReadOnly returns true for remote/PG-backed stores.
	ReadOnly() bool
}

// Compile-time check: *DB satisfies Store.
var _ Store = (*DB)(nil)
