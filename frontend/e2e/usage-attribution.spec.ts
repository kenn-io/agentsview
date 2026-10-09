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
      const bounds = await rows.first().boundingBox();
      await rows.first().tap();
      await expect(rows.first()).toHaveAttribute("aria-pressed", "true");
      await expect(rows).toHaveCount(count);
      await expect(panel.locator(".dimmed").first()).toBeVisible();
      expect(await rows.first().boundingBox()).toEqual(bounds);
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
  test("selection keeps tile positions, clears in the header and double click opens the clicked project", async ({ page }) => {
    await page.goto("/usage");
    const panel = page.locator(".attribution-panel");
    const tiles = panel.locator(".tile");
    await expect(tiles.first()).toBeVisible();
    const bounds = await tiles.evaluateAll((items) => items.map((item) => {
      const { x, y, width, height } = item.getBoundingClientRect();
      return { x, y, width, height };
    }));
    const label = await tiles.first().locator("text").first().textContent();
    await tiles.first().click();
    await expect(panel.getByRole("button", { name: "Clear selection", exact: true })).toBeVisible();
    await expect(panel.getByRole("button", { name: "Open", exact: true })).toBeVisible();
    await expect(page.getByRole("button", { name: `Project: ${label}`, exact: true })).toBeVisible();
    expect(await tiles.evaluateAll((items) => items.map((item) => {
      const { x, y, width, height } = item.getBoundingClientRect();
      return { x, y, width, height };
    }))).toEqual(bounds);
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

  test("model selection clears on reload and unchecking it shows remaining models", async ({ page }) => {
    await page.goto("/usage");
    const panel = page.locator(".attribution-panel");
    await expect(panel.locator(".tile").first()).toBeVisible();
    await panel.getByRole("button", { name: "Model", exact: true }).click();
    await panel.getByRole("button", { name: "List", exact: true }).click();
    const row = panel.locator(".list-row").first();
    await expect(row).toBeVisible();
    const model = await row.locator(".list-label").textContent();
    await row.click();
    await expect(row).toHaveAttribute("aria-pressed", "true");
    expect(new URL(page.url()).searchParams.has("model")).toBe(false);
    await page.reload();
    await expect(panel.locator(".list-row").first()).toBeVisible();
    await expect(panel.locator('.list-row[aria-pressed="true"]')).toHaveCount(0);
    await panel.locator(".list-row").filter({ hasText: model! }).click();
    await page.getByRole("button", { name: `Model: ${model}`, exact: true }).click();
    await page.locator(".kit-filter-dropdown__item").filter({ hasText: model! }).click();
    await expect(panel.getByRole("button", { name: "Clear selection", exact: true })).toBeHidden();
    await expect(panel.locator('.list-row[aria-pressed="true"]')).toHaveCount(0);
    await expect(panel.locator(".list-row").first()).toBeVisible();
    expect(new URL(page.url()).searchParams.has("model")).toBe(false);
    expect(new URL(page.url()).searchParams.get("exclude_model")).toBe(model);
  });

  test("unchecking a selected project clears its open view and keeps remaining projects", async ({ page }) => {
    await page.goto("/usage");
    const panel = page.locator(".attribution-panel");
    await expect(panel.locator(".tile").first()).toBeVisible();
    const label = await panel.locator(".tile text").first().textContent();
    await panel.locator(".tile").first().dblclick();
    await expect(panel.getByRole("button", { name: "All projects" })).toBeVisible();
    await page.getByRole("button", { name: `Project: ${label}`, exact: true }).click();
    await page.locator(".kit-filter-dropdown__item").filter({ hasText: label! }).click();
    await expect(panel.getByRole("button", { name: "All projects" })).toBeHidden();
    await expect(panel.getByRole("button", { name: "Clear selection", exact: true })).toBeHidden();
    await expect(panel.locator(".tile").first()).toBeVisible();
    await expect(panel.locator('.tile[aria-pressed="true"]')).toHaveCount(0);
  });

  test("two header agents both stay highlighted", async ({ page }) => {
    await page.goto("/usage?agent=claude,copilot");
    const panel = page.locator(".attribution-panel");
    await expect(panel.locator(".tile").first()).toBeVisible();
    await panel.getByRole("button", { name: "Agent", exact: true }).click();
    await panel.getByRole("button", { name: "List", exact: true }).click();
    await expect(panel.locator('.list-row[aria-pressed="true"]')).toHaveCount(2);
    await expect(panel.locator('.list-row[aria-pressed="true"] .list-label')).toHaveText(["claude", "copilot"]);
  });

  test("panel actions share styling and chart clear stays compact", async ({ page }, testInfo) => {
    await page.goto("/usage");
    const panel = page.locator(".attribution-panel");
    await expect(panel.locator(".tile").first()).toBeVisible();
    const chart = page.locator(".chart-container");
    const brush = chart.locator(".chart-body").first();
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

  test("brush then select keeps all attribution rows and narrows chart context", async ({
    page,
  }) => {
    await page.goto("/usage");
    const panel = page.locator(".attribution-panel");
    await panel.getByRole("button", { name: "List", exact: true }).click();
    const rows = panel.locator(".list-row");
    await expect(rows.first()).toBeVisible();
    const brush = page.locator(".chart-container .chart-body").first();
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
    const range = new URL(brushedResponse.url()).searchParams;
    const count = (await brushedResponse.json()).projectTotals.length;
    await expect(page.locator(".chart-container").getByRole("button", { name: "Clear selection", exact: true })).toBeVisible();
    await expect(rows).toHaveCount(count);
    const colors = await rows
      .locator(".list-dot")
      .evaluateAll((dots) => dots.map((dot) => dot.getAttribute("style")));
    const summaries: URL[] = [];
    page.on("request", (request) => {
      if (request.url().includes("/usage/summary")) summaries.push(new URL(request.url()));
    });
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
    expect(
      await rows
        .locator(".list-dot")
        .evaluateAll((dots) => dots.map((dot) => dot.getAttribute("style"))),
    ).toEqual(colors);
    await expect.poll(() => summaries.length).toBe(3);
    expect(summaries[0]!.searchParams.get("from")).toBe(range.get("from"));
    expect(summaries[0]!.searchParams.get("to")).toBe(range.get("to"));
    expect(summaries[1]!.searchParams.get("project_key")).toBe(
      summaries[0]!.searchParams.get("project_key"),
    );
    expect(summaries[2]!.searchParams.has("project_key")).toBe(false);

    await panel.getByRole("button", { name: "Agent", exact: true }).click();
    await expect(rows.first()).toBeVisible();
    await expect(page.getByRole("button", { name: "Refresh usage data", exact: true })).toBeEnabled();
    await expect(page.locator(".chart-container").getByRole("button", { name: "Clear selection", exact: true })).toBeVisible();
    summaries.length = 0;
    const agentResponse = page.waitForResponse(
      (response) =>
        response.url().includes("/usage/summary") &&
        new URL(response.url()).searchParams.has("agent") &&
        new URL(response.url()).searchParams.get("from") === range.get("from") &&
        response.ok(),
    );
    await rows.first().click();
    const agentRange = new URL((await agentResponse).url()).searchParams;
    await expect(rows.first()).toHaveAttribute("aria-pressed", "true");
    await expect(page.locator(".chart-container").getByRole("button", { name: "Clear selection", exact: true })).toBeVisible();
    expect(agentRange.get("from")).toBe(range.get("from"));
    expect(agentRange.get("to")).toBe(range.get("to"));
    expect(agentRange.has("project_key")).toBe(true);
    await expect.poll(() => summaries.length).toBe(3);
    expect(summaries[2]!.searchParams.has("agent")).toBe(false);
    expect(summaries[2]!.searchParams.has("project_key")).toBe(true);
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

  test("All projects and Escape keep the brush, dates and filters", async ({ page }) => {
    await page.goto("/usage?window_days=30&exclude_model=hidden-model");
    const panel = page.locator(".attribution-panel");
    await expect(panel.locator(".tile").first()).toBeVisible();
    await panel.getByRole("button", { name: "List", exact: true }).click();
    const row = panel.locator(".list-row").first();
    await expect(row).toBeVisible();
    const brush = page.locator(".chart-container .chart-body").first();
    const bounds = (await brush.boundingBox())!;
    const y = bounds.y + bounds.height / 2;
    await page.mouse.move(bounds.x + bounds.width * 0.2, y);
    await page.mouse.down();
    await page.mouse.move(bounds.x + bounds.width * 0.75, y, { steps: 8 });
    await page.mouse.up();
    await expect(page.locator(".chart-container").getByRole("button", { name: "Clear selection", exact: true })).toBeVisible();
    for (const action of ["All projects", "Escape", "Backspace"]) {
      if (action === "Escape") {
        await row.press("Enter");
        await expect(row).toHaveAttribute("aria-pressed", "false");
        await row.press("Enter");
        await expect(row).toHaveAttribute("aria-pressed", "true");
        await panel.getByRole("button", { name: "Open", exact: true }).press("Enter");
      } else await row.dblclick();
      await expect(panel.getByRole("button", { name: "All projects" })).toBeVisible();
      await expect(panel.locator(".list-row").first()).toBeVisible();
      const url = page.url();
      if (action === "All projects") await panel.getByRole("button", { name: action }).click();
      else await page.keyboard.press(action);
      await expect(panel.getByRole("button", { name: "All projects" })).toBeHidden();
      await expect(row).toHaveAttribute("aria-pressed", "true");
      await expect(page.locator(".chart-container").getByRole("button", { name: "Clear selection", exact: true })).toBeVisible();
      expect(page.url()).toBe(url);
    }
  });
});
