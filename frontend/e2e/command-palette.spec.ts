import { expect, test, type Page } from "@playwright/test";

async function openSearch(page: Page, query: string) {
  await page.keyboard.press("ControlOrMeta+k");
  const input = page.locator(".palette-input");
  await expect(input).toBeVisible();
  await input.fill(query);
}

for (const theme of ["light", "dark"] as const) {
  test(`search filters share one enabled field style in ${theme}`, async ({ page }) => {
    await page.addInitScript((t) => localStorage.setItem("theme", t), theme);
    await page.goto("/usage");
    await openSearch(page, "synchronous DB read");
    await expect(page.locator(".palette-item").first()).toBeVisible();

    const styles = await page.evaluate(() =>
      [
        ".palette-controls .kit-typeahead__trigger",
        ".palette-controls .kit-date-range-picker__trigger",
      ].map((selector) => {
        const el = document.querySelector<HTMLElement>(selector)!;
        const cs = getComputedStyle(el);
        return {
          background: cs.backgroundColor,
          border: cs.borderTopColor,
          color: cs.color,
          fontSize: cs.fontSize,
          radius: cs.borderTopLeftRadius,
          height: Math.round(el.getBoundingClientRect().height),
        };
      }),
    );
    const [project, dates] = styles;
    expect(project).toEqual(dates);
  });
}

test("search shows placeholder rows at a result-list size while it runs", async ({ page }) => {
  let release: () => void = () => {};
  const held = new Promise<void>((resolve) => (release = resolve));
  await page.route("**/api/v1/search**", async (route) => {
    await held;
    await route.continue();
  });
  await page.goto("/usage");
  await openSearch(page, "synchronous DB read");

  const loading = page.locator(".palette-loading");
  await expect(loading).toBeVisible();
  await expect(loading).toHaveAttribute("role", "status");
  await expect(loading.locator(".skeleton-row").first()).toBeVisible();
  const sizes = await page.evaluate(() => {
    const palette = document.querySelector(".palette")!.getBoundingClientRect();
    const rows = document.querySelector(".palette-loading")!.getBoundingClientRect();
    return { palette: palette.height, rows: rows.height };
  });
  // The palette hugs the placeholder rows instead of stretching to its
  // maximum height around them.
  expect(sizes.palette - sizes.rows).toBeLessThan(140);

  release();
  await expect(page.locator(".palette-item").first()).toBeVisible();
  await expect(loading).toHaveCount(0);
});
