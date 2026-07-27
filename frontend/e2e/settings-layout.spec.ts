import { expect, test } from "@playwright/test";

test.describe("Settings layout", () => {
  test("keeps navigation and actions fixed while panel content scrolls", async ({ page }) => {
    await page.setViewportSize({ width: 1280, height: 800 });
    await page.goto("/settings");

    const nav = page.getByRole("navigation", { name: "Settings" });
    await expect(nav).toBeVisible();

    const host = page.locator(".settings-page-host");
    await expect
      .poll(() => host.evaluate((element) => getComputedStyle(element).overflow))
      .toBe("hidden");
    await expect
      .poll(() => host.evaluate((element) => element.scrollHeight))
      .toBe(await host.evaluate((element) => element.clientHeight));

    await nav.locator("button", { hasText: "Agent Directories" }).click();
    await expect(page.getByRole("heading", { name: "Agent Directories" })).toBeVisible();

    const scroller = page.locator(".kit-settings__scroll");
    await expect
      .poll(() => scroller.evaluate((element) => element.scrollHeight > element.clientHeight))
      .toBe(true);
    await scroller.evaluate((element) => {
      element.scrollTop = element.scrollHeight;
    });
    await expect.poll(() => scroller.evaluate((element) => element.scrollTop)).toBeGreaterThan(0);
    await expect(page.getByRole("button", { name: "Full Resync" })).toBeVisible();

    await nav.locator("button", { hasText: "Terminal" }).click();
    await expect(page.getByRole("heading", { name: "Terminal" })).toBeVisible();
    await expect.poll(() => scroller.evaluate((element) => element.scrollTop)).toBe(0);
  });

  test("keeps settings operable at the narrow breakpoint", async ({ page }) => {
    await page.setViewportSize({ width: 700, height: 800 });
    await page.goto("/settings");

    const search = page.getByRole("searchbox", { name: "Search settings" });
    await expect(search).toBeVisible();

    const nav = page.getByRole("navigation", { name: "Settings" });
    await nav.locator("button", { hasText: "Terminal" }).click();
    await expect(page.getByRole("heading", { name: "Terminal" })).toBeVisible();
    await expect(page.getByRole("button", { name: "Full Resync" })).toBeVisible();
    await expect
      .poll(() => page.evaluate(() => document.documentElement.scrollWidth <= innerWidth))
      .toBe(true);
  });
});
