import type {
  QualitySignalTotals,
  SignalsAnalyticsResponse,
  SignalsTrendBucket,
} from "../../api/types.js";
import { m } from "../../i18n/index.js";

export type QualityPatternSeverity = "clear" | "watch" | "warning" | "critical" | "unavailable";

export interface QualityPatternDriver {
  id: keyof QualitySignalTotals | string;
  label: string;
  total: number;
  sessions: number;
  unit?: string;
  strength?: "weak" | "contextual" | "strong";
}

export interface QualityPatternExample {
  label: string;
  detail: string;
  score: number | null;
}

export interface QualityPatternView {
  id: string;
  title: string;
  summary: string;
  severity: QualityPatternSeverity;
  severityDescription: string;
  affectedSessions: number;
  totalSessions: number;
  drivers: QualityPatternDriver[];
  trendLabel: string;
  trend: Array<{
    date: string;
    value: number;
    label: string;
    score: number | null;
  }>;
  examples: QualityPatternExample[];
  examplesLabel: string;
  action: string;
}

export interface QualitySummaryView {
  totalSessions: number;
  scoredSessions: number;
  unscoredSessions: number;
  computedQualitySessions: number;
  avgHealthScore: number | null;
  lowQualitySessions: number;
  scoreDistribution: Array<{
    grade: string;
    count: number;
  }>;
}

export interface RuleBasedRecommendation {
  id: string;
  patternId: string;
  label: string;
  rationale: string;
}

const emptyTotals: QualitySignalTotals = {
  short_prompt_count: 0,
  unstructured_start: 0,
  missing_success_criteria_count: 0,
  missing_verification_count: 0,
  duplicate_prompt_count: 0,
  no_code_context_count: 0,
  runaway_tool_loop_count: 0,
  frustration_marker_count: 0,
};

export const QUALITY_PATTERN_SEVERITY_THRESHOLDS = {
  warningRatio: 0.18,
  criticalRatio: 0.35,
} as const;

export function buildQualitySummary(signals: SignalsAnalyticsResponse | null): QualitySummaryView {
  if (!signals) {
    return {
      totalSessions: 0,
      scoredSessions: 0,
      unscoredSessions: 0,
      computedQualitySessions: 0,
      avgHealthScore: null,
      lowQualitySessions: 0,
      scoreDistribution: [],
    };
  }

  const scoreDistribution = Object.entries(signals.grade_distribution ?? {})
    .map(([grade, count]) => ({ grade, count }))
    .sort((a, b) => gradeRank(a.grade) - gradeRank(b.grade));

  return {
    totalSessions: signals.scored_sessions + signals.unscored_sessions,
    scoredSessions: signals.scored_sessions,
    unscoredSessions: signals.unscored_sessions,
    computedQualitySessions: signals.quality_health?.computed_sessions ?? 0,
    avgHealthScore: signals.avg_health_score,
    lowQualitySessions: (signals.grade_distribution?.D ?? 0) + (signals.grade_distribution?.F ?? 0),
    scoreDistribution,
  };
}

export function buildQualityPatterns(
  signals: SignalsAnalyticsResponse | null,
): QualityPatternView[] {
  if (!signals) return [];

  return [
    promptMaturityPattern(signals),
    contextHealthPattern(signals),
    workflowHygienePattern(signals),
    toolReliabilityPattern(signals),
  ];
}

export function buildRuleBasedRecommendations(
  patterns: QualityPatternView[],
): RuleBasedRecommendation[] {
  return patterns
    .filter((p) => p.severity !== "clear" && p.severity !== "unavailable")
    .slice(0, 4)
    .map((pattern) => ({
      id: `rule-${pattern.id}`,
      patternId: pattern.id,
      label: pattern.action,
      rationale:
        pattern.affectedSessions > 0
          ? m.insights_pattern_recommendation_rationale({
              affected: pattern.affectedSessions,
              total: pattern.totalSessions,
            })
          : m.insights_pattern_recommendation_fallback(),
    }));
}

