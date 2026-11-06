import { expect, test } from "@playwright/test";
import type { DbMessage as Message } from "../src/lib/api/generated/index.js";

// A fixture session whose messages are replaced with one edit, so the
// diff strip is the only tool block on screen.
const SESSION_ID = "test-session-xlarge-5500";
const CONTEXT =
  "func (s *Server) handleSessionTranscriptExportWithAttachmentsAndRedactionOptions(ctx context.Context, request *ExportRequest) error {";

function message(ordinal: number, extra: Partial<Message>): Message {
  return {
    id: ordinal + 1,
    session_id: SESSION_ID,
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

test("a long hunk header wraps inside the diff strip", async ({ page }) => {
  const messages = [
    message(0, { role: "user", content: "Edit the handler", content_length: 16 }),
    message(1, {
      has_tool_use: true,
      tool_calls: [
        {
          tool_name: "apply_patch",
          category: "Edit",
          input_json: JSON.stringify({
            path: "internal/server/export.go",
            kind: { type: "update" },
            diff: `@@ -10,3 +10,4 @@ ${CONTEXT}\n-\treturn nil\n+\treturn s.export(ctx, request)\n`,
          }),
        },
      ],
    }),
  ];
  await page.route(`**/api/v1/sessions/${SESSION_ID}/messages*`, (route) =>
    route.fulfill({ json: { messages, count: messages.length } }),
  );
  await page.addInitScript(() => localStorage.setItem("agentsview-message-layout", "default"));
  await page.setViewportSize({ width: 1000, height: 800 });
  await page.goto(`/sessions/${SESSION_ID}`);

  const block = page.locator(".tool-block").first();
  await expect(block).toBeVisible();
  if ((await block.locator(".diff-view").count()) === 0) await block.locator(".tool-header").click();
  const hunk = block.locator(".diff-strip-hunk");
  await expect(hunk).toContainText("handleSessionTranscriptExport");

  const fit = await hunk.evaluate((el) => {
    const strip = el.closest(".tool-meta")!.getBoundingClientRect();
    const r = el.getBoundingClientRect();
    return {
      inside: r.left >= strip.left - 0.5 && r.right <= strip.right + 0.5,
      whole: el.scrollWidth <= el.clientWidth + 1,
      // More than one line: taller than two font sizes.
      wraps: r.height > parseFloat(getComputedStyle(el).fontSize) * 2,
    };
  });
  expect(fit, JSON.stringify(fit)).toEqual({ inside: true, whole: true, wraps: true });
});
