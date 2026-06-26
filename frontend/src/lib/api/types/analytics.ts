/** Analytics types — match Go structs in internal/db/analytics.go */

export type Granularity = "day" | "week" | "month";
export type TrendsGranularity = "day" | "week" | "month";
export type HeatmapMetric =
  | "messages"
  | "sessions"
  | "output_tokens";
export type TopSessionsMetric =
  | "messages"
  | "duration"
  | "output_tokens";
export type AutomatedScope = "human" | "all" | "automated";

export interface AgentSummary {
  sessions: number;
  messages: number;
}

export interface AnalyticsSummary {
  total_sessions: number;
  total_messages: number;
  total_output_tokens?: number;
  token_reporting_sessions?: number;
  active_projects: number;
  active_days: number;
  avg_messages: number;
  median_messages: number;
  p90_messages: number;
  most_active_project: string;
  concentration: number;
  agents: Record<string, AgentSummary>;
}

export interface ActivityEntry {
  date: string;
  sessions: number;
  messages: number;
  user_messages: number;
  assistant_messages: number;
  tool_calls: number;
  thinking_messages: number;
  by_agent: Record<string, number>;
}

export interface ActivityResponse {
  granularity: string;
  series: ActivityEntry[];
}

export interface HeatmapEntry {
  date: string;
  value: number;
  level: number;
}

export interface HeatmapLevels {
  l1: number;
  l2: number;
  l3: number;
  l4: number;
}

export interface HeatmapResponse {
  metric: HeatmapMetric;
  entries: HeatmapEntry[];
  levels: HeatmapLevels;
  entries_from: string;
}

export interface ProjectAnalytics {
  name: string;
  sessions: number;
  messages: number;
  first_session: string;
  last_session: string;
  avg_messages: number;
  median_messages: number;
  agents: Record<string, number>;
  daily_trend: number;
}

export interface ProjectsAnalyticsResponse {
  projects: ProjectAnalytics[];
}

export interface HourOfWeekCell {
  day_of_week: number;
  hour: number;
  messages: number;
}

export interface HourOfWeekResponse {
  cells: HourOfWeekCell[];
}

export interface DistributionBucket {
  label: string;
  count: number;
}

export interface SessionShapeResponse {
  count: number;
  length_distribution: DistributionBucket[];
  duration_distribution: DistributionBucket[];
  autonomy_distribution: DistributionBucket[];
}

export interface Percentiles {
  p50: number;
  p90: number;
}

export interface VelocityOverview {
  turn_cycle_sec: Percentiles;
  first_response_sec: Percentiles;
  msgs_per_active_min: number;
  chars_per_active_min: number;
  tool_calls_per_active_min: number;
}

export interface VelocityBreakdown {
  label: string;
  sessions: number;
  overview: VelocityOverview;
}

export interface VelocityResponse {
  overall: VelocityOverview;
  by_agent: VelocityBreakdown[];
  by_complexity: VelocityBreakdown[];
}

export interface TopSession {
  id: string;
  project: string;
  first_message: string | null;
  display_name?: string | null;
  message_count: number;
  output_tokens: number;
  duration_min: number;
  /** ISO timestamps used by the StatusDot component to compute
   * the active/stale/unclean tier — the column needs the same
   * recency inputs as the sidebar list. */
  started_at?: string | null;
  ended_at?: string | null;
  termination_status?: string | null;
}

export interface TopSessionsResponse {
  metric: TopSessionsMetric;
  sessions: TopSession[];
}

export interface ToolCategoryCount {
  category: string;
  count: number;
  pct: number;
}

export interface ToolAgentBreakdown {
  agent: string;
  total: number;
  categories: ToolCategoryCount[];
}

export interface ToolTrendEntry {
  date: string;
  by_category: Record<string, number>;
}

export interface ToolsAnalyticsResponse {
  total_calls: number;
  by_category: ToolCategoryCount[];
  by_agent: ToolAgentBreakdown[];
  trend: ToolTrendEntry[];
}

export interface SkillAgentBreakdown {
  agent: string;
  count: number;
}