function promptMaturityPattern(signals: SignalsAnalyticsResponse): QualityPatternView {
  const totals = signals.quality_health?.totals ?? emptyTotals;
  const sessions = signals.quality_health?.sessions_with_signal ?? emptyTotals;
  const computed = signals.quality_health?.computed_sessions ?? 0;
  const drivers: QualityPatternDriver[] = [
    signalDriver(
      "short_prompt_count",
      m.insights_pattern_short_task_starts(),
      totals,
      sessions,
      "weak",
    ),
    signalDriver(
      "unstructured_start",
      m.insights_pattern_unstructured_starts(),
      totals,
      sessions,
      "contextual",
    ),
    signalDriver(
      "missing_success_criteria_count",
      m.insights_pattern_missing_success_criteria(),
      totals,
      sessions,
      "contextual",
    ),
    signalDriver(
      "missing_verification_count",
      m.insights_pattern_missing_verification_path(),
      totals,
      sessions,
      "weak",
    ),
    signalDriver(
      "duplicate_prompt_count",
      m.insights_pattern_repeated_prompts(),
      totals,
      sessions,
      "contextual",
    ),
  ];
  const severityDrivers = drivers.filter((driver) => driver.strength !== "weak");
  const affected = maxSessions(severityDrivers);

  return {
    id: "prompt_maturity",
    title: m.insights_pattern_prompt_title(),
    summary: m.insights_pattern_prompt_summary(),
    severity: computed === 0 ? "unavailable" : severityFromRatio(affected, computed),
    severityDescription:
      computed === 0
        ? m.insights_pattern_no_phase3()
        : m.insights_pattern_prompt_severity_description({
            severityDescription: severityDescription(affected, computed),
          }),
    affectedSessions: affected,
    totalSessions: computed,
    drivers,
    trendLabel: m.insights_pattern_score_pressure_proxy(),
    trend: scorePressureTrend(signals.trend),
    examples: topAgentExamples(signals),
    examplesLabel: m.insights_pattern_comparison_groups(),
    action: m.insights_pattern_prompt_action(),
  };
}

function contextHealthPattern(signals: SignalsAnalyticsResponse): QualityPatternView {
  const h = signals.context_health;
  const totals = signals.quality_health?.totals ?? emptyTotals;
  const sessions = signals.quality_health?.sessions_with_signal ?? emptyTotals;
  const total = totalSessions(signals);
  const drivers: QualityPatternDriver[] = [
    {
      id: "sessions_with_compaction",
      label: m.insights_pattern_sessions_with_compaction(),
      total: h.sessions_with_compaction,
      sessions: h.sessions_with_compaction,
    },
    {
      id: "mid_task_compaction_count",
      label: m.insights_pattern_mid_task_compactions(),
      total: h.mid_task_compaction_count,
      sessions: h.sessions_with_mid_task_compaction,
    },
    {
      id: "high_pressure_sessions",
      label: m.insights_pattern_high_context_pressure(),
      total: h.high_pressure_sessions,
      sessions: h.high_pressure_sessions,
    },
    signalDriver(
      "no_code_context_count",
      m.insights_pattern_missing_code_context(),
      totals,
      sessions,
    ),
  ];
  const affected = maxSessions(drivers);

  return {
    id: "context_health",
    title: m.insights_pattern_context_title(),
    summary: m.insights_pattern_context_summary(),
    severity: severityFromRatio(affected, total),
    severityDescription: severityDescription(affected, total),
    affectedSessions: affected,
    totalSessions: total,
    drivers,
    trendLabel: m.insights_pattern_score_pressure_proxy(),
    trend: scorePressureTrend(signals.trend),
    examples: topProjectExamples(signals),
    examplesLabel: m.insights_pattern_comparison_groups(),
    action: m.insights_pattern_context_action(),
  };
}

