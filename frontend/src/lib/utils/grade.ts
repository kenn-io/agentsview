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

const outcomeLabels: Record<string, string> = {
  completed: "Completed",
  abandoned: "Abandoned",
  errored: "Errored",
  unknown: "Outcome unknown",
};

export function getOutcomeColor(outcome: string): string {
  return outcomeColors[outcome] ?? "var(--text-muted)";
}

export function getOutcomeLabel(outcome: string): string {
  return outcomeLabels[outcome] ?? outcome;
}

const penaltyLabels: Record<string, string> = {
  outcome_errored: "outcome (errored)",
  outcome_abandoned: "outcome (abandoned)",
  tool_failure_signals: "tool failures",
  tool_retries: "retries",
  edit_churn: "edit churn",
  consecutive_failures: "failure streak",
  compactions: "compactions",
  mid_task_compactions: "mid-task compactions",
  context_pressure_high: "context pressure",
  repeated_failing_tool_cycles: "repeated failing tool cycles",
  runaway_loop: "repeated failing tool cycles",
  constraintless_first_prompt: "unstructured task start",
  missing_success_criteria: "missing success criteria",
  stuck_repeated_prompts: "stuck repeated prompts",
  repeated_prompts: "stuck repeated prompts",
  };

export function getPenaltyLabel(key: string): string {
  return penaltyLabels[key] ?? key;
}

const basisLabels: Record<string, string> = {
  outcome: "Outcome",
  tool_health: "Tool health",
  context_pressure: "Context pressure",
  prompt_quality: "Prompt quality",
  context_quality: "Context quality",
  workflow_quality: "Workflow quality",
};

export function getBasisLabel(key: string): string {
  return basisLabels[key] ?? key;
}

export function scoreToGrade(score: number): string {
  if (score >= 90) return "A";
  if (score >= 75) return "B";
  if (score >= 60) return "C";
  if (score >= 40) return "D";
  return "F";
}
