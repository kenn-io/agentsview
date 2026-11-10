import type {
  AnalyticsSummaryResponse as AnalyticsSummary,
  DbActivityResponse as ActivityResponse,
  DbHeatmapResponse as HeatmapResponse,
  DbProjectsAnalyticsResponse as ProjectsAnalyticsResponse,
  DbHourOfWeekResponse as HourOfWeekResponse,
  DbSessionShapeResponse as SessionShapeResponse,
  DbVelocityResponse as VelocityResponse,
  DbToolsAnalyticsResponse as ToolsAnalyticsResponse,
  DbSkillsAnalyticsResponse as SkillsAnalyticsResponse,
  DbTopSessionsResponse as TopSessionsResponse,
  DbSignalsAnalyticsResponse as SignalsAnalyticsResponse,
} from "../api/generated/index.js";

export function makeSignals(): SignalsAnalyticsResponse {
  return {
    scored_sessions: 0,
    unscored_sessions: 0,
    grade_distribution: {},
    avg_health_score: null,
    outcome_distribution: {},
    outcome_confidence_distribution: {},
    tool_health: {
      total_failure_signals: 0,
      total_retries: 0,
      total_edit_churn: 0,
      sessions_with_failures: 0,
      failure_rate: 0,
    },
    context_health: {
      avg_compaction_count: 0,
      sessions_with_compaction: 0,
      mid_task_compaction_count: 0,
      sessions_with_mid_task_compaction: 0,
      sessions_with_context_data: 0,
      avg_context_pressure: null,
      high_pressure_sessions: 0,
    },
    quality_health: {
      computed_sessions: 0,
      totals: {
        short_prompt_count: 0,
        unstructured_start: 0,
        missing_success_criteria_count: 0,
        missing_verification_count: 0,
        duplicate_prompt_count: 0,
        no_code_context_count: 0,
        runaway_tool_loop_count: 0,
        frustration_marker_count: 0,
      },
      sessions_with_signal: {
        short_prompt_count: 0,
        unstructured_start: 0,
        missing_success_criteria_count: 0,
        missing_verification_count: 0,
        duplicate_prompt_count: 0,
        no_code_context_count: 0,
        runaway_tool_loop_count: 0,
        frustration_marker_count: 0,
      },
    },
    trend: [],
    by_agent: [],
    by_project: [],
    calibration: {},
  };
}

export function makeSummary(): AnalyticsSummary {
  return {
    total_sessions: 10,
    total_messages: 100,
    total_output_tokens: 42000,
    token_reporting_sessions: 8,
    models: [],
    active_projects: 3,
    active_days: 5,
    avg_messages: 10,
    median_messages: 8,
    p90_messages: 20,
    most_active_project: "proj",
    concentration: 0.5,
    agents: {},
  };
}

export function makeActivity(): ActivityResponse {
  return { granularity: "day", series: [] };
}

export function makeHeatmap(): HeatmapResponse {
  return {
    metric: "messages",
    entries: [],
    levels: { l1: 1, l2: 5, l3: 10, l4: 20 },
    entries_from: "2024-01-01",
  };
}

export function makeProjects(): ProjectsAnalyticsResponse {
  return { projects: [] };
}

export function makeHourOfWeek(): HourOfWeekResponse {
  return { cells: [] };
}

export function makeSessionShape(): SessionShapeResponse {
  return {
    count: 0,
    length_distribution: [],
    duration_distribution: [],
    autonomy_distribution: [],
  };
}

export function makeVelocity(): VelocityResponse {
  return {
    overall: {
      turn_cycle_sec: { p50: 0, p90: 0 },
      first_response_sec: { p50: 0, p90: 0 },
      msgs_per_active_min: 0,
      chars_per_active_min: 0,
      tool_calls_per_active_min: 0,
    },
    by_agent: [],
    by_complexity: [],
  };
}

export function makeTools(): ToolsAnalyticsResponse {
  return {
    total_calls: 0,
    by_category: [],
    by_agent: [],
    by_tool: [],
    trend: [],
  };
}

export function makeSkills(): SkillsAnalyticsResponse {
  return {
    total_skill_calls: 0,
    distinct_skills: 0,
    by_skill: [],
    trend: [],
  };
}

export function makeTopSessions(): TopSessionsResponse {
  return { metric: "messages", sessions: [] };
}
