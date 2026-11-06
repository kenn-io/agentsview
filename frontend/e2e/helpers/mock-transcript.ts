import type { Page } from "@playwright/test";
import type { DbMessage as Message } from "../../src/lib/api/generated/index.js";

/** A transcript message with empty defaults for the fields a test omits. */
export function transcriptMessage(
  sessionId: string,
  ordinal: number,
  extra: Partial<Message> = {},
): Message {
  return {
    id: ordinal + 1,
    session_id: sessionId,
    ordinal,
    role: "assistant",
    content: "",
    content_length: 0,
    timestamp: "2026-01-01T00:00:00Z",
    has_thinking: false,
    thinking_text: "",
    has_tool_use: false,
    model: "",
    context_tokens: 0,
    output_tokens: 0,
    has_context_tokens: false,
    has_output_tokens: false,
    is_system: false,
    ...extra,
  };
}

/**
 * Serves `messages` as a fixture session's whole transcript. It answers
 * the reads the app makes the way the server does: ascending or
 * descending (newest first, as the app reads a large session) from
 * `from`, up to `limit`. Older reads past the start come back empty, so
 * the app sees the transcript once, in order.
 */
export async function serveTranscript(page: Page, sessionId: string, messages: Message[]) {
  await page.route(`**/api/v1/sessions/${sessionId}/messages*`, async (route) => {
    const url = new URL(route.request().url());
    const descending = url.searchParams.get("direction") === "desc";
    const last = messages.length - 1;
    const from = Number(url.searchParams.get("from") ?? (descending ? last : 0));
    const limit = Number(url.searchParams.get("limit") ?? 1000);
    const selected = descending
      ? messages.filter((m) => m.ordinal <= from).slice(-limit).reverse()
      : messages.filter((m) => m.ordinal >= from).slice(0, limit);
    await route.fulfill({ json: { messages: selected, count: selected.length } });
  });
}
