import { test, expect, type Page } from "@playwright/test";

// The served fixture includes measured Bash and closed-child Task execution plus unknown Read calls.

const SHOWCASE = "test-session-duration-showcase";
const SHOWCASE_WORKTREE =
  "/workspace/مشروع/.worktrees/שלוםfeaturewithalongcheckoutnamefortooltipwrappingwithoutbreakopportunities";

// The conversation scrolls inside `.message-list-scroll`
// (`SessionsPage.scroller`). The plan's sketch referenced
// `.conv-body`, which does not exist in the live DOM.
const SCROLLER = ".message-list-scroll";

async function gotoShowcase(page: Page) {
  // Vitals panel defaults to closed. Open it via localStorage
  // so the panel renders on first paint.
  await page.addInitScript(() => {
    localStorage.setItem("agentsview-session-vitals", "true");
  });
  // The panel and its first call are the readiness contract below. Waiting
  // for WebKit's full load event also waits on unrelated page resources and
  // can exhaust the whole test timeout before either assertion runs.
  await page.goto(`/sessions/${SHOWCASE}`, {
    waitUntil: "domcontentloaded",
  });
  await expect(page.locator("aside.vitals")).toBeVisible({
    timeout: 5_000,
  });
  // Wait for timing data — the Calls section renders rows once
  // the API response lands. Without this, early assertions race
  // the fetch.
  await expect(page.locator(".calls .call").first()).toBeVisible({ timeout: 5_000 });
}

