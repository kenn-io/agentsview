import { test, expect } from "@playwright/test";

test("keeps the concurrency overlay selector inside its chart card", async ({ page }) => {
  await page.setViewportSize({ width: 2048, height: 768 });
  await page.goto("/activity");

  const chartCard = page.locator(".chart-panel").first();
  const overlaySelector = chartCard.locator(".overlay-toggle .kit-typeahead__trigger");
  await expect(overlaySelector).toBeVisible();

  const [cardBox, selectorBox] = await Promise.all([
    chartCard.boundingBox(),
    overlaySelector.boundingBox(),
  ]);
  expect(cardBox).not.toBeNull();
  expect(selectorBox).not.toBeNull();
  expect(selectorBox!.x + selectorBox!.width).toBeLessThanOrEqual(cardBox!.x + cardBox!.width);
});
