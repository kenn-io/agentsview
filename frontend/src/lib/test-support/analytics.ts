import type { DbSignalsAnalyticsResponse as SignalsAnalyticsResponse } from "../api/generated/index.js";

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
