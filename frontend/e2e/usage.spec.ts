import { test, expect, type Page } from "@playwright/test";
import type { UsageSummaryResponse } from "../src/lib/api/generated/index";
import { clickNavTab, expectActiveNavTab } from "./helpers/nav";

test.describe("Usage page", () => {
  test.beforeEach(async ({ page }) => {
    await page.goto("/usage");
    // Wait for the page shell to render.
    await expect(page.locator(".usage-page")).toBeVisible({ timeout: 10_000 });
  });

  test("shows toolbar and summary cards with data", async ({ page }) => {
    await expect(page.locator(".usage-toolbar").first()).toBeVisible();

    // Summary cards should appear with at least one value.
    await expect(page.locator(".summary-cards")).toBeVisible();
    await expect(page.locator(".card-value").first()).toBeVisible({ timeout: 10_000 });
  });

  test("shows cost time series chart", async ({ page }) => {
    // Wait for summary data to load.
    await expect(page.locator(".summary-cards .card-value").first()).toBeVisible({
      timeout: 10_000,
    });
    await expect(page.locator(".chart-container")).toBeVisible();
    await expect(
      page.locator(".chart-container").getByRole("figure", { name: "Cost Over Time" }),
    ).toBeVisible();
  });

  test("keeps summary cards and the chart fixed through brush selection", async ({ page }) => {
    await expect(page.locator(".summary-cards .card-value").first()).toBeVisible({
      timeout: 10_000,
    });

    const chart = page.locator(".chart-container").first();
    const plot = chart.locator("svg").first();
    const brush = chart.locator(".chart-body");
    const summaryRow = page.locator(".summary-cards");
    const summaryCards = summaryRow.locator(".card");
    await expect(plot).toBeVisible({ timeout: 10_000 });
    await expect(brush).toBeVisible({ timeout: 10_000 });

    const measureLayout = async () => {
      const row = await summaryRow.boundingBox();
      const chartBox = await chart.boundingBox();
      const plotBox = await plot.boundingBox();
      const cardBoxes = await summaryCards.evaluateAll((cards) =>
        cards.map((card) => card.getBoundingClientRect().height),
      );
      expect(row).not.toBeNull();
      expect(chartBox).not.toBeNull();
      expect(plotBox).not.toBeNull();
      if (!row || !chartBox || !plotBox) {
        throw new Error("Usage layout is not measurable");
      }
      expect(new Set(cardBoxes).size).toBe(1);
      return { row, chart: chartBox, plot: plotBox, cardBoxes };
    };

    const beforeLayout = await measureLayout();
    const brushBounds = await brush.boundingBox();
    expect(brushBounds).not.toBeNull();
    if (!brushBounds) return;

    const y = brushBounds.y + brushBounds.height / 2;
    await page.mouse.move(brushBounds.x + brushBounds.width * 0.25, y);
    await page.mouse.down();
    await page.mouse.move(brushBounds.x + brushBounds.width * 0.65, y, { steps: 8 });
    await page.mouse.up();

    await expect(chart.getByRole("button", { name: "Clear selection" })).toBeVisible();
    const selectedLayout = await measureLayout();
    expect(selectedLayout.plot.y - selectedLayout.chart.y).toBe(
      beforeLayout.plot.y - beforeLayout.chart.y,
    );
    expect(selectedLayout.cardBoxes).toEqual(beforeLayout.cardBoxes);
    expect(selectedLayout.row.height).toBe(beforeLayout.row.height);
    expect(selectedLayout.chart.y).toBe(beforeLayout.chart.y);
    expect(selectedLayout.plot.y).toBe(beforeLayout.plot.y);

    await chart.getByRole("button", { name: "Clear selection" }).click();
    await expect(chart.getByRole("button", { name: "Clear selection" })).toBeHidden();
    const clearedLayout = await measureLayout();
    expect(clearedLayout.cardBoxes).toEqual(beforeLayout.cardBoxes);
    expect(clearedLayout.row.height).toBe(beforeLayout.row.height);
    expect(clearedLayout.chart.y).toBe(beforeLayout.chart.y);
    expect(clearedLayout.plot.y).toBe(beforeLayout.plot.y);
  });

  test("shows attribution panel with treemap", async ({ page }) => {
    await expect(page.locator(".summary-cards .card-value").first()).toBeVisible({
      timeout: 10_000,
    });
    await expect(page.locator(".attribution-panel")).toBeVisible();
    await expect(
      page.locator(".treemap-container").getByRole("figure", { name: "Treemap" }),
    ).toBeVisible();
  });

  test("filter dropdown opens and shows items", async ({ page }) => {
    // Wait for data so filter items are populated.
    await expect(page.locator(".summary-cards .card-value").first()).toBeVisible({
      timeout: 10_000,
    });

    // Click the first filter dropdown (Project).
    const trigger = page.locator(".usage-toolbar .kit-filter-dropdown__btn").first();
    await trigger.click();

    // Dropdown panel should appear with rows.
    await expect(page.locator(".usage-toolbar .kit-filter-dropdown__panel").first()).toBeVisible();
    await expect(page.locator(".usage-toolbar .kit-filter-dropdown__item").first()).toBeVisible();
  });

  test("excluding a project updates total cost", async ({ page }) => {
    // Wait for data to load.
    await expect(page.locator(".summary-cards .card-value").first()).toBeVisible({
      timeout: 10_000,
    });

    // Grab the initial total cost text.
    const totalCostBefore = await page.locator(".card.featured .card-value").textContent();

    // Open the project filter and exclude the first item.
    const trigger = page.locator(".usage-toolbar .kit-filter-dropdown__btn").first();
    await trigger.click();
    await page
      .locator(".usage-toolbar .kit-filter-dropdown__item")
      .filter({ hasText: "project-delta" })
      .first()
      .click();

    // Close dropdown by clicking outside the menu.
    await page.mouse.click(10, 10);

    // Total cost should change after refetch.
    await expect(async () => {
      const after = await page.locator(".card.featured .card-value").textContent();
      expect(after).not.toBe(totalCostBefore);
    }).toPass({ timeout: 5_000 });
  });

  test("select all / deselect all buttons work", async ({ page }) => {
    // Wait for data so items populate.
    await expect(page.locator(".summary-cards .card-value").first()).toBeVisible({
      timeout: 10_000,
    });

    // Open the project filter.
    const trigger = page.locator(".usage-toolbar .kit-filter-dropdown__btn").first();
    await trigger.click();
    await expect(page.locator(".usage-toolbar .kit-filter-dropdown__item").first()).toBeVisible();

    // Click "Deselect all".
    await page
      .locator(".usage-toolbar .kit-filter-dropdown__bulk-btn")
      .filter({ hasText: "Deselect all" })
      .first()
      .click();

    // Trigger label should show "None".
    await expect(trigger).toContainText("None");

    // Click "Select all".
    await page
      .locator(".usage-toolbar .kit-filter-dropdown__bulk-btn")
      .filter({ hasText: "Select all" })
      .first()
      .click();

    // Trigger label should show "All".
    await expect(trigger).toContainText("All");
  });

  test("top nav shows Usage as the active destination", async ({ page }) => {
    await expectActiveNavTab(page, "Usage");
  });

  test("switches between cost and token views with canonical URLs", async ({ page }) => {
    const metric = page.getByRole("radiogroup", {
      name: "Usage metric",
    });

    await metric.getByRole("radio", { name: "Tokens" }).click();
    await expect(page).toHaveURL(/\/usage\?.*view=tokens/);
    await expect(metric.getByRole("radio", { name: "Tokens" })).toHaveAttribute(
      "aria-checked",
      "true",
    );

    await metric.getByRole("radio", { name: "Cost" }).click();
    await expect(page).toHaveURL(/\/usage(?:\?.*)?$/);
    expect(new URL(page.url()).searchParams.has("view")).toBe(false);
  });

  test("ranks token panels by an Output-only selection", async ({ page }) => {
    await page.getByRole("radio", { name: "Tokens" }).click();
    const picker = page.locator('.usage-toolbar button[title="Token types"]');
    await expect(picker).toHaveAttribute("aria-label", "Token types: All");
    await picker.click();

    const menu = page.locator(".usage-toolbar .kit-filter-dropdown__panel");
    await menu.locator("button", { hasText: "Input" }).click();
    await menu.locator("button", { hasText: "Cache Writes" }).click();
    const outputRequest = page.waitForRequest((request) => {
      const url = new URL(request.url());
      return (
        url.pathname.endsWith("/api/v1/usage/top-sessions") &&
        url.searchParams.get("token_types") === "output"
      );
    });
    await menu.locator("button", { hasText: "Cached Read" }).click();
    await outputRequest;

    await expect(picker).toHaveAttribute("aria-label", "Token types: Output");
    await expect(page).toHaveURL(
      (url) =>
        url.pathname === "/usage" &&
        url.searchParams.get("view") === "tokens" &&
        url.searchParams.get("token_types") === "output",
    );
    await expect(page.locator(".top-sessions-container .chart-title")).toHaveText(
      "Top Sessions by Output Tokens",
    );
  });

  test("normalizes legacy token links without dropping filters", async ({ page }) => {
    await page.goto("/token-usage?window_days=90&project=project-delta");

    await expect(page).toHaveURL(
      (url) =>
        url.pathname === "/usage" &&
        url.searchParams.get("view") === "tokens" &&
        url.searchParams.get("window_days") === "90" &&
        url.searchParams.get("project") === "project-delta",
    );
    await expect(page.getByRole("radio", { name: "Tokens" })).toHaveAttribute(
      "aria-checked",
      "true",
    );
  });

  test("project filters use stable keys without writing them to the URL", async ({ page }) => {
    // Wait for data.
    await expect(page.locator(".summary-cards .card-value").first()).toBeVisible({
      timeout: 10_000,
    });

    // Exclude a project.
    const trigger = page.locator(".usage-toolbar .kit-filter-dropdown__btn").first();
    await trigger.click();
    const projectOption = page
      .locator(".usage-toolbar .kit-filter-dropdown__item")
      .filter({ hasText: "project-delta" })
      .first();
    await expect(projectOption).toBeVisible();
    const filteredRequest = page.waitForRequest((request) => {
      const url = new URL(request.url());
      return (
        url.pathname.endsWith("/api/v1/usage/summary") &&
        !!url.searchParams.get("exclude_project_key")
      );
    });
    await projectOption.click();
    const requestUrl = new URL((await filteredRequest).url());
    await page.mouse.click(10, 10);

    expect(requestUrl.searchParams.get("exclude_project_key")).toBeTruthy();
    await expect(page).toHaveURL(
      (url) =>
        url.pathname === "/usage" &&
        !url.searchParams.has("exclude_project") &&
        !url.searchParams.has("exclude_project_key"),
    );
  });

  test("returning bare refreshes rolling bounds after midnight", async ({ page }) => {
    await page.clock.setFixedTime(new Date("2026-07-09T23:59:00"));
    await page.goto("/usage?window_days=30");
    await expect(page.locator(".usage-page")).toBeVisible();
    await expect(page.locator(".kit-date-range-picker__trigger")).toContainText("Last 30 days");

    await clickNavTab(page, "Sessions");
    await page.clock.setFixedTime(new Date("2026-07-10T00:01:00"));
    const requestPromise = page.waitForRequest((request) =>
      new URL(request.url()).pathname.endsWith("/api/v1/usage/summary"),
    );
    await clickNavTab(page, "Usage");
    const requestUrl = new URL((await requestPromise).url());

    expect(requestUrl.searchParams.get("from")).toBe("2026-06-11");
    expect(requestUrl.searchParams.get("to")).toBe("2026-07-10");
    await expect(page.locator(".kit-date-range-picker__trigger")).toContainText("Last 30 days");
  });

  test("adopts a retained Quality range after linking is enabled", async ({ page }) => {
    await page.goto("/quality");
    await expect(page.locator(".quality-page")).toBeVisible();

    await page.locator(".kit-date-range-picker__trigger").click();
    await page.getByRole("button", { name: "90d", exact: true }).click();
    await expect(page).toHaveURL(/window_days=90/);

    await page.getByRole("button", { name: "Settings" }).click();
    await page
      .getByRole("navigation", { name: "Settings" })
      .locator("button", { hasText: "Date ranges" })
      .click();
    await page.getByRole("switch", { name: "Link date ranges across pages" }).check();

    await clickNavTab(page, "Usage");

    await expect(page.locator(".usage-page")).toBeVisible();
    await expect(page.locator(".kit-date-range-picker__trigger")).toContainText("Last 90 days");
  });
});

