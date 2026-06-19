import type { AutomatedScope } from "./analytics.js";

export interface Insight {
  id: number;
  type: InsightType;
  date_from: string;
  date_to: string;
  project: string | null;
  agent: string;
  model: string | null;
  prompt: string | null;
  content: string;
  kind?: CannedInsightKind | "";
  schema_version?: string;
  template_id?: string;
  template_version?: string;
  aggregate_hash?: string;
  cache_key?: string;
  cache_status?: "fresh" | "hit" | "";
  provenance_json?: string;
  structured_json?: string;
  created_at: string;
}

export type InsightType =
  | "daily_activity"
  | "agent_analysis"
  | "llm_canned";

export type CannedInsightKind =
  | "prompt_maturity_review"
  | "context_setup_review"
  | "workflow_hygiene_review"
  | "tool_reliability_review"
  | "model_cost_review"
  | "instruction_opportunity_review";

export interface InsightsResponse {
  insights: Insight[];
}

export type AgentName =
  | "claude"
  | "codex"
  | "copilot"
  | "gemini"
  | "kiro";

export interface InsightGenerationFilters {
  timezone: string;
  machine?: string;
  agent?: string;
  termination?: string;
  min_user_messages?: number;
  include_one_shot: boolean;
  automated_scope?: AutomatedScope;
  active_since?: string;
}

export interface GenerateInsightRequest {
  type: InsightType;
  date_from: string;
  date_to: string;
  project?: string;
  prompt?: string;
  agent?: AgentName;
  // IANA timezone the date range is expressed in, so the server's activity
  // summary covers the same local-day window as the dashboard. Omit for UTC.
  timezone?: string;
  kind?: CannedInsightKind;
  llm_opt_in?: boolean;
  force_refresh?: boolean;
  automated_scope?: AutomatedScope;
  filters?: InsightGenerationFilters;
}
