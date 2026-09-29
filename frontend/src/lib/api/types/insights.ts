export type InsightType = "daily_activity" | "agent_analysis" | "llm_canned";

export type CannedInsightKind =
  | "prompt_maturity_review"
  | "context_setup_review"
  | "workflow_hygiene_review"
  | "tool_reliability_review"
  | "model_cost_review"
  | "instruction_opportunity_review";

/** Agent CLIs an insight request can name, mirroring the server set. */
export const AGENT_NAMES = ["claude", "codex", "copilot", "gemini", "kiro"] as const;

export type AgentName = (typeof AGENT_NAMES)[number];
