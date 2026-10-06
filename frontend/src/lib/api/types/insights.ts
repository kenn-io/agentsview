import type { DbInsight } from "../generated/index.js";

export type InsightType = "daily_activity" | "agent_analysis" | "llm_canned" | "tool_effectiveness";

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

export const TOOL_EFFECTIVENESS_SCHEMA_VERSION = "tool_effectiveness.v1";

export type ToolEffectivenessAssessment = "helped" | "did_not_help" | "unknown";

export interface ToolEffectivenessCallRef {
  ordinal: number;
  call_index: number;
}

export interface ToolEffectivenessConclusion {
  assessment: ToolEffectivenessAssessment;
  text: string;
  ordinals: number[];
  calls: ToolEffectivenessCallRef[];
}

export interface ToolEffectivenessOmission {
  reason: "budget" | "unretained" | "previews";
  ordinal?: number;
  call_index?: number;
  tool_name?: string;
  field?: "input" | "result";
  kept_bytes?: number;
  original_bytes?: number;
  count?: number;
}

/** Details of one cited call, saved with the report. */
export interface ToolEffectivenessCitedCall {
  ordinal: number;
  call_index: number;
  /** Lets a jump check that the message still holds this call; without it the jump goes by ordinal alone. */
  tool_use_id?: string;
  tool_name: string;
  input_preview: string;
  outcome: "errored" | "empty" | "content" | "unknown";
  result_bytes?: number;
  /** How many calls the cited message holds. */
  message_calls: number;
}

export interface ToolEffectivenessReport {
  session_id: string;
  call_count: number;
  conclusions: ToolEffectivenessConclusion[];
  omissions: ToolEffectivenessOmission[];
  transcript_revision?: string;
  termination_status?: string;
  cited_calls?: ToolEffectivenessCitedCall[];
}

const ASSESSMENTS = new Set(["helped", "did_not_help", "unknown"]);
const OMISSION_REASONS = new Set(["budget", "unretained", "previews"]);

function isInt(value: unknown): value is number {
  return Number.isInteger(value);
}

function isCallRef(value: unknown): value is ToolEffectivenessCallRef {
  const ref = value as ToolEffectivenessCallRef | null;
  return typeof ref === "object" && ref !== null && isInt(ref.ordinal) && isInt(ref.call_index);
}

function isConclusion(value: unknown): value is ToolEffectivenessConclusion {
  const c = value as ToolEffectivenessConclusion | null;
  return (
    typeof c === "object" &&
    c !== null &&
    ASSESSMENTS.has(c.assessment) &&
    typeof c.text === "string" &&
    Array.isArray(c.ordinals) &&
    c.ordinals.every(isInt) &&
    Array.isArray(c.calls) &&
    c.calls.every(isCallRef)
  );
}

const OUTCOMES = new Set(["errored", "empty", "content", "unknown"]);

function isCitedCall(value: unknown): value is ToolEffectivenessCitedCall {
  const c = value as ToolEffectivenessCitedCall | null;
  return (
    isCallRef(c) &&
    (c.tool_use_id === undefined || typeof c.tool_use_id === "string") &&
    typeof c.tool_name === "string" &&
    typeof c.input_preview === "string" &&
    OUTCOMES.has(c.outcome) &&
    (c.result_bytes === undefined || isInt(c.result_bytes)) &&
    isInt(c.message_calls)
  );
}

function isOmission(value: unknown): value is ToolEffectivenessOmission {
  const o = value as ToolEffectivenessOmission | null;
  return typeof o === "object" && o !== null && OMISSION_REASONS.has(o.reason);
}

/** Returns the saved tool-effectiveness report, or null when it is missing or unreadable. */
export function parseToolEffectivenessReport(item: DbInsight): ToolEffectivenessReport | null {
  if (
    item.type !== "tool_effectiveness" ||
    item.schema_version !== TOOL_EFFECTIVENESS_SCHEMA_VERSION
  ) {
    return null;
  }
  try {
    const raw = JSON.parse(item.structured_json ?? "") as ToolEffectivenessReport;
    if (
      typeof raw !== "object" ||
      raw === null ||
      typeof raw.session_id !== "string" ||
      raw.session_id === "" ||
      !isInt(raw.call_count) ||
      !Array.isArray(raw.conclusions) ||
      !raw.conclusions.every(isConclusion) ||
      !Array.isArray(raw.omissions) ||
      !raw.omissions.every(isOmission) ||
      (raw.transcript_revision !== undefined && typeof raw.transcript_revision !== "string") ||
      (raw.termination_status !== undefined && typeof raw.termination_status !== "string")
    ) {
      return null;
    }
    // Unreadable call details fall back to "Message N, call M" labels.
    if (!Array.isArray(raw.cited_calls) || !raw.cited_calls.every(isCitedCall)) {
      delete raw.cited_calls;
    }
    return raw;
  } catch {
    return null;
  }
}
