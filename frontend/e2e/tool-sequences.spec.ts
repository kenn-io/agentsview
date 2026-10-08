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
    const header = panel.locator(".panel-head");
    await expect(header).toContainText("3 calls in sequences");
    await expect(header).toContainText("3 tool calls in session");
    const summary = panel.locator(".sequence-row").first();
    await expect(summary).toContainText("Messages 1–3");
    await expect(summary).toContainText("Recovered");
    await expect(summary.locator(".step").first()).toContainText("×2");
    await summary.focus();
    await expect(summary).toBeFocused();
    await summary.press("Enter");
    await expect(summary).toHaveAttribute("aria-expanded", "true");
    await expect(panel).toContainText("A later call returned content.");
    await expect(panel).toContainText("same input");
    await expect(panel).toContainText("Tool switched");
    await expect(panel.locator(".call")).toHaveCount(3);

    const callRow = panel.locator(".call-row").first();
    await callRow.press("Enter");
    await expect(callRow).toHaveAttribute("aria-expanded", "true");
    const preview = panel.locator("pre").first();
    await expect(preview).toContainText("pattern");
    await preview.focus();
    await expect(preview).toBeFocused();
    // The duration column only fits a panel wider than 820px; below that the expanded call carries the timing.
    const durationColumn = await panel.locator(".dur").first().isVisible();
    if (width < 820) expect(durationColumn).toBe(false);
    if (durationColumn) {
      await expect(panel.locator(".dur").first()).toHaveText("2.0s");
      await expect(panel.locator(".dur").nth(1)).toHaveAttribute("title", "Not measured");
      await expect(panel.locator(".ev-duration").first()).toBeHidden();
    } else {
      const duration = panel.locator(".call").first().locator(".ev-duration");
      await expect(duration).toBeVisible();
      await expect(duration).toContainText("Duration");
      await expect(duration).toContainText("2.0s");
      await panel.locator(".call-row").nth(1).press("Enter");
      const unmeasured = panel.locator(".call").nth(1).locator(".ev-duration");
      await expect(unmeasured).toBeVisible();
      await expect(unmeasured).toContainText("Not measured");
      await panel.locator(".call-row").nth(1).press("Enter");
      await expect(unmeasured).toHaveCount(0);
    }

    const scroller = page.locator(".message-list-scroll");
    await scroller.evaluate((element) => {
      element.scrollTop = element.scrollHeight;
    });
    await panel
      .getByRole("link", { name: "Message 1: open the Grep call in the transcript" })
      .click();
    const target = scroller.locator(".virtual-row.selected");
    await expect(target).toHaveAttribute("data-index", "1");
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
        clipped: [
          ...element.querySelectorAll<HTMLElement>(".box, .sequence-row, .call-line"),
        ].filter((child) => child.scrollWidth > child.clientWidth).length,
        transcriptHeight: transcript.getBoundingClientRect().height,
        viewportWidth: window.innerWidth,
      };
    });
    expect(geometry.left).toBeGreaterThanOrEqual(0);
    expect(geometry.right).toBeLessThanOrEqual(geometry.viewportWidth);
    expect(geometry.scrollWidth).toBeLessThanOrEqual(geometry.clientWidth);
    expect(geometry.clipped).toBe(0);
    expect(geometry.height).toBeLessThanOrEqual(384);
    expect(geometry.transcriptHeight).toBeGreaterThan(100);
    await page.screenshot({ path: testInfo.outputPath(`tool-sequences-${width}.png`) });
  }
});

test("opens a sequence link in a fresh page and refuses one whose call moved", async ({ page }) => {
  await page.addInitScript(() => {
    localStorage.setItem("agentsview-signal-panel", "true");
  });
  const panel = await openSession(page, 1280);
  await panel.locator(".sequence-row").first().click();
  const href = await panel
    .getByRole("link", { name: "Message 1: open the Grep call in the transcript" })
    .getAttribute("href");
  expect(href).not.toBeNull();

  // A link opened in another tab loads the transcript fresh and still finds its call.
  await page.goto(href!, { waitUntil: "domcontentloaded" });
  const target = page.locator(".message-list-scroll .virtual-row.selected");
  await expect(target).toHaveAttribute("data-index", "1", { timeout: 10_000 });
  await expect(target).toContainText("Grep");

  // A link whose ordinal now holds a different call selects nothing.
  const moved = new URL(href!, page.url());
  moved.searchParams.set("tool_use_id", "a-call-that-moved");
  await page.goto(moved.pathname + moved.search, { waitUntil: "domcontentloaded" });
  await expect(page.locator(".message-list-scroll .virtual-row").first()).toBeVisible({
    timeout: 10_000,
  });
  await expect(page.locator(".message-list-scroll .virtual-row.selected")).toHaveCount(0);
});

test("shows the empty state for a session with no messages", async ({ page }) => {
  await page.addInitScript(() => {
    localStorage.setItem("agentsview-signal-panel", "true");
  });

  await page.setViewportSize({ width: 768, height: 900 });
  await page.goto("/sessions/test-session-empty-0", { waitUntil: "domcontentloaded" });
  const panel = page.locator(".tool-sequences-panel");
  await expect(panel).toBeVisible({ timeout: 10_000 });
  await expect(panel).toContainText("No tool calls recorded in this session.");
});