export interface SkillProjectBreakdown {
  project: string;
  count: number;
}

export interface SkillUsage {
  skill_name: string;
  call_count: number;
  session_count: number;
  agent_breakdown: SkillAgentBreakdown[];
  project_breakdown: SkillProjectBreakdown[];
  last_used_at: string;
  pct: number;
}

export interface SkillTrendEntry {
  date: string;
  by_skill: Record<string, number>;
}

export interface SkillsAnalyticsResponse {
  total_skill_calls: number;
  distinct_skills: number;
  by_skill: SkillUsage[];
  trend: SkillTrendEntry[];
}

export interface SignalsToolHealth {
  total_failure_signals: number;
  total_retries: number;
  total_edit_churn: number;
  sessions_with_failures: number;
  /** Already a percentage 0-100; do not multiply by 100. */
  failure_rate: number;
}

export interface SignalsContextHealth {
  avg_compaction_count: number;
  sessions_with_compaction: number;
  mid_task_compaction_count: number;
  sessions_with_mid_task_compaction: number;
  sessions_with_context_data: number;
  avg_context_pressure: number | null;
  high_pressure_sessions: number;
}

export interface QualitySignalTotals {
  short_prompt_count: number;
  unstructured_start: number;
  missing_success_criteria_count: number;
  missing_verification_count: number;
  duplicate_prompt_count: number;
  no_code_context_count: number;
  runaway_tool_loop_count: number;
  frustration_marker_count: number;
}

export interface SignalsQualityHealth {
  computed_sessions: number;
  totals: QualitySignalTotals;
  sessions_with_signal: QualitySignalTotals;
}

export interface SignalsTrendBucket {
  date: string;
  session_count: number;
  avg_health_score: number | null;
  completed: number;
  errored: number;
  abandoned: number;
  avg_failure_signals: number;
}

export interface SignalsAgentRow {
  agent: string;
  session_count: number;
  avg_health_score: number | null;
  /** Already a percentage 0-100; do not multiply by 100. */
  completed_rate: number;
  avg_failure_signals: number;
}

export interface SignalsProjectRow {
  project: string;
  session_count: number;
  avg_health_score: number | null;
  /** Already a percentage 0-100; do not multiply by 100. */
  completed_rate: number;
  avg_failure_signals: number;
}

export interface SignalCalibration {
  signal: string;
  affected_sessions: number;
  baseline_sessions: number;
  affected_incomplete_rate: number;
  baseline_incomplete_rate: number;
  incomplete_lift: number | null;
  avg_score_delta: number | null;
}

export interface SignalSessionExample {
  session_id: string;
  project: string;
  agent: string;
  date: string;
  is_automated: boolean;
  outcome: string;
  health_score: number | null;
  health_grade: string | null;
  signal_total: number;
  reason_code: string;
  excerpt: string;
  message_ordinal?: number | null;
  failure_signals: number;
  retries: number;
  edit_churn: number;
}

export interface SignalSessionsResponse {
  signal: string;
  sessions: SignalSessionExample[];
}

export interface SignalsAnalyticsResponse {
  scored_sessions: number;
  unscored_sessions: number;
  grade_distribution: Record<string, number>;
  avg_health_score: number | null;
  outcome_distribution: Record<string, number>;
  outcome_confidence_distribution: Record<string, number>;
  tool_health: SignalsToolHealth;
  context_health: SignalsContextHealth;
  quality_health: SignalsQualityHealth;
  trend: SignalsTrendBucket[];
  by_agent: SignalsAgentRow[];
  by_project: SignalsProjectRow[];
  calibration: Record<string, SignalCalibration>;
}

export interface TrendsBucket {
  date: string;
  message_count: number;
}

export interface TrendsPoint {
  date: string;
  count: number;
}

export interface TrendsSeries {
  term: string;
  variants: string[];
  total: number;
  points: TrendsPoint[];
}

export interface TrendsTermsResponse {
  granularity: TrendsGranularity;
  from: string;
  to: string;
  message_count: number;
  buckets: TrendsBucket[];
  series: TrendsSeries[];
}