test.describe("Usage input cards", () => {
  const totals = {
    inputTokens: 248_600_000,
    cacheCreationTokens: 1_400_000,
    cacheReadTokens: 7_650_000_000,
    outputTokens: 20_000_000,
  };
  const summary: UsageSummaryResponse = {
    from: "2026-07-01",
    to: "2026-07-01",
    projects: {},
    totals: {
      ...totals,
      totalCost: { microdollars: 1_000_000 },
      cacheSavings: { microdollars: 0 },
    },
    daily: [
      {
        date: "2026-07-01",
        ...totals,
        totalCost: { microdollars: 1_000_000 },
        modelsUsed: ["model"],
        modelBreakdowns: [],
        projectBreakdowns: [],
        agentBreakdowns: [],
        machineBreakdowns: [],
      },
    ],
    projectTotals: [],
    modelTotals: [],
    agentTotals: [],
    sessionCounts: { total: 1, byProject: {}, byAgent: {} },
    cacheStats: {
      cacheReadTokens: totals.cacheReadTokens,
      cacheCreationTokens: totals.cacheCreationTokens,
      uncachedInputTokens: totals.inputTokens,
      outputTokens: totals.outputTokens,
      hitRate: 0.97,
      savingsVsUncached: { microdollars: 0 },
    },
  };

  const openUsage = async (page: Page, width: number) => {
    await page.setViewportSize({ width, height: 900 });
    await page.route(/\/api\/v1\/usage\/summary(\?|$)/, (route) =>
      route.fulfill({ json: summary }),
    );
    await page.route(/\/api\/v1\/usage\/comparison(\?|$)/, (route) =>
      route.fulfill({
        json: {
          deltaPct: 1.25,
          priorFrom: "2026-06-30",
          priorTo: "2026-06-30",
          priorTotalCost: { microdollars: 444_444 },
        },
      }),
    );
    await page.goto("/usage");
    await expect(page.locator(".summary-cards .card-value").first()).toBeVisible({
      timeout: 10_000,
    });
  };

  const card = (page: Page, label: string) =>
    page.locator(".summary-cards .card").filter({
      has: page.locator(".card-label", { hasText: new RegExp(`^${label}$`) }),
    });

  const expectStableCards = async (page: Page, equalHeights = true) => {
    const cards = page.locator(".summary-cards .card");
    await expect(cards).toHaveCount(10);
    await expect(cards.locator(".card-sub").first()).toContainText("+125%");
    const boxes = await cards.evaluateAll((els) =>
      els.map((el) => ({
        label: el.querySelector(".card-label")?.textContent,
        height: el.getBoundingClientRect().height,
        overflowX: el.scrollWidth > el.clientWidth,
        overflowY: el.scrollHeight > el.clientHeight,
      })),
    );
    if (equalHeights) {
      expect(new Set(boxes.map((box) => box.height)).size).toBe(1);
    }
    expect(boxes.filter((box) => box.overflowX || box.overflowY)).toEqual([]);
  };

  for (const width of [1280, 768, 400]) {
    test(`shows total and uncached input at ${width}px`, async ({ page }) => {
      await openUsage(page, width);

      await expect(card(page, "Total Input").locator(".card-value")).toHaveText("7.9B");
      const uncached = card(page, "Uncached Input");
      await expect(uncached.locator(".card-value")).toHaveText("248.6M");
      await expect(uncached.locator(".card-sub")).toHaveText("+7.6B cached");
      await expect(page.locator(".card-label", { hasText: /^Input Tokens$/ })).toHaveCount(0);
      await expectStableCards(page);
    });
  }

  for (const width of [1280, 768, 400]) {
    test(`keeps az labels inside the cards at ${width}px`, async ({ page }) => {
      await page.addInitScript(() => localStorage.setItem("agentsview-locale", "az"));
      await openUsage(page, width);

      await expect(card(page, "Cəmi giriş").locator(".card-value")).toHaveText("7.9B");
      await expect(card(page, "Keşsiz giriş").locator(".card-value")).toHaveText("248.6M");
      await expectStableCards(page, false);
    });
  }
});
