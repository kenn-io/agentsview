package db

import "go.kenn.io/agentsview/internal/money"

// SessionStats is the top-level v2 output of GetSessionStats.
// schema_version is locked at 2. Additive fields (new keys that
// old consumers can ignore) and semantic tightening (e.g., routing
// an existing field through a stricter definition) are allowed
// within v1 without a bump as long as the field *shape* stays
// compatible. Incompatible shape changes or bucket-boundary shifts
// still require a version bump.
//
// Feature detection by consumers should use the presence of
// specific fields (e.g., agent_portfolio.by_sessions_human) or the
// reporter's agentsview_version, not schema_version, for non-bump
// changes.
type SessionStats struct {
	SchemaVersion   int                  `json:"schema_version"`
	Window          StatsWindow          `json:"window"`
	Filters         StatsFilters         `json:"filters"`
	Totals          StatsTotals          `json:"totals"`
	Distributions   StatsDistributions   `json:"distributions"`
	Archetypes      StatsArchetypes      `json:"archetypes"`
	Velocity        StatsVelocity        `json:"velocity"`
	ToolMix         StatsToolMix         `json:"tool_mix"`
	ModelMix        StatsModelMix        `json:"model_mix"`
	Adoption        *StatsAdoption       `json:"adoption,omitempty"`
	AgentPortfolio  StatsAgentPortfolio  `json:"agent_portfolio"`
	CacheEconomics  *StatsCacheEconomics `json:"cache_economics,omitempty"`
	Temporal        StatsTemporal        `json:"temporal"`
	OutcomeStats    *StatsOutcomeStats   `json:"outcome_stats,omitempty"`
	Outcomes        *StatsOutcomes       `json:"outcomes,omitempty"`
	CodeAttribution *CodeAttribution     `json:"code_attribution,omitempty"`
	GeneratedAt     string               `json:"generated_at"`
}

type StatsWindow struct {
	Since string `json:"since"`
	Until string `json:"until"`
	Days  int    `json:"days"`
}

type StatsFilters struct {
	Agent            string   `json:"agent"`
	ProjectsIncluded []string `json:"projects_included,omitempty"`
	ProjectsExcluded []string `json:"projects_excluded"`
	Timezone         string   `json:"timezone"`
}

type StatsTotals struct {
	SessionsAll        int `json:"sessions_all"`
	SessionsHuman      int `json:"sessions_human"`
	SessionsAutomation int `json:"sessions_automation"`
	// SessionsSubagent is the count of subagent sessions folded into
	// SessionsAll. Human, automation, and subagent partition
	// SessionsAll: sessions_all == sessions_human + sessions_automation
	// + sessions_subagent. Subagents are kept out of the human and
	// automation buckets (a subagent is neither) but counted in the
	// total, so the breakdown stays decomposable.
	SessionsSubagent  int `json:"sessions_subagent"`
	MessagesTotal     int `json:"messages_total"`
	UserMessagesTotal int `json:"user_messages_total"`
}

type DistributionBucketV1 struct {
	// Edge is [lo, hi]; hi may be JSON null for the unbounded top bucket.
	Edge  [2]*float64 `json:"edge"`
	Count int         `json:"count"`
}

type ScopedDistribution struct {
	Buckets []DistributionBucketV1 `json:"buckets"`
	Mean    float64                `json:"mean"`
}

type StatsDistributions struct {
	DurationMinutes   ScopedDistributionPair  `json:"duration_minutes"`
	UserMessages      ScopedDistributionPair  `json:"user_messages"`
	PeakContextTokens PeakContextDistribution `json:"peak_context_tokens"`
	ToolsPerTurn      ScopedDistributionPair  `json:"tools_per_turn"`
}

type ScopedDistributionPair struct {
	ScopeAll   ScopedDistribution `json:"scope_all"`
	ScopeHuman ScopedDistribution `json:"scope_human"`
}

type PeakContextDistribution struct {
	ScopeAll   ScopedDistribution `json:"scope_all"`
	ScopeHuman ScopedDistribution `json:"scope_human"`
	NullCount  int                `json:"null_count"`
	// ClaudeOnly is kept for v1 schema compatibility and is always
	// false: since #646 the distribution covers every agent that
	// reports peak context (claude, hermes, kimi, forge, zed, ...).
	ClaudeOnly bool `json:"claude_only"`
}

type StatsArchetypes struct {
	Automation   int    `json:"automation"`
	Quick        int    `json:"quick"`
	Standard     int    `json:"standard"`
	Deep         int    `json:"deep"`
	Marathon     int    `json:"marathon"`
	Primary      string `json:"primary"`
	PrimaryHuman string `json:"primary_human"`
}

type StatsPercentiles struct {
	P50  float64 `json:"p50"`
	P90  float64 `json:"p90"`
	Mean float64 `json:"mean"`
}

