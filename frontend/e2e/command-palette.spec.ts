import { expect, test, type Page } from "@playwright/test";

const searchResults = [
  { session_id: "first", project: "project", agent: "codex", ordinal: 0, snippet: "first" },
  { session_id: "second", project: "project", agent: "codex", ordinal: 0, snippet: "second" },
];

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

test("Retry sends a new search and shows its results", async ({ page }) => {
  let attempts = 0;
  await page.route("**/api/v1/search?**", async (route) => {
    if (++attempts <= 2) await route.fulfill({ status: 503 });
    else await route.fulfill({ json: { results: [searchResults[0]] } });
  });
  await page.goto("/usage");
  await openSearch(page, "synchronous DB read");
  const retry = page.getByRole("button", { name: "Retry" });
  await expect(retry).toBeVisible();
  await retry.click();
  await expect(page.locator(".palette-item").first()).toBeVisible();
  expect(attempts).toBe(3);
});

test("automatic recovery selects the first result after a populated search", async ({ page }) => {
  let attempts = 0;
  let release: () => void = () => {};
  const held = new Promise<void>((resolve) => (release = resolve));
  await page.route("**/api/v1/search?**", async (route) => {
    if (new URL(route.request().url()).searchParams.get("q") === "initial search") {
      await route.fulfill({ json: { results: searchResults } });
    } else if (++attempts === 1) {
      await held;
      await route.fulfill({ status: 503 });
    } else {
      await route.fulfill({ json: { results: [searchResults[0]] } });
    }
  });
  await page.goto("/usage");
  await openSearch(page, "initial search");
  await expect(page.locator(".palette-item")).toHaveCount(2);
  const input = page.locator(".palette-input");
  await input.evaluate((el: HTMLInputElement) => {
    el.value = "recovering search";
    el.dispatchEvent(new Event("input", { bubbles: true }));
    el.dispatchEvent(new KeyboardEvent("keydown", { key: "Enter", bubbles: true }));
  });
  await expect(page.locator(".palette-loading")).toBeVisible();
  await input.press("Enter");
  await expect(input).toBeVisible();
  await input.press("ArrowDown");
  release();
  await expect(page.locator(".palette-item")).toHaveCount(1);
  await expect(page.locator(".palette-item").first()).toHaveClass(/selected/);
  await input.press("Enter");
  await expect(page).toHaveURL(/\/sessions\/first/);
  expect(attempts).toBe(2);
});

test("client errors stay visible after another manual attempt", async ({ page }) => {
  let attempts = 0;
  await page.route("**/api/v1/search?**", async (route) => {
    attempts++;
    await route.fulfill({ status: 400, body: "invalid search" });
  });
  await page.goto("/usage");
  await openSearch(page, "invalid search");
  const retry = page.getByRole("button", { name: "Retry" });
  await expect(retry).toBeVisible();
  expect(attempts).toBe(1);
  await retry.click();
  await expect(retry).toBeVisible();
  expect(attempts).toBe(2);
});

test("clearing a failed search prevents its delayed retry from reaching the server", async ({
  page,
}) => {
  let attempts = 0;
  await page.route("**/api/v1/search?**", async (route) => {
    attempts++;
    await route.fulfill({ status: 503 });
  });
  await page.goto("/usage");
  await openSearch(page, "cancel this search");
  await page.waitForResponse(
    (response) => response.url().includes("/api/v1/search?") && response.status() === 503,
  );
  const input = page.locator(".palette-input");
  await input.fill("");
  await page.waitForTimeout(500);
  expect(attempts).toBe(1);
  await expect(input).toHaveValue("");
  await expect(page.locator(".palette-error")).toHaveCount(0);
});
