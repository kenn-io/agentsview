import { expect, test, type Page } from "@playwright/test";

function holdSignals(page: Page): Promise<() => void> {
  let release!: () => void;
  const released = new Promise<void>((resolve) => {
    release = resolve;
  });
  return page
    .route("**/api/v1/analytics/signals*", async (route) => {
      await released;
      await route.continue();
    })
    .then(() => release);
}

async function summaryHeight(page: Page): Promise<number> {
  return page.locator(".summary-grid").first().evaluate((el) => el.getBoundingClientRect().height);
}

test("Quality summary cards stay in their columns at 200% interface zoom", async ({ page }) => {
  // CSS zoom does not change media queries, so the page keeps four
  // columns at a viewport above 900px even though it lays out at 480px.
  await page.addInitScript(() => localStorage.setItem("agentsview-zoom-level", "200"));
  await page.setViewportSize({ width: 960, height: 900 });
  await page.goto("/quality");
  const cards = page.locator(".summary-grid .stat-card");
  await expect(cards).toHaveCount(4);

  const boxes = await cards.evaluateAll((els) =>
    els.map((el) => {
      const r = el.getBoundingClientRect();
      return { left: r.left, right: r.right, top: r.top };
    }),
  );
  for (let i = 1; i < boxes.length; i++) {
    if (boxes[i]!.top !== boxes[i - 1]!.top) continue;
    expect(boxes[i]!.left).toBeGreaterThanOrEqual(boxes[i - 1]!.right);
  }
});

for (const width of [1280, 800]) {
  test(`Quality summary keeps its height when loading finishes at ${width}px`, async ({ page }) => {
    await page.setViewportSize({ width, height: 900 });
    const release = await holdSignals(page);
    await page.goto("/quality");
    await expect(page.locator(".summary-grid .skeleton-card")).toHaveCount(4);
    const loading = await summaryHeight(page);

    release();
    await expect(page.locator(".summary-grid .stat-card")).toHaveCount(4);
    expect(Math.abs((await summaryHeight(page)) - loading)).toBeLessThanOrEqual(1);
  });
}
