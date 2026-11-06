import { expect, test } from "@playwright/test";
import type { DbMessage as Message } from "../src/lib/api/generated/index.js";
import { serveTranscript, transcriptMessage } from "./helpers/mock-transcript";

// A fixture session whose messages are replaced with one edit, so the
// diff strip is the only tool block on screen.
const SESSION_ID = "test-session-xlarge-5500";
const CONTEXT =
  "func (s *Server) handleSessionTranscriptExportWithAttachmentsAndRedactionOptions(ctx context.Context, request *ExportRequest) error {";

const message = (ordinal: number, extra: Partial<Message>) =>
  transcriptMessage(SESSION_ID, ordinal, extra);

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
  await serveTranscript(page, SESSION_ID, messages);
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
