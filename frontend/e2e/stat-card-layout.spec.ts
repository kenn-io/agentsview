import { expect, test } from "@playwright/test";

for (const { path, width, count, labels, locale } of [
  {
    path: "/usage",
    width: 1350,
    count: 11,
    labels: ["Copilot AI Credits", "Uncached Input"],
    locale: "en",
  },
  { path: "/activity", width: 871, count: 7, labels: ["Interactive peak"], locale: "en" },
  { path: "/analytics", width: 1035, count: 6, labels: ["Concentration"], locale: "en" },
  { path: "/activity", width: 871, count: 7, labels: ["Pic interactif"], locale: "fr" },
]) {
  test(`${path} summary cards fit at ${width}px in ${locale}`, async ({ page }) => {
    await page.setViewportSize({ width, height: 900 });
    await page.addInitScript((value) => localStorage.setItem("agentsview-locale", value), locale);
    if (path === "/usage") {
      await page.route(/\/api\/v1\/usage\/summary(\?|$)/, async (route) => {
        const response = await route.fetch();
        const summary = await response.json();
        summary.totals.copilotAICredits = 42;
        await route.fulfill({ response, json: summary });
      });
    }
    await page.goto(path);
    const cards = page.locator(".summary-cards .stat-card");
    await expect(cards).toHaveCount(count);
    await expect(cards.locator(".card-value").first()).toContainText(/\d/);
    const tops = await cards.evaluateAll((elements) =>
      elements.map((element) => element.getBoundingClientRect().top),
    );
    expect(new Set(tops).size).toBe(1);
    for (const label of labels) {
      const element = cards.locator(".card-label").getByText(label, { exact: true });
      await expect(element).toBeVisible();
      const size = await element.evaluate((el) => ({
        width: el.clientWidth,
        scroll: el.scrollWidth,
      }));
      expect(size.scroll, label).toBeLessThanOrEqual(size.width);
    }
  });
}
