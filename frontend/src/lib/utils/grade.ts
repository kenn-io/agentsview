import { m } from "../i18n/index.js";

export interface GradeStyle {
  bg: string;
  text: string;
  border: string;
}

const gradeStyles: Record<string, GradeStyle> = {
  A: { bg: "#dcfce7", text: "#166534", border: "#bbf7d0" },
  B: { bg: "#dbeafe", text: "#1e40af", border: "#bfdbfe" },
  C: { bg: "#fef3c7", text: "#92400e", border: "#fde68a" },
  D: { bg: "#fef3c7", text: "#92400e", border: "#fde68a" },
  F: { bg: "#fee2e2", text: "#991b1b", border: "#fecaca" },
};

const nullGradeStyle: GradeStyle = {
  bg: "#f3f4f6",
  text: "#6b7280",
  border: "#e5e7eb",
};

export function getGradeStyle(grade: string | null | undefined): GradeStyle {
  if (!grade) return nullGradeStyle;
  return gradeStyles[grade] ?? nullGradeStyle;
}

export function getGradeLabel(grade: string | null | undefined): string {
  return grade ?? "--";
}

const outcomeColors: Record<string, string> = {
  completed: "var(--accent-green)",
  abandoned: "var(--accent-amber)",
  errored: "var(--accent-red)",
  unknown: "var(--text-muted)",
};

// Looked up at call time, so each label follows the active locale.
const outcomeLabels: Record<string, () => string> = {
  completed: m.grade_outcome_completed,
  abandoned: m.grade_outcome_abandoned,
  errored: m.grade_outcome_errored,
  unknown: m.grade_outcome_unknown,
};

export function getOutcomeColor(outcome: string): string {
  return outcomeColors[outcome] ?? "var(--text-muted)";
}

export function getOutcomeLabel(outcome: string): string {
  return outcomeLabels[outcome]?.() ?? outcome;
}

const penaltyLabels: Record<string, () => string> = {
  outcome_errored: m.grade_penalty_outcome_errored,
  outcome_abandoned: m.grade_penalty_outcome_abandoned,
  tool_failure_signals: m.grade_penalty_tool_failure_signals,
  tool_retries: m.grade_penalty_tool_retries,
  edit_churn: m.grade_penalty_edit_churn,
  consecutive_failures: m.grade_penalty_consecutive_failures,
  compactions: m.grade_penalty_compactions,
  mid_task_compactions: m.grade_penalty_mid_task_compactions,
  context_pressure_high: m.grade_penalty_context_pressure_high,
  repeated_failing_tool_cycles: m.grade_penalty_repeated_failing_tool_cycles,
  runaway_loop: m.grade_penalty_repeated_failing_tool_cycles,
  constraintless_first_prompt: m.grade_penalty_constraintless_first_prompt,
  missing_success_criteria: m.grade_penalty_missing_success_criteria,
  stuck_repeated_prompts: m.grade_penalty_stuck_repeated_prompts,
  repeated_prompts: m.grade_penalty_stuck_repeated_prompts,
};

export function getPenaltyLabel(key: string): string {
  return penaltyLabels[key]?.() ?? key;
}

const basisLabels: Record<string, () => string> = {
  outcome: m.grade_basis_outcome,
  tool_health: m.grade_basis_tool_health,
  context_pressure: m.grade_basis_context_pressure,
  prompt_quality: m.grade_basis_prompt_quality,
  context_quality: m.grade_basis_context_quality,
  workflow_quality: m.grade_basis_workflow_quality,
};

export function getBasisLabel(key: string): string {
  return basisLabels[key]?.() ?? key;
}

export function scoreToGrade(score: number): string {
  if (score >= 90) return "A";
  if (score >= 75) return "B";
  if (score >= 60) return "C";
  if (score >= 40) return "D";
  return "F";
}
