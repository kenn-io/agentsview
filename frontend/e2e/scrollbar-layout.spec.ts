import { expect, test, type Locator, type Page, type Route } from "@playwright/test";

// Headless Chromium hides scrollbars by default, which would hide the
// scrollbar-width shift these tests guard against.
test.use({
  launchOptions: { ignoreDefaultArgs: ["--hide-scrollbars"] },
  viewport: { width: 1600, height: 600 },
});

function holdRequests(
  page: Page,
  url: string,
  respond: (route: Route) => Promise<void> = (route) => route.continue(),
): Promise<() => void> {
  let release!: () => void;
  const released = new Promise<void>((resolve) => {
    release = resolve;
  });
  return page
    .route(url, async (route) => {
      await released;
      await respond(route);
    })
    .then(() => release);
}

function overflows(scroller: Locator): Promise<boolean> {
  return scroller.evaluate((element) => element.scrollHeight > element.clientHeight);
}

function scrollbarWidth(scroller: Locator): Promise<number> {
  return scroller.evaluate((element) => (element as HTMLElement).offsetWidth - element.clientWidth);
}

async function leftEdge(element: Locator): Promise<number> {
  const box = await element.boundingBox();
  expect(box).not.toBeNull();
  return box!.x;
}

async function rightEdge(element: Locator): Promise<number> {
  const box = await element.boundingBox();
  expect(box).not.toBeNull();
  return box!.x + box!.width;
}

async function expectScrollbarAppeared(scroller: Locator): Promise<void> {
  await expect.poll(() => overflows(scroller)).toBe(true);
  expect(await scrollbarWidth(scroller)).toBeGreaterThan(0);
}

const syntheticPins = Array.from({ length: 30 }, (_, index) => ({
  id: index + 1,
  message_id: index + 1,
  ordinal: index,
  session_id: "synthetic-session",
  session_project: "synthetic-project",
  session_agent: "claude",
  role: "user",
  content: `Synthetic pinned message ${index + 1}`,
  created_at: "2026-01-01T00:00:00Z",
}));

const syntheticEditedFiles = Array.from({ length: 30 }, (_, index) => ({
  edit_count: 1,
  edits: [],
  edits_truncated: false,
  file_path: `/synthetic/file-${index + 1}.ts`,
  last_session_id: "synthetic-session",
  project: "synthetic-project",
}));

const syntheticTrashedSessions = Array.from({ length: 12 }, (_, index) => ({
  id: `synthetic-trashed-${index + 1}`,
  project: "synthetic-project",
  agent: "claude",
  first_message: `Synthetic trashed session ${index + 1}`,
  user_message_count: 1,
  message_count: 1,
  deleted_at: "2026-01-01T00:00:00Z",
}));