function workflowHygienePattern(signals: SignalsAnalyticsResponse): QualityPatternView {
  const outcomes = signals.outcome_distribution ?? {};
  const totals = signals.quality_health?.totals ?? emptyTotals;
  const sessions = signals.quality_health?.sessions_with_signal ?? emptyTotals;
  const total = totalSessions(signals);
  const errored = outcomes.errored ?? 0;
  const abandoned = outcomes.abandoned ?? 0;
  const interrupted = errored + abandoned;
  const runawayDriver = signalDriver(
    "runaway_tool_loop_count",
    m.insights_pattern_repeated_failing_tool_cycles(),
    totals,
    sessions,
    "strong",
  );
  const frustrationDriver = signalDriver(
    "frustration_marker_count",
    m.insights_pattern_frustration_markers(),
    totals,
    sessions,
    "contextual",
  );
  const affected = Math.max(interrupted, runawayDriver.sessions, frustrationDriver.sessions);

  return {
    id: "workflow_hygiene",
    title: m.insights_pattern_workflow_title(),
    summary: m.insights_pattern_workflow_summary(),
    severity: severityFromRatio(affected, total),
    severityDescription: severityDescription(affected, total),
    affectedSessions: affected,
    totalSessions: total,
    drivers: [
      {
        id: "outcome_errored",
        label: m.insights_pattern_errored_outcomes(),
        total: errored,
        sessions: errored,
      },
      {
        id: "outcome_abandoned",
        label: m.insights_pattern_abandoned_outcomes(),
        total: abandoned,
        sessions: abandoned,
      },
      {
        id: "outcome_completed",
        label: m.insights_pattern_completed_outcomes(),
        total: outcomes.completed ?? 0,
        sessions: outcomes.completed ?? 0,
      },
      runawayDriver,
      frustrationDriver,
    ],
    trend: signals.trend.map((t) => ({
      date: t.date,
      value: t.errored + t.abandoned,
      label: m.insights_pattern_errored_or_abandoned_sessions(),
      score: t.avg_health_score,
    })),
    trendLabel: m.insights_pattern_interrupted_sessions(),
    examples: topAgentExamples(signals),
    examplesLabel: m.insights_pattern_comparison_groups(),
    action: m.insights_pattern_workflow_action(),
  };
}

function toolReliabilityPattern(signals: SignalsAnalyticsResponse): QualityPatternView {
  const h = signals.tool_health;
  const total = totalSessions(signals);
  const drivers: QualityPatternDriver[] = [
    {
      id: "tool_failure_signals",
      label: m.insights_pattern_failure_signals(),
      total: h.total_failure_signals,
      sessions: toolDriverSessions(signals, "tool_failure_signals", h.sessions_with_failures),
    },
    {
      id: "tool_retries",
      label: m.insights_pattern_retries(),
      total: h.total_retries,
      sessions: toolDriverSessions(signals, "tool_retries", h.sessions_with_failures),
    },
    {
      id: "edit_churn",
      label: m.insights_pattern_edit_churn(),
      total: h.total_edit_churn,
      sessions: toolDriverSessions(signals, "edit_churn", h.sessions_with_failures),
    },
  ];
  const affected = maxSessions(drivers);

  return {
    id: "tool_reliability",
    title: m.insights_pattern_tool_title(),
    summary: m.insights_pattern_tool_summary(),
    severity: severityFromRatio(affected, total),
    severityDescription: severityDescription(affected, total),
    affectedSessions: affected,
    totalSessions: total,
    drivers,
    trend: signals.trend.map((t) => ({
      date: t.date,
      value: t.avg_failure_signals,
      label: m.insights_pattern_average_failure_signals(),
      score: t.avg_health_score,
    })),
    trendLabel: m.insights_pattern_average_failure_signals(),
    examples: topProjectExamples(signals),
    examplesLabel: m.insights_pattern_comparison_groups(),
    action: m.insights_pattern_tool_action(),
  };
}