type StatsVelocity struct {
	TurnCycleSeconds      StatsPercentiles `json:"turn_cycle_seconds"`
	FirstResponseSeconds  StatsPercentiles `json:"first_response_seconds"`
	MessagesPerActiveHour float64          `json:"messages_per_active_hour"`
}

type StatsToolMix struct {
	ByCategory map[string]int `json:"by_category"`
	TotalCalls int            `json:"total_calls"`
}

type StatsModelMix struct {
	ByTokens map[string]int64 `json:"by_tokens"`
}

type StatsAdoption struct {
	ClaudeOnly          bool    `json:"claude_only"`
	PlanModeRate        float64 `json:"plan_mode_rate"`
	SubagentsPerSession float64 `json:"subagents_per_session"`
	DistinctSkills      int     `json:"distinct_skills"`
}

type StatsAgentPortfolio struct {
	BySessions map[string]int   `json:"by_sessions"`
	ByTokens   map[string]int64 `json:"by_tokens"`
	ByMessages map[string]int   `json:"by_messages"`
	Primary    string           `json:"primary"`

	// Human-scoped peer fields. Populated alongside the all-sessions
	// maps and filtered to rows where is_automated = 0. Introduced in
	// the flag-authority pipeline change; tkmx-server's renderer
	// prefers these when every portfolio-bearing blob in a user's
	// machine set carries them.
	BySessionsHuman map[string]int   `json:"by_sessions_human"`
	ByTokensHuman   map[string]int64 `json:"by_tokens_human"`
	ByMessagesHuman map[string]int   `json:"by_messages_human"`
	PrimaryHuman    string           `json:"primary_human"`
}

type StatsCacheEconomics struct {
	ClaudeOnly             bool                      `json:"claude_only"`
	CacheHitRatio          CacheHitRatioDistribution `json:"cache_hit_ratio"`
	DollarsSavedVsUncached money.Money               `json:"saved_vs_uncached"`
	DollarsSpent           money.Money               `json:"spent"`
}

type CacheHitRatioDistribution struct {
	Overall float64                `json:"overall"`
	Buckets []DistributionBucketV1 `json:"buckets"`
}

type TemporalHourlyUTCEntry struct {
	TS           string `json:"ts"` // RFC3339 at UTC hour boundary
	Sessions     int    `json:"sessions"`
	UserMessages int    `json:"user_messages"`
}

type StatsTemporal struct {
	HourlyUTC        []TemporalHourlyUTCEntry `json:"hourly_utc"`
	ReporterTimezone string                   `json:"reporter_timezone"`
}

type StatsOutcomeStats struct {
	ReposActive  int  `json:"repos_active"`
	Commits      int  `json:"commits"`
	LOCAdded     int  `json:"loc_added"`
	LOCRemoved   int  `json:"loc_removed"`
	FilesChanged int  `json:"files_changed"`
	PRsOpened    *int `json:"prs_opened,omitempty"` // nil when gh not configured
	PRsMerged    *int `json:"prs_merged,omitempty"`
}

type StatsOutcomes struct {
	ClaudeOnly            bool           `json:"claude_only"`
	Success               int            `json:"success"`
	Failure               int            `json:"failure"`
	Unknown               int            `json:"unknown"`
	GradeDistribution     map[string]int `json:"grade_distribution"`
	ToolRetryRate         float64        `json:"tool_retry_rate"`
	CompactionsPerSession float64        `json:"compactions_per_session"`
	AvgEditChurn          float64        `json:"avg_edit_churn"`
}

type CodeAttribution struct {
	Sources []CodeAttributionSource `json:"sources,omitempty"`
}

type CodeAttributionSource struct {
	Provider string                    `json:"provider"`
	Scope    string                    `json:"scope"`
	Status   string                    `json:"status"`
	Warnings []string                  `json:"warnings,omitempty"`
	Metrics  *CursorAttributionMetrics `json:"metrics,omitempty"`
}

type CursorAttributionMetrics struct {
	ScoredCommits        int64                     `json:"scored_commits"`
	LinesAdded           int64                     `json:"lines_added"`
	LinesDeleted         int64                     `json:"lines_deleted"`
	TabLinesAdded        int64                     `json:"tab_lines_added"`
	TabLinesDeleted      int64                     `json:"tab_lines_deleted"`
	ComposerLinesAdded   int64                     `json:"composer_lines_added"`
	ComposerLinesDeleted int64                     `json:"composer_lines_deleted"`
	HumanLinesAdded      int64                     `json:"human_lines_added"`
	HumanLinesDeleted    int64                     `json:"human_lines_deleted"`
	BlankLinesAdded      int64                     `json:"blank_lines_added"`
	BlankLinesDeleted    int64                     `json:"blank_lines_deleted"`
	AIAuthoredPct        float64                   `json:"ai_authored_pct"`
	ConversationCounts   []CursorConversationCount `json:"conversation_counts,omitempty"`
}

type CursorConversationCount struct {
	Model string `json:"model"`
	Mode  string `json:"mode"`
	Count int64  `json:"count"`
}
