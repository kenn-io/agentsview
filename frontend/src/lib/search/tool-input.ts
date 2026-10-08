/** Shared input text resolution for tool rendering and session search. */
import type { DbToolCall as ToolCall } from "../api/generated/index.js";
import { generateFallbackContent } from "../utils/tool-params.js";

export interface ToolInput {
  isTask: boolean;
  taskPrompt: string | null;
  fallbackContent: string | null;
  text: string;
}

export interface ParsedToolInput {
  params: Record<string, unknown> | null;
  raw: string | null;
}

/** Split input_json into object params or raw text such as a custom tool's script or patch. */
export function parseToolInput(inputJson: string | undefined): ParsedToolInput {
  if (!inputJson) return { params: null, raw: null };
  let parsed: unknown;
  try {
    parsed = JSON.parse(inputJson);
  } catch {
    return { params: null, raw: inputJson };
  }
  if (parsed === null || (Array.isArray(parsed) && parsed.length === 0)) {
    return { params: null, raw: null };
  }
  if (typeof parsed === "object" && !Array.isArray(parsed)) {
    return { params: parsed as Record<string, unknown>, raw: null };
  }
  return { params: null, raw: inputJson };
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
  const { params, raw: rawInput } = parseToolInput(toolCall?.input_json);
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
    text: taskPrompt || (fallbackContent ?? (segmentContent || rawInput || "")),
  };
}

/** Return the full searchable input before the component's preview limit. */
export function resolveToolInputText(
  toolCall: ToolCall | undefined,
  segmentContent: string,
): string {
  return resolveToolInput(toolCall, segmentContent).text;
}
