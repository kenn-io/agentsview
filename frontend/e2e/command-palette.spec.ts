import { expect, test, type Page } from "@playwright/test";

async function openSearch(page: Page, query: string) {
  await page.keyboard.press("ControlOrMeta+k");
  const input = page.locator(".palette-input");
  await expect(input).toBeVisible();
  await input.fill(query);
}

function heldRequest() {
  let release: () => void = () => {};
  const held = new Promise<void>((resolve) => (release = resolve));
  return { held, release };
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
  const { held, release } = heldRequest();
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

test("Retry keeps focus and filters through another failure and success", async ({ page }) => {
  await page.goto("/usage");
  await openSearch(page, "synchronous DB read");
  await page.locator(".palette-controls .kit-typeahead__trigger").click();
  await page.locator(".kit-typeahead__option", { hasText: "project-duration" }).click();
  await page.locator(".palette-controls .kit-date-range-picker__trigger").click();
  await page.getByRole("button", { name: "90d", exact: true }).click();
  await page.getByRole("button", { name: "Recency", exact: true }).click();
  await expect(page.locator(".palette-item").first()).toBeVisible();
  const urls: string[] = [];
  const { held, release } = heldRequest();
  await page.route("**/api/v1/search?**", async (route) => {
    urls.push(route.request().url());
    if (urls.length === 3) await held;
    if (urls.length <= 4) {
      await route.fulfill({ status: 503, json: { error: "Search temporarily unavailable" } });
    } else {
      await route.continue();
    }
  });
  await page.locator(".palette-input").fill("synchronous DB");
  const retry = page.getByRole("button", { name: "Retry" });
  const input = page.locator(".palette-input");
  await expect(retry).toBeVisible();
  await retry.focus();
  await page.keyboard.press("Enter");
  await expect(page.locator(".palette-loading")).toBeVisible();
  await expect(input).toBeFocused();
  release();
  await expect(retry).toBeVisible();
  await expect(input).toBeFocused();
  await retry.focus();
  await page.keyboard.press("Enter");
  await expect(page.locator(".palette-item").first()).toBeVisible();
  await expect(input).toBeFocused();
  await expect(input).toHaveValue("synchronous DB");
  expect(urls).toHaveLength(5);
  for (const url of urls) {
    const params = new URL(url).searchParams;
    expect(params.get("q")).toBe("synchronous DB");
    expect(params.get("project")).toBe("project-duration");
    expect(params.get("sort")).toBe("recency");
    expect(params.get("date_from")).toBeTruthy();
    expect(params.get("date_to")).toBeTruthy();
  }
  expect(new Set(urls).size).toBe(1);
});
