import { expect, test, type Page } from "@playwright/test";

function trendsResponse(termCount: number) {
  const buckets = Array.from({ length: 12 }, (_, i) => ({
    date: `2026-${String(i + 1).padStart(2, "0")}-01`,
    message_count: 100,
  }));
  return {
    granularity: "month",
    from: "2026-01-01",
    to: "2026-12-31",
    message_count: 1200,
    buckets,
    series: Array.from({ length: termCount }, (_, t) => ({
      term: `term ${t + 1}`,
      variants: [`term ${t + 1}`],
      total: (t + 1) * 12,
      points: buckets.map((b, i) => ({ date: b.date, count: (t + 1) * ((i % 3) + 1) })),
    })),
  };
}

async function openTrends(page: Page, termCount: number) {
  await page.route("**/api/v1/trends/terms**", (route) =>
    route.fulfill({ json: trendsResponse(termCount) }),
  );
  await page.setViewportSize({ width: 1440, height: 900 });
  await page.goto("/trends");
  await expect(page.locator(".chart-panel svg").first()).toBeVisible();
}

for (const termCount of [3, 12]) {
  test(`the chart keeps its height with ${termCount} terms`, async ({ page }) => {
    await openTrends(page, termCount);
    await expect(page.locator(".term-table-wrap tbody tr")).toHaveCount(termCount);
    const layout = await page.evaluate(() => {
      const box = (selector: string) => document.querySelector(selector)!.getBoundingClientRect();
      const table = document.querySelector<HTMLElement>(".table-panel")!;
      return {
        chartBody: box(".chart-body").height,
        chartPanel: box(".chart-panel").height,
        termsPanel: box(".terms-panel").height,
        tableScrolls: table.scrollHeight > table.clientHeight + 1,
      };
    });
    // The chart is capped, and the terms panel matches the chart panel
    // instead of stretching it.
    expect(layout.chartBody, JSON.stringify(layout)).toBeLessThanOrEqual(420);
    expect(layout.chartBody).toBeGreaterThanOrEqual(320);
    expect(Math.abs(layout.termsPanel - layout.chartPanel)).toBeLessThanOrEqual(1);
    // A long term list scrolls inside its own table.
    expect(layout.tableScrolls).toBe(termCount === 12);
  });
}

test("enlarging the terms field keeps its resize handle and some counts in view", async ({
  page,
}) => {
  await openTrends(page, 12);
  await expect(page.locator(".term-table-wrap tbody tr")).toHaveCount(12);
  const field = page.locator("#trend-terms");
  const box = (await field.boundingBox())!;
  // Drag the resize handle at the field's bottom-right corner far down.
  await page.mouse.move(box.x + box.width - 3, box.y + box.height - 3);
  await page.mouse.down();
  await page.mouse.move(box.x + box.width - 3, box.y + box.height + 800, { steps: 8 });
  await page.mouse.up();

  const layout = await page.evaluate(() => {
    const rect = (selector: string) => document.querySelector(selector)!.getBoundingClientRect();
    return {
      field: rect("#trend-terms"),
      panel: rect(".terms-panel"),
      table: rect(".table-panel"),
    };
  });
  // The drag did make the field taller.
  expect(layout.field.height, JSON.stringify(layout)).toBeGreaterThan(box.height + 20);
  // Its bottom edge, where the handle is, stays inside the panel, and the
  // counts keep at least a few rows.
  expect(layout.field.bottom).toBeLessThanOrEqual(layout.panel.bottom);
  expect(layout.table.height).toBeGreaterThanOrEqual(100);
});
