import type { Message } from "../api/types.js";
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
