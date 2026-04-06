import { test, expect } from "@playwright/test";
import { SessionsPage } from "./pages/sessions-page";

test.describe("Activity Minimap", () => {
  let sp: SessionsPage;

  test.beforeEach(async ({ page }) => {
    sp = new SessionsPage(page);
    await sp.goto();
    await sp.selectSession(0);
  });

  test("minimap is hidden by default", async ({
    page,
  }) => {
    await expect(
      page.locator(".activity-minimap"),
    ).not.toBeVisible();
  });

  test("toggle button shows and hides minimap", async ({
    page,
  }) => {
    await page.locator(".minimap-btn").click();
    await expect(
      page.locator(".activity-minimap"),
    ).toBeVisible();

    await page.locator(".minimap-btn").click();
    await expect(
      page.locator(".activity-minimap"),
    ).not.toBeVisible();
  });

  test("minimap shows bars for sessions with timestamps", async ({
    page,
  }) => {
    await page.locator(".minimap-btn").click();

    await expect(
      page.locator(".minimap-status"),
    ).not.toBeVisible({ timeout: 3000 });

    const bars = page.locator(".minimap-bar");
    const count = await bars.count();
    expect(count).toBeGreaterThan(0);
  });

  test("clicking a bar scrolls the message list", async ({
    page,
  }) => {
    await page.locator(".minimap-btn").click();

    const clickableBars = page.locator(
      "g.minimap-bar[role='button']",
    );
    await clickableBars.first().waitFor({ timeout: 3000 });

    const barCount = await clickableBars.count();
    expect(barCount).toBeGreaterThan(0);

    const scrollBefore = await sp.scroller.evaluate(
      (el) => el.scrollTop,
    );

    const lastBar = clickableBars.nth(barCount - 1);
    await lastBar.click();

    const isScrollable = await sp.scroller.evaluate(
      (el) => el.scrollHeight > el.clientHeight,
    );
    if (barCount > 1 && isScrollable) {
      await expect
        .poll(
          () =>
            sp.scroller.evaluate((el) => el.scrollTop),
          { timeout: 5000 },
        )
        .not.toBe(scrollBefore);
    }
  });

  test("active indicator moves after reopen without scroll", async ({
    page,
  }) => {
    // Open minimap, wait for multiple bars.
    await page.locator(".minimap-btn").click();
    const bars = page.locator("g.minimap-bar[role='button']");
    await bars.first().waitFor({ timeout: 3000 });

    const barCount = await bars.count();
    if (barCount < 2) {
      // Can't test indicator movement with < 2 bars.
      return;
    }

    // Record the x position of the active indicator.
    const indicator = page.locator(".bar-indicator");
    await expect(indicator).toBeVisible();
    const xBefore = await indicator.evaluate(
      (el) => el.getAttribute("x"),
    );

    // Close minimap, scroll to the bottom.
    await page.locator(".minimap-btn").click();
    await expect(
      page.locator(".activity-minimap"),
    ).not.toBeVisible();

    await sp.scroller.evaluate((el) => {
      el.scrollTop = el.scrollHeight;
    });

    // Reopen — indicator should appear at a different
    // position reflecting the new scroll location.
    await page.locator(".minimap-btn").click();
    await expect(indicator).toBeVisible({ timeout: 3000 });

    await expect
      .poll(
        () =>
          indicator.evaluate(
            (el) => el.getAttribute("x"),
          ),
        { timeout: 3000 },
      )
      .not.toBe(xBefore);
  });
});
