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

test("switches message metrics by keyboard and fits narrow chart cards", async ({ page }) => {
  // This seeded day includes 750 user messages from the two large sessions.
  const response = await page.request.get("/api/v1/sessions/test-session-xlarge-5500");
  expect(response.ok()).toBe(true);
  const session = await response.json();
  const date = new Date(session.started_at).toISOString().slice(0, 10);
  await page.goto(`/activity?preset=day&date=${date}&timezone=UTC`);
  const chart = page.locator(".timeline");
  const user = chart.getByRole("radio", { name: "User messages", exact: true });
  await user.click();
  await expect(user).toBeChecked();
  await expect(chart.locator(".chart-peak")).toHaveText("750 total");
  await expect(chart.locator(".concurrency-seg.user_messages").first()).toBeVisible();
  await user.press("ArrowRight");
  await expect(chart.getByRole("radio", { name: "Assistant messages", exact: true })).toBeChecked();
  await expect(chart.locator(".concurrency-seg.assistant_messages").first()).toBeVisible();

  for (const locale of ["en", "fr", "ja"]) {
    await page.evaluate((value) => localStorage.setItem("agentsview-locale", value), locale);
    await page.reload();
    await page.setViewportSize({ width: 320, height: 844 });
    const control = chart.getByRole("radiogroup");
    await expect(control).toBeVisible();
    const cardBox = await chart.boundingBox();
    const controlBox = await control.boundingBox();
    expect(controlBox!.x + controlBox!.width).toBeLessThanOrEqual(cardBox!.x + cardBox!.width);
  }
});
