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
      await expect(rows.first()).toBeVisible();
      const count = await rows.count();
      expect(count).toBeGreaterThan(1);
      const colors = await rows.evaluateAll((items) => items.map((item) => item.querySelector("rect")?.getAttribute("fill") ?? item.querySelector(".list-dot")?.getAttribute("style")));
      await rows.first().scrollIntoViewIfNeeded();
      const geometry = (row: Element) => {
        const bounds = row.getBoundingClientRect();
        const panel = row.closest(".attribution-panel")!.getBoundingClientRect();
        return { width: bounds.width, height: bounds.height, x: bounds.x - panel.x, y: bounds.y - panel.y };
      };
      const bounds = await rows.first().evaluate(geometry);
      await rows.first().tap();
      await expect(rows.first()).toHaveAttribute("aria-pressed", "true");
      await expect(rows).toHaveCount(count);
      await expect(panel.locator(".dimmed").first()).toBeVisible();
      expect(await rows.first().evaluate(geometry)).toEqual(bounds);
      expect(await rows.evaluateAll((items) => items.map((item) => item.querySelector("rect")?.getAttribute("fill") ?? item.querySelector(".list-dot")?.getAttribute("style")))).toEqual(colors);
      await panel.getByRole("button", { name: "Open", exact: true }).tap();
      await expect(panel.getByRole("button", { name: "All projects" })).toBeVisible();
      await expect(rows.first()).toBeVisible();
      await panel.getByRole("button", { name: "All projects" }).tap();
      await expect(rows).toHaveCount(count);
      await expect(rows.first()).toHaveAttribute("aria-pressed", "true");
    });
  }
});

test.describe("Usage attribution selection", () => {
  test("selection clears in the header and double click opens the clicked project", async ({ page }) => {
    await page.goto("/usage");
    const panel = page.locator(".attribution-panel");
    const tiles = panel.locator(".tile");
    await expect(tiles.first()).toBeVisible();
    const label = await tiles.first().locator("text").first().textContent();
    await tiles.first().click();
    await expect(panel.getByRole("button", { name: "Clear selection", exact: true })).toBeVisible();
    await expect(panel.getByRole("button", { name: "Open", exact: true })).toBeVisible();
    await expect(page.getByRole("button", { name: `Project: ${label}`, exact: true })).toBeVisible();
    await panel.getByRole("button", { name: "Clear selection", exact: true }).click();
    await expect(panel.getByRole("button", { name: "Open", exact: true })).toBeHidden();
    await expect(panel.locator('.tile[aria-pressed="true"]')).toHaveCount(0);
    await expect(page.getByRole("button", { name: "Project: All", exact: true })).toBeVisible();
    const clickedLabel = await panel.locator(".rail-label").nth(1).textContent();
    await tiles.nth(1).dblclick();
    await expect(panel.getByRole("button", { name: "All projects" })).toBeVisible();
    await expect(panel.locator("h3")).toHaveText(clickedLabel!);
    await expect(panel.locator(".tile, .rail-row").first()).toBeVisible();
  });

  test("panel actions share styling and chart clear stays compact", async ({ page }, testInfo) => {
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
    await page.evaluate(() => {
      const comparison = document.createElement("div");
      comparison.id = "header-comparison";
      comparison.className = "usage-page";
      comparison.style.cssText = "position:fixed;inset:0 auto auto 0;width:1600px;display:grid;grid-template-columns:1fr 1fr;gap:16px;padding:16px;background:var(--bg-surface);z-index:10000";
      for (const [parentClass, headerSelector] of [["chart-container", ".chart-header"], ["attribution-panel", ".panel-header"]]) {
        const wrapper = document.createElement("div");
        wrapper.className = parentClass!;
        wrapper.append(document.querySelector(headerSelector!)!.cloneNode(true));
        comparison.append(wrapper);
      }
      document.body.append(comparison);
    });
    await page.locator("#header-comparison").screenshot({ path: testInfo.outputPath("usage-panel-and-chart-headers.png") });
  });

  test("brush then select keeps all attribution rows", async ({
    page,
  }) => {
    await page.goto("/usage");
    const panel = page.locator(".attribution-panel");
    await panel.getByRole("button", { name: "List", exact: true }).click();
    const rows = panel.locator(".list-row");
    await expect(rows.first()).toBeVisible();
    const brush = page.locator(".chart-container .chart-body").first();
    await brush.scrollIntoViewIfNeeded();
    const bounds = await brush.boundingBox();
    expect(bounds).not.toBeNull();
    const rangeResponse = page.waitForResponse(
      (response) => response.url().includes("/usage/summary") && response.ok(),
    );
    const y = bounds!.y + bounds!.height / 2;
    await page.mouse.move(bounds!.x + bounds!.width * 0.2, y);
    await page.mouse.down();
    await page.mouse.move(bounds!.x + bounds!.width * 0.75, y, { steps: 8 });
    await page.mouse.up();
    const brushedResponse = await rangeResponse;
    const count = (await brushedResponse.json()).projectTotals.length;
    await expect(page.locator(".chart-container").getByRole("button", { name: "Clear selection", exact: true })).toBeVisible();
    await expect(rows).toHaveCount(count);
    const narrowed = page.waitForResponse(
      (response) =>
        response.url().includes("/usage/summary") &&
        new URL(response.url()).searchParams.has("project_key") &&
        response.ok(),
    );
    await rows.first().click();
    await narrowed;
    await expect(rows.first()).toHaveAttribute("aria-pressed", "true");
    await expect(rows).toHaveCount(count);
    await expect(panel.locator(".dimmed").first()).toBeVisible();
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
