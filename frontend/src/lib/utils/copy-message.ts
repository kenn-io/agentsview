import type { DbMessage as Message } from "../api/generated/index.js";
import { parseToolInput } from "../search/tool-input.js";
import { extractToolParamMeta, generateFallbackContent } from "./tool-params.js";

/**
 * Format a message for clipboard copy, including tool call content.
 */
export function formatMessageForCopy(message: Message): string {
  const parts: string[] = [];

  if (message.content) {
    parts.push(message.content);
  }

  if (message.tool_calls?.length) {
    for (const tc of message.tool_calls) {
      const input = parseToolInput(tc.input_json);
      const params = input.params ?? {};
      const meta = extractToolParamMeta(tc.category ?? "", params) ?? [];
      const metaStr = meta.map((m) => `${m.label}: ${m.value}`).join(" | ");
      const header = metaStr ? `[${tc.tool_name}] ${metaStr}` : `[${tc.tool_name}]`;

      parts.push(header);

      const body = input.raw ?? generateFallbackContent(tc.tool_name, params);
      if (body) parts.push(body);

      if (tc.result_content) parts.push(tc.result_content);
    }
  }

  return parts.join("\n\n");
}
