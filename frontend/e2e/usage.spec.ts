import { test, expect } from "@playwright/test";
import { expectActiveNavTab } from "./helpers/nav";

test.describe("Usage page", () => {
  test.beforeEach(async ({ page }) => {
    await page.goto("/usage");
    // Wait for the page shell to render.
    await expect(
      page.locator(".usage-page"),
    ).toBeVisible({ timeout: 10_000 });
  });

  test("shows toolbar and summary cards with data", async ({
    page,
  }) => {
    await expect(
      page.locator(".usage-toolbar").first(),
    ).toBeVisible();

    // Summary cards should appear with at least one value.
    await expect(
      page.locator(".summary-cards"),
    ).toBeVisible();
    await expect(
      page.locator(".card-value").first(),
    ).toBeVisible({ timeout: 10_000 });
  });

  test("shows cost time series chart", async ({ page }) => {
    // Wait for summary data to load.
    await expect(
      page.locator(".summary-cards .card-value").first(),
    ).toBeVisible({ timeout: 10_000 });
    await expect(
      page.locator(".chart-container"),
    ).toBeVisible();
    // SVG chart should render.
    await expect(
      page.locator(".chart-container svg"),
    ).toBeVisible();
  });

  test("shows attribution panel with treemap", async ({
    page,
  }) => {
    await expect(
      page.locator(".summary-cards .card-value").first(),
    ).toBeVisible({ timeout: 10_000 });
    await expect(
      page.locator(".attribution-panel"),
    ).toBeVisible();
    // Treemap SVG should be rendered.
    await expect(
      page.locator(".treemap-container svg"),
    ).toBeVisible();
  });

  test("filter dropdown opens and shows items", async ({
    page,
  }) => {
    // Wait for data so filter items are populated.
    await expect(
      page.locator(".summary-cards .card-value").first(),
    ).toBeVisible({ timeout: 10_000 });

    // Click the first filter dropdown (Project).
    const trigger = page
      .locator(".usage-toolbar .kit-filter-dropdown__btn")
      .first();
    await trigger.click();

    // Dropdown panel should appear with rows.
    await expect(
      page.locator(".usage-toolbar .kit-filter-dropdown__panel").first(),
    ).toBeVisible();
    await expect(
      page.locator(".usage-toolbar .kit-filter-dropdown__item").first(),
    ).toBeVisible();
  });

  test("excluding a project updates total cost", async ({
    page,
  }) => {
    // Wait for data to load.
    await expect(
      page.locator(".summary-cards .card-value").first(),
    ).toBeVisible({ timeout: 10_000 });

    // Grab the initial total cost text.
    const totalCostBefore = await page
      .locator(".card.featured .card-value")
      .textContent();

    // Open the project filter and exclude the first item.
    const trigger = page
      .locator(".usage-toolbar .kit-filter-dropdown__btn")
      .first();
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
      const after = await page
        .locator(".card.featured .card-value")
        .textContent();
      expect(after).not.toBe(totalCostBefore);
    }).toPass({ timeout: 5_000 });
  });

  test("select all / deselect all buttons work", async ({
    page,
  }) => {
    // Wait for data so items populate.
    await expect(
      page.locator(".summary-cards .card-value").first(),
    ).toBeVisible({ timeout: 10_000 });

    // Open the project filter.
    const trigger = page
      .locator(".usage-toolbar .kit-filter-dropdown__btn")
      .first();
    await trigger.click();

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

  test("top nav shows Usage as the active destination", async ({
    page,
  }) => {
    await expectActiveNavTab(page, "Usage");
  });

  test("URL updates when filter changes", async ({ page }) => {
    // Wait for data.
    await expect(
      page.locator(".summary-cards .card-value").first(),
    ).toBeVisible({ timeout: 10_000 });

    // Exclude a project.
    const trigger = page
      .locator(".usage-toolbar .kit-filter-dropdown__btn")
      .first();
    await trigger.click();
    await page
      .locator(".usage-toolbar .kit-filter-dropdown__item")
      .filter({ hasText: "project-delta" })
      .first()
      .click();
    await page.mouse.click(10, 10);

    // URL should contain the exclude_project param.
    await expect(page).toHaveURL(/exclude_project=/);
  });
});