function toolDriverSessions(
  signals: SignalsAnalyticsResponse,
  signal: string,
  fallback: number,
): number {
  return signals.calibration?.[signal]?.affected_sessions ?? fallback;
}

function signalDriver(
  id: keyof QualitySignalTotals,
  label: string,
  totals: QualitySignalTotals,
  sessions: QualitySignalTotals,
  strength: QualityPatternDriver["strength"] = "strong",
): QualityPatternDriver {
  return {
    id,
    label,
    total: totals[id] ?? 0,
    sessions: sessions[id] ?? 0,
    strength,
  };
}

function totalSessions(signals: SignalsAnalyticsResponse): number {
  return signals.scored_sessions + signals.unscored_sessions;
}

function maxSessions(drivers: QualityPatternDriver[]): number {
  return drivers.reduce((max, driver) => Math.max(max, driver.sessions), 0);
}

function severityFromRatio(affected: number, total: number): QualityPatternSeverity {
  if (total <= 0) return "unavailable";
  const ratio = affected / total;
  if (ratio === 0) return "clear";
  if (ratio >= QUALITY_PATTERN_SEVERITY_THRESHOLDS.criticalRatio) {
    return "critical";
  }
  if (ratio >= QUALITY_PATTERN_SEVERITY_THRESHOLDS.warningRatio) {
    return "warning";
  }
  return "watch";
}

function severityDescription(affected: number, total: number): string {
  if (total <= 0) return m.insights_pattern_no_computed();
  const ratio = affected / total;
  if (ratio === 0) return m.insights_pattern_none_fire();
  if (ratio >= QUALITY_PATTERN_SEVERITY_THRESHOLDS.criticalRatio) {
    return m.insights_pattern_critical_description();
  }
  if (ratio >= QUALITY_PATTERN_SEVERITY_THRESHOLDS.warningRatio) {
    return m.insights_pattern_warning_description();
  }
  return m.insights_pattern_watch_description();
}

function scorePressureTrend(trend: SignalsTrendBucket[]) {
  return trend.map((t) => ({
    date: t.date,
    value: t.avg_health_score == null ? 0 : Math.max(0, Math.round(100 - t.avg_health_score)),
    label: m.insights_pattern_points_below_average(),
    score: t.avg_health_score,
  }));
}

function topProjectExamples(signals: SignalsAnalyticsResponse): QualityPatternExample[] {
  return [...signals.by_project]
    .sort((a, b) => {
      if (b.avg_failure_signals !== a.avg_failure_signals) {
        return b.avg_failure_signals - a.avg_failure_signals;
      }
      return b.session_count - a.session_count;
    })
    .slice(0, 3)
    .map((row) => ({
      label: row.project || m.insights_pattern_unassigned_project(),
      detail: m.insights_pattern_example_detail({
        count: row.session_count,
        completedRate: Math.round(row.completed_rate),
        failureSignals: row.avg_failure_signals.toFixed(1),
      }),
      score: row.avg_health_score,
    }));
}

function topAgentExamples(signals: SignalsAnalyticsResponse): QualityPatternExample[] {
  return [...signals.by_agent]
    .sort((a, b) => {
      const incompleteDelta = 100 - b.completed_rate - (100 - a.completed_rate);
      if (incompleteDelta !== 0) return incompleteDelta;
      return b.session_count - a.session_count;
    })
    .slice(0, 3)
    .map((row) => ({
      label: row.agent,
      detail: m.insights_pattern_example_detail({
        count: row.session_count,
        completedRate: Math.round(row.completed_rate),
        failureSignals: row.avg_failure_signals.toFixed(1),
      }),
      score: row.avg_health_score,
    }));
}

function gradeRank(grade: string): number {
  const order = ["A", "B", "C", "D", "F"];
  const idx = order.indexOf(grade);
  return idx === -1 ? order.length : idx;
}
