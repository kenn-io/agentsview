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
  return page
    .locator(".summary-grid")
    .first()
    .evaluate((el) => el.getBoundingClientRect().height);
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

for (const width of [320, 375]) {
  test(`Quality driver rows keep signal names and every column readable at ${width}px`, async ({
    page,
  }) => {
    await page.setViewportSize({ width, height: 900 });
    await page.goto("/quality");
    const rows = page.locator(".driver-row");
    await expect(rows.first()).toBeVisible();
    const layout = await rows.evaluateAll((els) =>
      els.map((row) => {
        const list = row.closest(".driver-list")!.getBoundingClientRect();
        const cells = [...row.children].map((cell) => {
          const r = cell.getBoundingClientRect();
          const el = cell as HTMLElement;
          return { left: r.left, right: r.right, clipped: el.scrollWidth > el.clientWidth + 1 };
        });
        const name = row.firstElementChild as HTMLElement;
        return {
          nameWidth: name.getBoundingClientRect().width,
          nameWhole: name.scrollWidth <= name.clientWidth + 1,
          inside: cells.every((c) => c.left >= list.left - 0.5 && c.right <= list.right + 0.5),
          valuesWhole: cells.slice(1).every((c) => !c.clipped),
        };
      }),
    );
    for (const row of layout) {
      expect(row.nameWhole || row.nameWidth >= 120, JSON.stringify(row)).toBe(true);
      expect(row.inside, JSON.stringify(row)).toBe(true);
      expect(row.valuesWhole, JSON.stringify(row)).toBe(true);
    }
    const head = await page
      .locator(".driver-head")
      .first()
      .evaluate((head) => {
        const spans = [...head.children].map((s) => s.getBoundingClientRect());
        return spans.every((a, i) =>
          spans
            .slice(i + 1)
            .every(
              (b) => a.right <= b.left + 0.5 || a.bottom <= b.top + 0.5 || b.bottom <= a.top + 0.5,
            ),
        );
      });
    expect(head, "column headings do not overlap").toBe(true);
  });
}
