import type { DbMessage as Message } from "../api/generated/index.js";
import { extractToolParamMeta, generateFallbackContent } from "./tool-params.js";
import { messageSegments } from "./content-parser.js";
import { displayToolResult } from "./toolDisplay.js";

/**
 * Format a message for clipboard copy, including tool call content.
 */
export function formatMessageForCopy(message: Message): string {
  const parts: string[] = [];

  if (message.content_layout != null) {
    for (const segment of messageSegments(message)) {
      if (segment.type === "thinking") {
        parts.push(`[Thinking]\n${segment.content}\n[/Thinking]`);
      } else if (segment.type === "tool_result") {
        parts.push(displayToolResult(segment.content));
      } else if (segment.type === "tool") {
        const call = segment.toolCall!;
        parts.push(
          call.rendering || `[${call.tool_name}]${segment.content ? `\n${segment.content}` : ""}`,
        );
        if (call.result_events?.length) {
          for (const event of call.result_events) parts.push(displayToolResult(event.content));
        } else if (call.result_content) {
          parts.push(displayToolResult(call.result_content));
        }
      } else {
        parts.push(segment.copyContent ?? segment.content);
      }
    }
    return parts.filter((part) => part !== "").join("\n\n");
  }

  if (message.content) {
    parts.push(message.content);
  }

  if (message.tool_calls?.length) {
    for (const tc of message.tool_calls) {
      let params: Record<string, unknown> = {};
      if (tc.input_json) {
        try {
          params = JSON.parse(tc.input_json);
        } catch {
          // input_json may not be valid JSON for some tools
        }
      }
      const meta = extractToolParamMeta(tc.category ?? "", params) ?? [];
      const metaStr = meta.map((m) => `${m.label}: ${m.value}`).join(" | ");
      const header = metaStr ? `[${tc.tool_name}] ${metaStr}` : `[${tc.tool_name}]`;

      parts.push(header);

      const body = generateFallbackContent(tc.tool_name, params);
      if (body) parts.push(body);

      if (tc.result_content) parts.push(tc.result_content);
    }
  }

  return parts.join("\n\n");
}
