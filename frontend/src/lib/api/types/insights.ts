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
  created_at: string;
}

export type InsightType =
  | "daily_activity"
  | "agent_analysis";

export interface InsightsResponse {
  insights: Insight[];
}

export type AgentName =
  | "claude"
  | "codex"
  | "copilot"
  | "gemini"
  | "kiro";

export interface GenerateInsightRequest {
  type: InsightType;
  date_from: string;
  date_to: string;
  project?: string;
  prompt?: string;
  agent?: AgentName;
}
