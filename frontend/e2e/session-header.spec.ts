import { expect, test, type Page } from "@playwright/test";
import { SessionsPage } from "./pages/sessions-page";

type Box = { x: number; y: number; width: number; height: number };

async function box(page: Page, selector: string): Promise<Box> {
  const found = await page.locator(selector).first().boundingBox();
  if (!found) throw new Error(`${selector} has no box`);
  return found;
}

// Give the open session usage steps so the header shows the usage
// breakdown trigger; the fixture sessions have none.
async function withUsageSteps(page: Page) {
  await page.route("**/api/v1/sessions/*/usage**", async (route) => {
    const response = await route.fetch();
    const body = await response.json();
    const url = new URL(route.request().url());
    const rows = Array.from({ length: 6 }, (_, index) => ({
      ordinal: index + 1,
      source: "message",
      label: `Prompt ${index + 1}`,
      timestamp: "2026-02-20T12:30:00Z",
      model: "claude-opus-4-6",
      input_tokens: 1000,
      output_tokens: 100,
      cache_creation_input_tokens: 0,
      cache_read_input_tokens: 0,
      cost: body.cost,
      has_cost: false,
    }));
    await route.fulfill({
      response,
      json: {
        ...body,
        breakdown_count: rows.length,
        breakdown: url.searchParams.get("breakdown") === "true" ? rows : [],
      },
    });
  });
}

test("the usage breakdown stays inside a narrow transcript pane", async ({ page }) => {
  await withUsageSteps(page);
  await page.setViewportSize({ width: 1000, height: 900 });
  const sp = new SessionsPage(page);
  await sp.goto();
  await sp.selectFirstSession();
  const analysis = page.getByRole("button", { name: "Show session analysis" });
  if (await analysis.isVisible()) await analysis.click();

  const trigger = page.locator(".usage-breakdown-trigger").first();
  await expect(trigger).toBeVisible();
  const pane = await box(page, ".session-breadcrumb");
  const anchor = (await trigger.boundingBox())!;
  // The scenario under test: a menu right-aligned to the trigger would
  // run past the pane's left edge.
  expect(anchor.x + anchor.width - 460).toBeLessThan(pane.x);

  await trigger.click();
  const menu = page.locator(".usage-breakdown-menu");
  await expect(menu.locator(".usage-breakdown-row").first()).toBeVisible();
  const placed = (await menu.boundingBox())!;
  expect(placed.x).toBeGreaterThanOrEqual(pane.x);
  expect(placed.x + placed.width).toBeLessThanOrEqual(pane.x + pane.width);
  expect(placed.y).toBeGreaterThanOrEqual(anchor.y + anchor.height);
});

async function expectMenuInsidePane(page: Page) {
  const menu = page.locator(".usage-breakdown-menu");
  await expect(menu.locator(".usage-breakdown-row").first()).toBeVisible();
  await expect(async () => {
    const pane = await box(page, ".session-breadcrumb");
    const placed = (await menu.boundingBox())!;
    expect(placed.x).toBeGreaterThanOrEqual(pane.x - 0.5);
    expect(placed.x + placed.width).toBeLessThanOrEqual(pane.x + pane.width + 0.5);
  }).toPass();
}

test("the usage breakdown stays inside the pane at interface zoom", async ({ page }) => {
  await page.addInitScript(() => localStorage.setItem("agentsview-zoom-level", "150"));
  await withUsageSteps(page);
  await page.setViewportSize({ width: 1400, height: 900 });
  const sp = new SessionsPage(page);
  await sp.goto();
  await sp.selectFirstSession();
  const analysis = page.getByRole("button", { name: "Show session analysis" });
  if (await analysis.isVisible()) await analysis.click();

  await page.locator(".usage-breakdown-trigger").first().click();
  await expectMenuInsidePane(page);
});

test("the usage breakdown follows the pane when the sidebar opens", async ({ page }) => {
  await withUsageSteps(page);
  await page.setViewportSize({ width: 1000, height: 900 });
  const sp = new SessionsPage(page);
  await sp.goto();
  await sp.selectFirstSession();
  const analysis = page.getByRole("button", { name: "Show session analysis" });
  if (await analysis.isVisible()) await analysis.click();
  const sidebar = page.locator("#session-sidebar");
  await page.keyboard.press("b");
  await expect(sidebar).toBeHidden();

  await page.locator(".usage-breakdown-trigger").first().click();
  await expectMenuInsidePane(page);
  // Opening the sidebar narrows the pane without resizing the window.
  await page.locator(".usage-breakdown-trigger").first().focus();
  await page.keyboard.press("b");
  await expect(sidebar).toBeVisible();
  await expect(page.locator(".usage-breakdown-menu")).toBeVisible();
  await expectMenuInsidePane(page);
});

test("transcript controls fit a very narrow transcript pane", async ({ page }) => {
  // Just above the phone layout, the sidebar and analysis panel at their
  // minimum widths leave the transcript about 260px.
  await page.setViewportSize({ width: 770, height: 900 });
  const sp = new SessionsPage(page);
  await sp.goto();
  await sp.selectFirstSession();
  await page.getByRole("button", { name: "Show session analysis" }).click();
  await page.locator(".view-trigger").click();
  await page.getByRole("menuitemradio", { name: "Prompts and final answers" }).click();

  const pane = await box(page, ".session-breadcrumb");
  expect(pane.width).toBeLessThan(300);
  for (const name of ["Follow latest messages", "Toggle sort order (o)"]) {
    const control = page.locator(".session-breadcrumb").getByRole("button", { name, exact: true });
    const placed = (await control.boundingBox())!;
    expect(placed.x).toBeGreaterThanOrEqual(pane.x);
    expect(placed.x + placed.width).toBeLessThanOrEqual(pane.x + pane.width);
  }
  const view = await box(page, ".session-breadcrumb .view-trigger");
  expect(view.x).toBeGreaterThanOrEqual(pane.x);
  expect(view.x + view.width).toBeLessThanOrEqual(pane.x + pane.width);
});

test("the View trigger shows its full label and keeps its width across layouts and modes", async ({ page }) => {
  const sp = new SessionsPage(page);
  await sp.goto();
  await sp.selectFirstSession();
  const trigger = page.locator(".session-breadcrumb .view-trigger");
  const label = trigger.locator(".view-trigger-text");
  const widths = new Set<number>();
  for (const mode of ["Everything", "Prompts and final answers"]) {
    await trigger.click();
    await page.getByRole("menuitemradio", { name: new RegExp(`^${mode}`) }).click();
    for (const layout of ["Default", "Compact", "Stream", "Skim"]) {
      await trigger.click();
      await page.getByRole("menuitemradio", { name: new RegExp(`^${layout}`) }).click();
      await expect(label).toContainText(layout);
      // The label is drawn in full: nothing clipped behind an ellipsis.
      const clip = await label.evaluate((el) => el.scrollWidth - el.clientWidth);
      expect(clip, `${mode} / ${layout}`).toBeLessThanOrEqual(0);
      widths.add(Math.round((await trigger.boundingBox())!.width));
    }
  }
  // Switching layouts or modes does not move the trigger's neighbors.
  expect([...widths]).toHaveLength(1);
});
