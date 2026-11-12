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
  const status = page.getByRole("status").filter({ hasText: "Searching" });
  await expect(loading).toBeVisible();
  await expect(status).toHaveCount(1);
  // The announcer sits outside the busy results, so it is not muted.
  expect(await status.evaluate((el) => el.closest("[aria-busy=true]"))).toBeNull();
  await expect(page.locator(".palette-results")).toHaveAttribute("aria-busy", "true");
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
  await expect(status).toHaveCount(0);
  await expect(page.locator(".palette-results")).toHaveAttribute("aria-busy", "false");
});

test("keeps keyboard focus inside the open palette and returns it on close", async ({ page }) => {
  await page.goto("/usage");
  const opener = page.locator(".kit-top-bar__tab, .kit-top-bar__nav-select button").first();
  await opener.focus();
  await openSearch(page, "synchronous DB read");
  await expect(page.locator(".palette-item").first()).toBeVisible();

  const insidePalette = () =>
    page.evaluate(() => document.querySelector(".palette")?.contains(document.activeElement));
  await page.keyboard.press("Shift+Tab");
  expect(await insidePalette()).toBe(true);
  for (let i = 0; i < 12; i += 1) {
    await page.keyboard.press("Tab");
    expect(await insidePalette()).toBe(true);
  }

  await page.keyboard.press("Escape");
  await expect(page.locator(".palette")).toHaveCount(0);
  await expect(opener).toBeFocused();
});

test("keeps a real result selected when a new search returns fewer rows", async ({ page }) => {
  let release: () => void = () => {};
  const held = new Promise<void>((resolve) => (release = resolve));
  let first: Record<string, unknown> | undefined;
  await page.route((url) => url.pathname === "/api/v1/search", async (route) => {
    const query = new URL(route.request().url()).searchParams.get("q");
    const response = await route.fetch();
    const body = await response.json();
    if (query === "test") {
      first = body.results?.[0];
      expect(first, "the fixture has a result for the query").toBeTruthy();
      body.results = [0, 1, 2].map((i) => ({ ...first, session_id: `${first!.session_id}-${i}` }));
    } else {
      await held;
      body.results = [first];
    }
    await route.fulfill({ response, json: body });
  });
  await page.goto("/usage");
  await openSearch(page, "test");
  await expect(page.locator(".palette-item")).toHaveCount(3);

  await page.locator(".palette-input").press("s");
  await expect(page.locator(".palette-loading")).toBeVisible();
  // Arrows while the new search runs must not select the old, hidden rows.
  await page.keyboard.press("ArrowDown");
  await page.keyboard.press("ArrowDown");
  release();
  await expect(page.locator(".palette-item")).toHaveCount(1);

  const input = page.locator(".palette-input");
  const active = await input.getAttribute("aria-activedescendant");
  expect(active).toBeTruthy();
  await expect(page.locator(`[id="${active}"]`)).toHaveClass(/palette-item/);
  await page.keyboard.press("Enter");
  await expect(page).toHaveURL(/\/sessions\//);
});
