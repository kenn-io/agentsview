/** Shared input text resolution for tool rendering and session search. */
import type { DbToolCall as ToolCall } from "../api/generated/index.js";
import {
  extractToolParamMeta,
  generateFallbackContent,
  type MetaTag,
} from "../utils/tool-params.js";

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

function isTaskCall(toolCall: ToolCall | undefined): boolean {
  return (
    toolCall?.tool_name === "Task" ||
    toolCall?.tool_name === "Agent" ||
    toolCall?.category === "Task" ||
    (toolCall?.tool_name.includes("subagent") ?? false)
  );
}

function presentTags(tags: Array<MetaTag | false>): MetaTag[] | null {
  const kept = tags.filter((tag): tag is MetaTag => tag !== false);
  return kept.length ? kept : null;
}

/** Labeled fields a tool block shows above its input. In-session find
 *  indexes each value, so ToolBlock and the search index share this list. */
export function toolMetaTags(toolCall: ToolCall | undefined): MetaTag[] | null {
  const { params } = parseToolInput(toolCall?.input_json);
  if (!params || !toolCall) return null;
  const text = (value: unknown) => String(value);
  const task =
    isTaskCall(toolCall) &&
    presentTags([
      !!params.subagent_type && { label: "type", value: text(params.subagent_type) },
      !!params.description && { label: "description", value: text(params.description) },
    ]);
  const taskCreate =
    toolCall.tool_name === "TaskCreate" &&
    presentTags([
      !!params.subject && { label: "subject", value: text(params.subject) },
      !!params.description && { label: "description", value: text(params.description) },
    ]);
  const taskUpdate =
    toolCall.tool_name === "TaskUpdate" &&
    presentTags([
      !!params.taskId && { label: "task", value: `#${text(params.taskId)}` },
      !!params.status && { label: "status", value: text(params.status) },
      !!params.subject && { label: "subject", value: text(params.subject) },
    ]);
  return (
    task ||
    taskCreate ||
    taskUpdate ||
    extractToolParamMeta(toolCall.tool_name, params, toolCall.category)
  );
}

/** Resolve the same prompt/category/name precedence used by ToolBlock. */
export function resolveToolInput(
  toolCall: ToolCall | undefined,
  segmentContent: string,
): ToolInput {
  const isTask = isTaskCall(toolCall);
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