test.describe("Session Vital Signs", () => {
  test("renders all five sections", async ({ page }) => {
    await gotoShowcase(page);

    // Section labels are not semantic headings, so match the compact
    // section-header rows. Calls uses a disclosure button while the other
    // sections keep text-only labels.
    const headers = page
      .locator(".v-section .v-h")
      .filter({ hasText: /(Session|Turn activity|Time spent|Timeline|Calls)/ });
    await expect(headers).toHaveCount(5);

    await expect(page.locator(".v-section .v-h", { hasText: "Session" })).toBeVisible();
    await expect(page.locator(".v-section .v-h", { hasText: "Turn activity" })).toBeVisible();
    await expect(page.locator(".v-section .v-h", { hasText: "Time spent" })).toBeVisible();
    await expect(page.locator(".v-section .v-h", { hasText: "Timeline" })).toBeVisible();
    await expect(page.locator(".v-section .v-h", { hasText: "Calls" })).toBeVisible();
  });

  test("shows measured execution and unknown calls with horizontal activity tracks", async ({
    page,
  }, testInfo) => {
    await gotoShowcase(page);

    const response = await page.request.get(`/api/v1/sessions/${SHOWCASE}/timing`);
    expect(response.ok()).toBe(true);
    const timing = await response.json();
    expect(timing.tool_duration_ms).toBe(140_000);
    expect(timing.activity_totals).toEqual({
      tool_ms: 140_000,
      unattributed_ms: 35_000,
    });
    expect(timing.activity).toEqual([
      expect.objectContaining({
        ordinal: 0,
        duration_ms: 139_000,
        tool_ms: 120_000,
        unattributed_ms: 19_000,
        running: false,
      }),
      expect.objectContaining({
        ordinal: 5,
        duration_ms: 36_000,
        tool_ms: 20_000,
        unattributed_ms: 16_000,
        running: false,
      }),
    ]);

    const bash = page
      .locator(".calls .call")
      .filter({ has: page.locator(".cn", { hasText: "Bash" }) });
    await expect(bash.locator(".cd")).toHaveText("20.0s");
    const read = page
      .locator(".calls .call")
      .filter({ has: page.locator(".cn", { hasText: "Read" }) });
    await expect(read.locator(".cd")).toHaveText(["unknown", "unknown", "unknown"]);
    const task = page
      .locator(".calls .call")
      .filter({ has: page.locator(".cn", { hasText: "Task" }) });
    await expect(task.locator(".cd")).toHaveText("2m 0s");

    const activity = page.locator('.activity-row[data-activity-ordinal="5"]');
    await expect(activity.locator('[data-activity-kind="tool"]')).toHaveAttribute(
      "title",
      "Tool execution · 20.0s",
    );
    await expect(activity.locator('[data-activity-kind="unattributed"]')).toHaveAttribute(
      "title",
      "Unattributed · 16.0s",
    );
    const layout = await activity.locator(".activity-track").evaluate((element) => {
      const track = element.getBoundingClientRect();
      const row = element.closest(".activity-row")!;
      const rowBounds = row.getBoundingClientRect();
      const tool = element.querySelector('[data-activity-kind="tool"]')!.getBoundingClientRect();
      const unattributed = element
        .querySelector('[data-activity-kind="unattributed"]')!
        .getBoundingClientRect();
      return {
        display: getComputedStyle(element).display,
        clientWidth: element.clientWidth,
        scrollWidth: element.scrollWidth,
        rowClientWidth: row.clientWidth,
        rowScrollWidth: row.scrollWidth,
        rowLeft: rowBounds.left,
        rowRight: rowBounds.right,
        trackLeft: track.left,
        trackRight: track.right,
        toolLeft: tool.left,
        toolRight: tool.right,
        toolTop: tool.top,
        unattributedLeft: unattributed.left,
        unattributedRight: unattributed.right,
        unattributedTop: unattributed.top,
      };
    });
    expect(layout.display).toBe("flex");
    expect(layout.scrollWidth).toBeLessThanOrEqual(layout.clientWidth);
    expect(layout.rowScrollWidth).toBeLessThanOrEqual(layout.rowClientWidth);
    expect(layout.trackLeft).toBeGreaterThanOrEqual(layout.rowLeft);
    expect(layout.trackRight).toBeLessThanOrEqual(layout.rowRight);
    expect(layout.toolTop).toBe(layout.unattributedTop);
    expect(layout.toolLeft).toBeGreaterThanOrEqual(layout.trackLeft);
    expect(layout.toolRight).toBeCloseTo(layout.unattributedLeft, 0);
    expect(layout.unattributedRight).toBeLessThanOrEqual(layout.trackRight + 1);
    expect(layout.toolRight - layout.toolLeft).toBeGreaterThan(0);
    expect(layout.unattributedRight - layout.unattributedLeft).toBeGreaterThan(0);

    const scroller = page.locator(SCROLLER);
    await scroller.evaluate((element) => {
      element.scrollTop = 0;
    });
    await activity.click();
    await expect
      .poll(() => scroller.evaluate((element) => element.scrollTop), { timeout: 15_000 })
      .toBeGreaterThan(0);
    await expect(
      page.getByText("Confirm the slow path with the auth tests.", { exact: true }),
    ).toBeInViewport();
    await page.screenshot({ path: testInfo.outputPath("turn-activity.png"), fullPage: true });
  });

  test("keeps an absolute mixed-direction worktree path ordered", async ({ page }) => {
    await gotoShowcase(page);

    const path = page.locator(".context-value--path");
    // The worktree row renders only once the sidebar hydration delivers
    // session.cwd, which arrives independently of the timing fetch that
    // gotoShowcase waits on. On loaded CI runners hydration can trail the
    // timing data past the 5s expect default, so give it its own budget.
    await expect(path).toHaveText(SHOWCASE_WORKTREE, {
      timeout: 15_000,
    });

    const layout = await path.evaluate((element, expectedPath) => {
      const walker = document.createTreeWalker(element, NodeFilter.SHOW_TEXT);
      let textNode = walker.nextNode();
      while (textNode && !textNode.textContent?.includes(expectedPath)) {
        textNode = walker.nextNode();
      }
      if (!textNode?.textContent) {
        throw new Error("worktree path text node not found");
      }

      const start = textNode.textContent.indexOf(expectedPath);
      const firstCharacter = document.createRange();
      firstCharacter.setStart(textNode, start);
      firstCharacter.setEnd(textNode, start + 1);
      const lastCharacter = document.createRange();
      lastCharacter.setStart(textNode, start + expectedPath.length - 1);
      lastCharacter.setEnd(textNode, start + expectedPath.length);

      const container = element.getBoundingClientRect();
      const first = firstCharacter.getBoundingClientRect();
      const last = lastCharacter.getBoundingClientRect();
      return {
        overflows: element.scrollWidth > element.clientWidth,
        containerLeft: container.left,
        containerRight: container.right,
        firstRight: first.right,
        lastLeft: last.left,
        lastRight: last.right,
      };
    }, SHOWCASE_WORKTREE);

    expect(layout.overflows).toBe(true);
    expect(layout.firstRight).toBeLessThanOrEqual(layout.containerLeft);
    expect(layout.lastLeft).toBeGreaterThanOrEqual(layout.containerLeft);
    expect(layout.lastRight).toBeLessThanOrEqual(layout.containerRight);
  });

  test("uses available viewport width for the worktree tooltip", async ({ page }) => {
    await gotoShowcase(page);

    const panel = page.locator("aside.vitals");
    const path = page.locator(".context-value--path");
    const trigger = page.locator(".context-tooltip .kit-tooltip-trigger");
    await path.hover();

    const tooltip = page.getByRole("tooltip");
    await expect(tooltip).toHaveText(SHOWCASE_WORKTREE);
    const panelLeft = await panel.evaluate((element) => element.getBoundingClientRect().left);
    const wideLayout = await tooltip.evaluate((element) => {
      const range = document.createRange();
      range.selectNodeContents(element);
      const lineTops = Array.from(range.getClientRects(), (rect) => Math.round(rect.top));
      const rect = element.getBoundingClientRect();
      const arrowStyle = getComputedStyle(element, "::before");
      const arrowWidth = Number.parseFloat(arrowStyle.width);
      const arrowCenter =
        arrowStyle.left === "auto"
          ? rect.right - Number.parseFloat(arrowStyle.right) - arrowWidth / 2
          : rect.left + Number.parseFloat(arrowStyle.left) + arrowWidth / 2;
      return {
        arrowCenter,
        left: rect.left,
        lineCount: new Set(lineTops).size,
        right: rect.right,
        viewportWidth: window.innerWidth,
        width: rect.width,
      };
    });
    const triggerBounds = await trigger.boundingBox();
    expect(triggerBounds).not.toBeNull();

    expect(wideLayout.width).toBeLessThanOrEqual(wideLayout.viewportWidth * 0.5 + 1);
    expect(wideLayout.lineCount).toBe(1);
    expect(wideLayout.left).toBeGreaterThanOrEqual(0);
    expect(wideLayout.right).toBeLessThanOrEqual(wideLayout.viewportWidth);
    expect(wideLayout.left).toBeLessThan(panelLeft);
    expect(wideLayout.arrowCenter).toBeGreaterThanOrEqual(triggerBounds!.x);
    expect(wideLayout.arrowCenter).toBeLessThanOrEqual(triggerBounds!.x + triggerBounds!.width);

    await page.setViewportSize({ width: 1000, height: 900 });
    await expect(path).toBeVisible();
    await path.hover();
    const narrowLayout = await tooltip.evaluate((element) => {
      const range = document.createRange();
      range.selectNodeContents(element);
      const lineTops = Array.from(range.getClientRects(), (rect) => Math.round(rect.top));
      const rect = element.getBoundingClientRect();
      return {
        clientWidth: element.clientWidth,
        left: rect.left,
        lineCount: new Set(lineTops).size,
        right: rect.right,
        scrollWidth: element.scrollWidth,
        viewportWidth: window.innerWidth,
        width: rect.width,
      };
    });

    expect(narrowLayout.width).toBeLessThanOrEqual(narrowLayout.viewportWidth * 0.5 + 1);
    expect(narrowLayout.lineCount).toBeGreaterThan(1);
    expect(narrowLayout.scrollWidth).toBeLessThanOrEqual(narrowLayout.clientWidth + 1);
    expect(narrowLayout.left).toBeGreaterThanOrEqual(0);
    expect(narrowLayout.right).toBeLessThanOrEqual(narrowLayout.viewportWidth);
  });

  test("slowest-call link scrolls the conversation", async ({ page }) => {
    await gotoShowcase(page);

    const scroller = page.locator(SCROLLER);
    // Reset to top so the click-induced scroll is observable.
    // (Default scrollTop is already 0 for this fixture, but be
    // explicit to avoid coupling to startup state.)
    await scroller.evaluate((el) => {
      el.scrollTop = 0;
    });

    await page.locator(".stat-grid .val-link").click();

    // ui.scrollToOrdinal sets pending state; the conversation
    // scrolls once MessageList processes the request.
    await expect
      .poll(() => scroller.evaluate((el) => el.scrollTop), {
        timeout: 3_000,
      })
      .toBeGreaterThan(0);
  });

  test("Time spent row click filters siblings", async ({ page }) => {
    await gotoShowcase(page);

    // Bash exists in the showcase fixture — pick it as the
    // active filter target. (Plan sketch suggested Bash.)
    const bashRow = page
      .locator(".agg-row")
      .filter({ has: page.locator(".agg-name", { hasText: "Bash" }) });
    const taskRow = page
      .locator(".agg-row")
      .filter({ has: page.locator(".agg-name", { hasText: "Task" }) });

    await expect(bashRow).toHaveCount(1);
    await expect(taskRow).toHaveCount(1);

    await bashRow.click();

    await expect(bashRow).toHaveClass(/\bactive\b/);
    await expect(taskRow).toHaveClass(/\bdimmed\b/);

    // Filter chip lives in the Time-spent section header.
    const chip = page.locator(".filter-chip");
    await expect(chip).toBeVisible();
    await expect(chip).toContainText("Bash");

    // Clear via the × inside the chip.
    await chip.locator(".x").click();

    await expect(page.locator(".filter-chip")).toHaveCount(0);
    await expect(taskRow).not.toHaveClass(/\bdimmed\b/);
    await expect(bashRow).not.toHaveClass(/\bactive\b/);
  });

  test("clicking a slow call scrolls the conversation", async ({ page }) => {
    await gotoShowcase(page);

    const scroller = page.locator(SCROLLER);
    await scroller.evaluate((el) => {
      el.scrollTop = 0;
    });

    // The measured Bash call is the only call eligible for the slow marker.
    const slowCall = page.locator(".call.slow").first();
    await expect(slowCall).toBeVisible();
    await slowCall.locator(".cn").click();

    await expect
      .poll(() => scroller.evaluate((el) => el.scrollTop), {
        timeout: 3_000,
      })
      .toBeGreaterThan(0);
  });

  test("sub-agent expands and collapses inline via chevron", async ({ page }) => {
    await gotoShowcase(page);

    // The sub-agent lives on the Task call inside the parallel
    // group (`.cgroup`). CallRow renders `button.chev` only for
    // calls with a subagent_session_id.
    const taskRow = page.locator(".cgroup .call").filter({
      has: page.locator(".cn", { hasText: "Task" }),
    });
    await expect(taskRow).toHaveCount(1);
    const chev = taskRow.locator("button.chev");
    await expect(chev).toBeVisible();
    await expect(chev).toHaveAttribute("aria-expanded", "false");

    await chev.click();

    const saExpand = page.locator(".sa-expand");
    await expect(saExpand).toBeVisible();
    // The expanded state is mirrored on the chevron button so
    // assistive tech sees the toggle work.
    await expect(chev).toHaveAttribute("aria-expanded", "true");

    // Collapse — re-clicking the chevron tears down the panel.
    await chev.click();
    await expect(saExpand).toHaveCount(0);
    await expect(chev).toHaveAttribute("aria-expanded", "false");
  });
});
