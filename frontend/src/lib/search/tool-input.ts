/** Shared input text resolution for tool rendering and session search. */
import type { ToolCall } from "../api/types.js";
import { generateFallbackContent } from "../utils/tool-params.js";

export interface ToolInput {
  isTask: boolean;
  taskPrompt: string | null;
  fallbackContent: string | null;
  text: string;
}

/** Resolve the same prompt/category/name precedence used by ToolBlock. */
export function resolveToolInput(
  toolCall: ToolCall | undefined,
  segmentContent: string,
): ToolInput {
  const isTask =
    toolCall?.tool_name === "Task" ||
    toolCall?.tool_name === "Agent" ||
    toolCall?.category === "Task" ||
    (toolCall?.tool_name.includes("subagent") ?? false);
  let params: Record<string, unknown> | null = null;
  if (toolCall?.input_json) {
    try {
      const parsed: unknown = JSON.parse(toolCall.input_json);
      if (parsed !== null && typeof parsed === "object" && !Array.isArray(parsed)) {
        params = parsed as Record<string, unknown>;
      }
    } catch {
      // A malformed input retains the legacy segment text.
    }
  }
  const taskPrompt = isTask && typeof params?.prompt === "string" ? params.prompt : null;
  let fallbackContent: string | null = null;
  if (!segmentContent && params && toolCall) {
    const category = toolCall.category || null;
    fallbackContent =
      (category ? generateFallbackContent(category, params) : null) ??
      generateFallbackContent(toolCall.tool_name, params);
  }
  return {
    isTask,
    taskPrompt,
    fallbackContent,
    // An empty Task prompt takes the ordinary-content branch in ToolBlock.
    text: taskPrompt || (fallbackContent ?? segmentContent),
  };
}

/** Return the full searchable input before the component's preview limit. */
export function resolveToolInputText(
  toolCall: ToolCall | undefined,
  segmentContent: string,
): string {
  return resolveToolInput(toolCall, segmentContent).text;
}
