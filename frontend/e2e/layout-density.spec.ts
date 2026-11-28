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

for (const layout of ["stream", "skim"]) {
  test(`keeps message actions available in ${layout} layout`, async ({ page }) => {
    await page.setViewportSize({ width: 390, height: 844 });
    await page.addInitScript(
      (value) => localStorage.setItem("agentsview-message-layout", value),
      layout,
    );
    await serveTranscript(page, SESSION_ID, messages);
    await page.goto(`/sessions/${SESSION_ID}`);
    const card = page.locator(".message").filter({ hasText: "Run the checks" });
    await expect(card).toBeVisible();
    await card.hover();
    const link = card.getByRole("button", { name: "Copy link to message" });
    const pin = card.getByRole("button", { name: "Pin message" });
    await expect(link).toBeVisible();
    await expect(pin).toBeVisible();
    await expect(link).toHaveCSS("opacity", "1");
    const bounds = await card.evaluate((el) => {
      const content = el.querySelector(".text-content")!;
      const range = document.createRange();
      range.selectNodeContents(content);
      const text = range.getBoundingClientRect();
      const action = el
        .querySelector('[aria-label="Copy link to message"]')!
        .getBoundingClientRect();
      return {
        textRight: text.right,
        actionLeft: action.left,
        actionRight: action.right,
        viewport: innerWidth,
      };
    });
    expect(bounds.textRight).toBeLessThanOrEqual(bounds.actionLeft);
    expect(bounds.actionRight).toBeLessThanOrEqual(bounds.viewport);
    await link.focus();
    await expect(link).toBeFocused();
  });
}

test("keeps grouped-message links within the narrow viewport", async ({ page }) => {
  await page.setViewportSize({ width: 390, height: 844 });
  await serveTranscript(page, SESSION_ID, messages);
  await page.goto(`/sessions/${SESSION_ID}`);
  const group = page.locator(".tool-group").first();
  await expect(group).toBeVisible();
  await group.hover();
  const link = group.getByRole("button", { name: "Copy link to message" }).first();
  await expect(link).toBeVisible();
  const bounds = await link.evaluate((el) => {
    const rect = el.getBoundingClientRect();
    const clippedBy = [];
    for (let parent = el.parentElement; parent; parent = parent.parentElement) {
      const style = getComputedStyle(parent);
      if (!/(hidden|clip|auto|scroll)/.test(style.overflowX)) continue;
      const box = parent.getBoundingClientRect();
      if (rect.left < box.left || rect.right > box.right)
        clippedBy.push({ left: box.left, right: box.right });
    }
    return { left: rect.left, right: rect.right, viewport: innerWidth, clippedBy };
  });
  expect(bounds.left).toBeGreaterThanOrEqual(0);
  expect(bounds.right).toBeLessThanOrEqual(bounds.viewport);
  expect(bounds.clippedBy).toEqual([]);
});
