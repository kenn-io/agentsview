import { expect, test, type Browser } from "@playwright/test";
import type {
  DbMessage as Message,
  DbToolCall as ToolCall,
} from "../src/lib/api/generated/index.js";
import { serveTranscript, transcriptMessage } from "./helpers/mock-transcript";

// A fixture session whose messages are replaced with a run of single-call
// turns (a tool group) and one turn with three calls (a parallel group),
// so both kinds of row are measured on every run.
const SESSION_ID = "test-session-xlarge-5500";

function bash(command: string, callIndex = 0): ToolCall {
  return {
    call_index: callIndex,
    tool_name: "Bash",
    category: "Bash",
    input_json: JSON.stringify({ command }),
  };
}

const message = (ordinal: number, extra: Partial<Message>) =>
  transcriptMessage(SESSION_ID, ordinal, extra);

const messages: Message[] = [
  message(0, { role: "user", content: "Run the checks", content_length: 14 }),
  message(1, { has_tool_use: true, tool_calls: [bash("make lint")] }),
  message(2, { has_tool_use: true, tool_calls: [bash("make test")] }),
  message(3, { has_tool_use: true, tool_calls: [bash("make build")] }),
  message(4, { content: "Now in parallel.", content_length: 16 }),
  message(5, {
    has_tool_use: true,
    tool_calls: [bash("go vet ./...", 0), bash("npm run check", 1), bash("git status", 2)],
  }),
];

async function rowHeights(browser: Browser, layout: string) {
  const page = await browser.newPage();
  await page.addInitScript((value) => {
    localStorage.setItem("agentsview-message-layout", value);
  }, layout);
  await serveTranscript(page, SESSION_ID, messages);
  await page.goto(`/sessions/${SESSION_ID}`);
  const grouped = page.locator(".tool-group-body .tool-header-row:not(.pg-members *)");
  const parallel = page.locator(".pg-members .tool-header-row");
  await expect(grouped).toHaveCount(3);
  await expect(parallel).toHaveCount(3);
  const heights = (rows: typeof grouped) =>
    rows.evaluateAll((els) => els.map((el) => el.getBoundingClientRect().height));
  const result = { grouped: await heights(grouped), parallel: await heights(parallel) };
  await page.close();
  return result;
}

test("Compact packs grouped and parallel tool rows tighter than Default", async ({ browser }) => {
  const defaults = await rowHeights(browser, "default");
  const compact = await rowHeights(browser, "compact");

  for (const kind of ["grouped", "parallel"] as const) {
    expect(compact[kind]).toHaveLength(defaults[kind].length);
    defaults[kind].forEach((height, i) => {
      // Compact trims a pixel from each side of every row.
      expect(height - compact[kind][i]!, `${kind} row ${i}`).toBeCloseTo(2, 1);
    });
  }
});
