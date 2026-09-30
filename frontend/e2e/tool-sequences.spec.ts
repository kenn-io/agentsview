import { test, expect, type Page } from "@playwright/test";

const SESSION_ID = "test-session-tool-sequences";

async function openSession(page: Page, width: number) {
  await page.setViewportSize({ width, height: 900 });
  await page.goto(`/sessions/${SESSION_ID}`, { waitUntil: "domcontentloaded" });
  const panel = page.locator(".tool-sequences-panel");
  await expect(panel).toBeVisible({ timeout: 10_000 });
  await expect(panel).toContainText("Observed tool sequences");
  return panel;
}

test("renders, expands, and navigates observed sequences at desktop, tablet, and phone widths", async ({
  page,
}, testInfo) => {
  await page.addInitScript(() => {
    localStorage.setItem("agentsview-signal-panel", "true");
  });

  for (const width of [1280, 768, 400]) {
    const panel = await openSession(page, width);
    const response = await page.request.get(`/api/v1/sessions/${SESSION_ID}/tool-sequences`);
    expect(response.ok()).toBe(true);
    const data = await response.json();
    expect(data).toMatchObject({
      session_id: SESSION_ID,
      total_tool_calls: 3,
      total_sequences: 1,
      total_sequence_calls: 3,
    });
    expect(data.sequences[0]).toMatchObject({
      ending: "recovered",
      identical: true,
      tool_changed: true,
      calls: [
        { ordinal: 1, tool_use_id: "grep-1", duration_ms: 2000 },
        { ordinal: 2, tool_use_id: "grep-2", duration_ms: null },
        { ordinal: 3, tool_use_id: "read-1", duration_ms: 2000 },
      ],
    });

    const sequence = panel.locator("details.sequence").first();
    const summary = sequence.locator("summary");
    await summary.focus();
    await expect(summary).toBeFocused();
    await summary.press("Enter");
    await expect(sequence).toHaveJSProperty("open", true);
    await expect(panel).toContainText("A later call returned content.");
    await expect(panel).toContainText("Same input as the previous call");
    await expect(panel).toContainText("A later call switched tools.");
    await expect(panel).toContainText("Not measured");
    await expect(panel.locator(".duration").first()).toHaveText("2.0s");
    await expect(panel.locator("pre").first()).toContainText("pattern");

    const scroller = page.locator(".message-list-scroll");
    await scroller.evaluate((element) => {
      element.scrollTop = element.scrollHeight;
    });
    await panel.getByRole("button", { name: "Open call 1 for Grep in the transcript" }).click();
    const target = scroller.locator(".virtual-row.selected");
    await expect(target).toHaveAttribute("data-index", String(data.sequences[0].calls[0].ordinal));
    await expect(target).toBeInViewport({ timeout: 10_000 });
    await expect(target).toContainText("Grep");

    const geometry = await panel.evaluate((element) => {
      const bounds = element.getBoundingClientRect();
      const transcript = document.querySelector<HTMLElement>(".message-list-scroll")!;
      return {
        left: bounds.left,
        right: bounds.right,
        height: bounds.height,
        clientWidth: element.clientWidth,
        scrollWidth: element.scrollWidth,
        transcriptHeight: transcript.getBoundingClientRect().height,
        viewportWidth: window.innerWidth,
      };
    });
    expect(geometry.left).toBeGreaterThanOrEqual(0);
    expect(geometry.right).toBeLessThanOrEqual(geometry.viewportWidth);
    expect(geometry.scrollWidth).toBeLessThanOrEqual(geometry.clientWidth);
    expect(geometry.height).toBeLessThanOrEqual(384);
    expect(geometry.transcriptHeight).toBeGreaterThan(100);
    await page.screenshot({ path: testInfo.outputPath(`tool-sequences-${width}.png`) });
  }
});

test("shows the empty state for a session with no messages", async ({ page }) => {
  await page.addInitScript(() => {
    localStorage.setItem("agentsview-signal-panel", "true");
  });

  await page.setViewportSize({ width: 768, height: 900 });
  await page.goto("/sessions/test-session-empty-0", { waitUntil: "domcontentloaded" });
  const panel = page.locator(".tool-sequences-panel");
  await expect(panel).toBeVisible({ timeout: 10_000 });
  const response = await page.request.get(
    "/api/v1/sessions/test-session-empty-0/tool-sequences",
  );
  expect(response.ok()).toBe(true);
  expect(await response.json()).toMatchObject({
    session_id: "test-session-empty-0",
    total_tool_calls: 0,
    total_sequences: 0,
  });
  await expect(panel).toContainText("No tool calls recorded.");
});
