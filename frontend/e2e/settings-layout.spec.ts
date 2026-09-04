import { expect, test, type Page, type Response } from "@playwright/test";

async function openSettledSettings(page: Page) {
  const responses: Response[] = [];
  const settingsLoaded = new Promise<void>((resolve) => {
    const onResponse = (response: Response) => {
      if (new URL(response.url()).pathname !== "/api/v1/settings") return;
      responses.push(response);
      if (responses.length < 2) return;
      page.off("response", onResponse);
      void Promise.all(responses.map((item) => item.finished())).then(() => {
        resolve();
      });
    };
    page.on("response", onResponse);
  });

  await page.goto("/settings");
  await settingsLoaded;
  await page.evaluate(
    () =>
      new Promise<void>((resolve) => {
        requestAnimationFrame(() => requestAnimationFrame(() => resolve()));
      }),
  );
  await expect(page.locator(".settings-loading")).toHaveCount(0);
}

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

    await nav.locator("button", { hasText: "Session Providers" }).click();
    await expect(page.getByRole("heading", { name: "Session Providers" })).toBeVisible();

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

  test("hides an unmatched panel without discarding its draft", async ({ page }) => {
    // App and SettingsPage both start an initial settings read. Wait for both
    // before editing so a late response cannot reset the unsaved draft.
    await openSettledSettings(page);

    const nav = page.getByRole("navigation", { name: "Settings" });
    await nav.locator("button", { hasText: "Terminal" }).click();
    await page.getByRole("radio", { name: "Custom", exact: true }).click();

    const binary = page.getByLabel("Terminal binary");
    await binary.fill("/usr/bin/kitty");

    const search = page.getByRole("searchbox", { name: "Search settings" });
    await search.fill("no such setting");

    await expect(page.getByText("No matching settings", { exact: true })).toBeVisible();
    await expect(page.getByRole("heading", { name: "Terminal" })).toHaveCount(0);
    await expect(page.locator("#terminal-bin")).toBeHidden();

    await search.fill("");

    await expect(page.getByRole("heading", { name: "Terminal" })).toBeVisible();
    await expect(binary).toHaveValue("/usr/bin/kitty");
  });
});
