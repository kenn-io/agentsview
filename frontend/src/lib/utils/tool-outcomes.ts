import type { SessionToolSequenceCall } from "../api/generated/index.js";
import { m } from "../i18n/index.js";

export type ToolOutcome = SessionToolSequenceCall["outcome"];

export const TOOL_OUTCOMES: ToolOutcome[] = ["errored", "empty", "content", "unknown"];

/** data-kit-tone for each outcome; unknown outcomes draw a hollow dot instead. */
export const OUTCOME_TONES: Record<ToolOutcome, string | undefined> = {
  errored: "danger",
  empty: "warning",
  content: "success",
  unknown: undefined,
};

export function outcomeLabel(outcome: ToolOutcome): string {
  switch (outcome) {
    case "errored":
      return m.tool_sequences_outcome_errored();
    case "empty":
      return m.tool_sequences_outcome_empty();
    case "content":
      return m.tool_sequences_outcome_content();
    case "unknown":
      return m.tool_sequences_outcome_unknown();
  }
}
