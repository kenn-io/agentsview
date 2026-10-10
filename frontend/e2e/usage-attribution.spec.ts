import { test, expect, devices } from "@playwright/test";
import { clickNavTab } from "./helpers/nav";

test.describe("Usage attribution touch", () => {
  const { defaultBrowserType: _browser, ...iPhone } = devices["iPhone 13"];
  test.use(iPhone);

  for (const view of ["treemap", "list"] as const) {
    test(`Open opens a selected project in ${view}`, async ({ page }) => {
      await page.goto("/usage");
      const panel = page.locator(".attribution-panel");
      await expect(panel.locator(".tile").first()).toBeVisible();
      if (view === "list") await panel.getByRole("button", { name: "List", exact: true }).tap();
      const rows = panel.locator(view === "treemap" ? ".tile" : ".list-row");
      await rows.first().scrollIntoViewIfNeeded();
      await rows.first().tap();
      await expect(rows.first()).toHaveAttribute("aria-pressed", "true");
      await panel.getByRole("button", { name: "Open", exact: true }).tap();
      await expect(panel.getByRole("button", { name: "All projects" })).toBeVisible();
    });
  }
});

test.describe("Usage attribution selection", () => {
  for (const [view, selector, brushed] of [["treemap", ".tile", false], ["list", ".list-row", false], ["list", ".list-row", true]] as const) {
    test(`selecting keeps ${brushed ? "brushed " : ""}${view} geometry`, async ({ page }) => {
      // Summary cards wrap at this width, so a card dropping out on select would shift the panel.
      await page.setViewportSize({ width: 700, height: 900 });
      // A refresh includes both summaries and the comparison requests they start afterward.
      const refresh = (selected: boolean) => Promise.all(
        ["summary", "top-sessions", "comparison", "pairwise-comparison"].map(async (endpoint) => {
          const response = await page.waitForResponse((response) => {
            const url = new URL(response.url());
            return url.pathname === `/api/v1/usage/${endpoint}` && url.searchParams.has("project_key") === selected && response.ok();
          });
          await response.finished();
        }),
      );
      const loaded = refresh(false);
      await page.goto("/usage");
      await loaded;
      const panel = page.locator(".attribution-panel");
      await expect(panel.locator(".tile").first()).toBeVisible();
      if (view === "list") await panel.getByRole("button", { name: "List", exact: true }).click();
      if (brushed) {
        const brush = page.locator(".chart-container .chart-body").first();
        await brush.scrollIntoViewIfNeeded();
        const bounds = (await brush.boundingBox())!;
        const y = bounds.y + bounds.height / 2;
        await page.mouse.move(bounds.x + bounds.width * 0.2, y);
        await page.mouse.down();
        await page.mouse.move(bounds.x + bounds.width * 0.75, y, { steps: 8 });
        await page.mouse.up();
        await expect(page.locator(".chart-container").getByRole("button", { name: "Clear selection", exact: true })).toBeVisible();
      }
      await expect(page.locator(".usage-content")).toHaveAttribute("aria-busy", "false");
      const items = panel.locator(selector);
      // Content coordinates, so clicking's scroll into view and scroll anchoring cannot mask a shift.
      const geometry = () => items.evaluateAll((nodes) => {
        const content = document.querySelector(".usage-content")!;
        const top = content.getBoundingClientRect().top - content.scrollTop;
        return nodes.map((node) => {
          const { x, y, width, height } = node.getBoundingClientRect();
          return [x, y - top, width, height].map(Math.round);
        });
      });
      const before = await geometry();
      const narrowed = refresh(true);
      await items.nth(1).click();
      await narrowed;
      await expect(items.nth(1)).toHaveAttribute("aria-pressed", "true");
      await expect(page.locator(".usage-content")).toHaveAttribute("aria-busy", "false");
      expect(await geometry()).toEqual(before);
    });
  }

  test("panel actions share styling and chart clear stays compact", async ({ page }) => {
    await page.goto("/usage");
    const panel = page.locator(".attribution-panel");
    await expect(panel.locator(".tile").first()).toBeVisible();
    const chart = page.locator(".chart-container");
    const brush = chart.locator(".chart-body").first();
    await brush.scrollIntoViewIfNeeded();
    const bounds = (await brush.boundingBox())!;
    const y = bounds.y + bounds.height / 2;
    await page.mouse.move(bounds.x + bounds.width * 0.2, y);
    await page.mouse.down();
    await page.mouse.move(bounds.x + bounds.width * 0.75, y, { steps: 8 });
    await page.mouse.up();
    await expect(chart.getByRole("button", { name: "Clear selection", exact: true })).toBeVisible();
    await panel.locator(".tile").first().click();
    const panelClear = panel.getByRole("button", { name: "Clear selection", exact: true });
    await expect(panelClear).toBeVisible();
    const style = (button: Element) => {
      const css = getComputedStyle(button);
      return { height: css.height, padding: css.padding, font: css.font, background: css.backgroundColor, border: css.border, radius: css.borderRadius };
    };
    expect(await chart.getByRole("button", { name: "Clear selection", exact: true }).evaluate(style)).toEqual(expect.objectContaining({ height: "22px", padding: "0px 8px" }));
    expect(await panel.getByRole("button", { name: "Open", exact: true }).evaluate(style)).toEqual(await panelClear.evaluate(style));
  });

  test("Back and Forward follow page history and retain populated project rows", async ({ page }) => {
    await page.goto("/sessions");
    await clickNavTab(page, "Usage");
    const panel = page.locator(".attribution-panel");
    await expect(panel.locator(".tile").first()).toBeVisible();
    await panel.getByRole("button", { name: "List", exact: true }).click();
    await panel.locator(".list-row").first().dblclick();
    await expect(panel.getByRole("button", { name: "All projects" })).toBeVisible();
    await expect(panel.locator(".list-row").first()).toBeVisible();
    const usageURL = page.url();
    await page.goBack();
    await expect(page.locator(".usage-page")).toBeHidden();
    await expect(page).toHaveURL(/\/sessions/);
    await page.goForward();
    await expect(panel.getByRole("button", { name: "All projects" })).toBeVisible();
    await expect(panel.locator(".list-row").first()).toBeVisible();
    expect(page.url()).toBe(usageURL);
    await clickNavTab(page, "Sessions");
    await page.goBack();
    await expect(panel.getByRole("button", { name: "All projects" })).toBeVisible();
    await expect(panel.locator(".list-row").first()).toBeVisible();
    await page.goForward();
    await expect(page.locator(".usage-page")).toBeHidden();
  });
});
