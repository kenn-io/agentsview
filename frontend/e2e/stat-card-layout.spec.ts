import { expect, test } from "@playwright/test";

for (const { path, width, count, labels } of [
  {
    path: "/usage",
    width: 1350,
    count: 11,
    labels: ["Copilot AI Credits", "Uncached Input"],
  },
  { path: "/activity", width: 871, count: 7, labels: ["Interactive peak"] },
  { path: "/analytics", width: 1035, count: 6, labels: ["Concentration"] },
]) {
  test(`${path} summary cards fit at ${width}px`, async ({ page }) => {
    await page.setViewportSize({ width, height: 900 });
    await page.addInitScript(() => localStorage.setItem("agentsview-locale", "en"));
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
      const edges = await element.evaluate((el) => {
        const range = document.createRange();
        range.selectNodeContents(el);
        return {
          text: range.getBoundingClientRect().right,
          label: el.getBoundingClientRect().right,
          card: el.closest(".stat-card")!.getBoundingClientRect().right,
        };
      });
      expect(edges.text, label).toBeLessThanOrEqual(Math.min(edges.label, edges.card));
    }
  });
}