test.describe("Scrollbar layout", () => {
  test.skip(
    ({ browserName }) => browserName !== "chromium",
    "WebKit reserves no scrollbar-gutter space for a ::-webkit-scrollbar width, so the shift remains",
  );

  test("keeps the Pinned column in place when pins make the page scroll", async ({ page }) => {
    const release = await holdRequests(page, "**/api/v1/pins*", (route) =>
      route.fulfill({ json: { pins: syntheticPins } }),
    );
    await page.goto("/pinned");

    const scroller = page.locator(".page-scroll");
    const heading = page.getByRole("heading", { name: "Pinned Messages" });
    await expect(heading).toBeVisible();
    expect(await overflows(scroller)).toBe(false);
    const before = await leftEdge(heading);

    release();
    await expect(page.getByText("Synthetic pinned message 1", { exact: true })).toBeVisible();
    await expectScrollbarAppeared(scroller);

    expect(await leftEdge(heading)).toBe(before);
  });

  test("keeps the Recent Edits column in place when files make the page scroll", async ({
    page,
  }) => {
    const release = await holdRequests(page, "**/api/v1/recent-edits?*", (route) =>
      route.fulfill({ json: { files: syntheticEditedFiles, has_more: false } }),
    );
    await page.goto("/recent-edits");

    const scroller = page.locator(".page-scroll");
    const heading = page.getByRole("heading", { name: "Recent Edits" });
    await expect(heading).toBeVisible();
    expect(await overflows(scroller)).toBe(false);
    const before = await leftEdge(heading);

    release();
    await expect(page.locator(".re-file-row")).toHaveCount(30);
    await expectScrollbarAppeared(scroller);

    expect(await leftEdge(heading)).toBe(before);
  });

  test("keeps the Trends column in place when results make the page scroll", async ({ page }) => {
    const release = await holdRequests(page, "**/api/v1/trends/terms*");
    await page.goto("/trends");

    const scroller = page.locator(".page-scroll");
    const heading = page.getByRole("heading", { name: "Trends" });
    await expect(heading).toBeVisible();
    expect(await overflows(scroller)).toBe(false);
    const before = await leftEdge(heading);

    release();
    await expectScrollbarAppeared(scroller);

    expect(await leftEdge(heading)).toBe(before);
  });

  test("keeps the Trash column in place when restoring removes the scrollbar", async ({ page }) => {
    await page.route("**/api/v1/trash", (route) =>
      route.fulfill({ json: { sessions: syntheticTrashedSessions } }),
    );
    await page.route("**/api/v1/sessions/*/restore", (route) =>
      route.fulfill({ json: { status: "restored" } }),
    );
    await page.goto("/trash");

    const scroller = page.locator(".page-scroll");
    const heading = page.getByRole("heading", { name: "Trash" });
    const restoreButtons = page.locator(".restore-btn");
    await expect(restoreButtons).toHaveCount(12);
    await expectScrollbarAppeared(scroller);
    const before = await leftEdge(heading);

    for (let remaining = 11; await overflows(scroller); remaining--) {
      expect(remaining).toBeGreaterThan(0);
      await restoreButtons.first().click();
      await expect(restoreButtons).toHaveCount(remaining);
    }

    expect(await leftEdge(heading)).toBe(before);
  });

  test("keeps the Settings panel in place when a taller panel makes it scroll", async ({
    page,
  }) => {
    await page.setViewportSize({ width: 1600, height: 900 });
    await page.goto("/settings");

    const nav = page.getByRole("navigation", { name: "Settings" });
    const scroller = page.locator(".kit-settings__scroll");
    const panel = page.locator(".kit-settings__panel");
    await nav.locator("button", { hasText: "Terminal" }).click();
    await expect(page.getByRole("heading", { name: "Terminal" })).toBeVisible();
    expect(await overflows(scroller)).toBe(false);
    const before = await leftEdge(panel);

    await nav.locator("button", { hasText: "Session Providers" }).click();
    await expect(page.getByRole("heading", { name: "Session Providers" })).toBeVisible();
    await expectScrollbarAppeared(scroller);

    expect(await leftEdge(panel)).toBe(before);
  });

  test("keeps Quality's right-aligned controls in place when signals make the page scroll", async ({
    page,
  }) => {
    await page.setViewportSize({ width: 1600, height: 1400 });
    const release = await holdRequests(page, "**/api/v1/analytics/signals*");
    await page.goto("/quality");

    const scroller = page.locator(".page-scroll");
    const refresh = page.locator(".kit-refresh-control");
    await expect(refresh).toBeVisible();
    expect(await overflows(scroller)).toBe(false);
    const before = await rightEdge(refresh);

    release();
    await expectScrollbarAppeared(scroller);

    expect(await rightEdge(refresh)).toBe(before);
  });

  test("keeps Analytics' right-aligned controls in place when results make the pane scroll", async ({
    page,
  }) => {
    await page.setViewportSize({ width: 1600, height: 2400 });
    const release = await holdRequests(page, "**/api/v1/analytics/**");
    await page.goto("/sessions");

    const scroller = page.locator(".analytics-content");
    const toggle = scroller.locator(".metric-toggle").first();
    await expect(toggle).toBeVisible();
    expect(await overflows(scroller)).toBe(false);
    const before = await rightEdge(toggle);

    release();
    await expectScrollbarAppeared(scroller);

    expect(await rightEdge(toggle)).toBe(before);
  });
});
