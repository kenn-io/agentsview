import { createRequire } from "node:module";
import { test, expect } from "@playwright/test";
import { SessionsPage } from "./pages/sessions-page";
import { clickNavTab } from "./helpers/nav";
import { createMockSessions, handleSessionsRoute, sessionsRoutePattern } from "./helpers/mock-sessions";

type RenderLintModule = {
  renderLintSnippet: (scopeSelector: string, options?: Record<string, unknown>) => string;
};

const require = createRequire(import.meta.url);
const renderLintPath = process.env.PR_RENDER_LINT_PATH;
const renderLint = renderLintPath
  ? (require(renderLintPath) as RenderLintModule)
  : undefined;

// Test-fixture assumptions: project-alpha has 2 sessions,
// project-beta has 3, project-duration has 1 (the duration UX
// showcase), project-edits has 1 (the recent-edits fixture), and the
// project-reclassification fixture has 2, totalling 12 sessions.
const TOTAL_SESSIONS = 12;
const ALPHA_SESSIONS = 2;
const BETA_SESSIONS = 3;
const SLOW_SESSION_RESPONSE_MS = 5_500;

test("CI session startup tolerates a slow initial response", async ({ browserName, page }) => {
  test.skip(process.env.CI !== "true", "exercises the CI timeout policy");
  test.skip(browserName !== "webkit", "covers the observed WebKit failure");

  let delayedRequests = 0;
  await page.route(/\/api\/v1\/sessions\/sidebar-index(?:\?|$)/, async (route) => {
    delayedRequests += 1;
    await new Promise((resolve) => setTimeout(resolve, SLOW_SESSION_RESPONSE_MS));
    await route.continue();
  });

  const sp = new SessionsPage(page);
  await sp.goto();

  expect(delayedRequests).toBeGreaterThan(0);
  await expect(sp.sessionItems.first()).toBeVisible();
});

test("session previews hide a leading system-reminder envelope", async ({ page }) => {
  const session = {
    ...createMockSessions(1, "preview", () => "project-preview")[0]!,
    first_message:
      '<system-reminder data-role="user-context">ctx</system-reminder>\nrefactor the auth guard',
  };
  await page.route(sessionsRoutePattern, handleSessionsRoute([{ sessions: [session], project: null }]));
  await page.route("**/api/v1/projects*", (route) =>
    route.fulfill({ json: { projects: [{ name: "project-preview", session_count: 1 }] } }),
  );

  const sp = new SessionsPage(page);
  for (const width of [1280, 768, 400]) {
    await page.setViewportSize({ width, height: 900 });
    await page.goto("/sessions");
    if (width === 400) {
      const drawer = page.locator("#session-sidebar");
      if (!(await drawer.evaluate((sidebar) => sidebar.classList.contains("open")))) {
        await page.locator("button.hamburger").click();
      }
      await expect(drawer).toHaveClass(/open/);
    }
    await expect(sp.sessionItems.first()).toBeVisible();
    const name = sp.sessionItems.first().locator(".session-name");
    await expect(name).toHaveText("refactor the auth guard");
    await expect(name).toHaveAttribute("title", "refactor the auth guard");
    if (renderLint) {
      const violations = await page.evaluate(
        (snippet) => (0, eval)(snippet),
        renderLint.renderLintSnippet("#session-sidebar"),
      );
      console.log(`render-lint width=${width}px violations=${JSON.stringify(violations)}`);
      expect(violations).toEqual([]);
    }
  }
});

test.describe("Session list", () => {
  let sp: SessionsPage;

  test.beforeEach(async ({ page }) => {
    sp = new SessionsPage(page);
    await sp.goto();
  });

  test("sessions load and display", async () => {
    await expect(sp.sessionItems).toHaveCount(TOTAL_SESSIONS);
  });

  test("session count header is visible", async () => {
    await expect(sp.sessionListHeader).toBeVisible();
    await expect(sp.sessionListHeader).toContainText("sessions");
  });

  test("clicking a session marks it active", async () => {
    await sp.sessionItems.first().click();
    await expect(sp.sessionItems.first()).toHaveClass(/active/);
  });

  test("plain arrows follow the last session or message interaction", async ({ page }) => {
    const sessionWithMessages = sp.sessionItems
      .filter({
        has: page.locator(".session-count", { hasText: /[1-9]/ }),
      })
      .first();
    const sessionId = await sessionWithMessages.getAttribute("data-session-id");
    expect(sessionId).toBeTruthy();
    await page.goto(`/sessions/${encodeURIComponent(sessionId!)}`);
    const hasMessages = await expect(sp.messageRows.first())
      .toBeVisible({ timeout: 5_000 })
      .then(() => true)
      .catch(() => false);
    expect(hasMessages || process.env.AGENTSVIEW_E2E_BACKEND === "duckdb").toBe(true);
    await expect(sessionWithMessages).toHaveClass(/active/);

    await sessionWithMessages.focus();
    const activeSessionBefore = await sessionWithMessages.getAttribute("data-session-id");
    await page.keyboard.press("ArrowDown");
    await expect(page.locator('.session-item[aria-current="page"]')).not.toHaveAttribute(
      "data-session-id",
      activeSessionBefore!,
    );

    if (!hasMessages) return;

    await sp.messageRows.first().click();
    await page.keyboard.press("ArrowDown");
    await expect(sp.messageRows.nth(1)).toHaveClass(/selected/);
  });

  const filterCases = [
    { project: "project-alpha", expectedCount: ALPHA_SESSIONS },
    { project: "project-beta", expectedCount: BETA_SESSIONS },
    { project: "", expectedCount: TOTAL_SESSIONS },
  ];

  for (const { project, expectedCount } of filterCases) {
    const label = project || "all";

    test(`filtering by ${label} shows ${expectedCount} sessions`, async () => {
      if (project) {
        await sp.filterByProject(project);
      } else {
        await sp.clearProjectFilter();
      }
      await expect(sp.sessionItems.first()).toBeVisible();
      await expect(sp.sessionListHeader).toContainText(`${expectedCount} sessions`);
      await expect(sp.sessionItems).toHaveCount(expectedCount);
    });
  }

  test("URL updates when filter changes on bare /sessions", async ({ page }) => {
    await sp.filterByProject("project-alpha");
    await expect(page).toHaveURL(/[?&]project=project-alpha/);
  });

  test("URL re-syncs filter from localStorage on tab switch back", async ({ page }) => {
    // Apply a filter so the URL and localStorage record it.
    await sp.filterByProject("project-alpha");
    await expect(page).toHaveURL(/[?&]project=project-alpha/);

    // Switch to Usage; the sessions URL leaves view.
    await clickNavTab(page, "Usage");
    await expect(page).toHaveURL(/\/usage/);

    // Return to Sessions. The bare /sessions navigation should
    // re-acquire the filter from localStorage and reflect it
    // back into the URL so it matches what's displayed.
    await clickNavTab(page, "Sessions");
    await expect(page).toHaveURL(/[?&]project=project-alpha/);
  });

  test("restored rolling dates reach the first Sessions request", async ({ page }) => {
    await page.locator(".kit-date-range-picker__trigger").click();
    await page.getByRole("button", { name: "90d", exact: true }).click();
    await expect(page).toHaveURL(/window_days=90/);
    const selectedUrl = new URL(page.url());
    const expectedFrom = selectedUrl.searchParams.get("date_from");
    const expectedTo = selectedUrl.searchParams.get("date_to");
    expect(expectedFrom).toMatch(/^\d{4}-\d{2}-\d{2}$/);
    expect(expectedTo).toMatch(/^\d{4}-\d{2}-\d{2}$/);

    await clickNavTab(page, "Quality");
    await expect(page).toHaveURL(/\/quality/);

    const requestPromise = page.waitForRequest((request) =>
      new URL(request.url()).pathname.endsWith("/api/v1/sessions/sidebar-index"),
    );
    await clickNavTab(page, "Sessions");
    const requestUrl = new URL((await requestPromise).url());

    expect(requestUrl.searchParams.get("date_from")).toBe(expectedFrom);
    expect(requestUrl.searchParams.get("date_to")).toBe(expectedTo);
  });

  test("linked dates reach the first request on direct detail entry", async ({ page }) => {
    const sessionId = await sp.sessionItems.first().getAttribute("data-session-id");
    expect(sessionId).toBeTruthy();

    await page.locator(".kit-date-range-picker__trigger").click();
    await page.getByRole("button", { name: "90d", exact: true }).click();
    await expect(page).toHaveURL(/window_days=90/);
    const selectedUrl = new URL(page.url());
    const expectedFrom = selectedUrl.searchParams.get("date_from");
    const expectedTo = selectedUrl.searchParams.get("date_to");

    await page.getByRole("button", { name: "Settings" }).click();
    await page
      .getByRole("navigation", { name: "Settings" })
      .locator("button", { hasText: "Date ranges" })
      .click();
    await page.getByRole("switch", { name: "Link date ranges across pages" }).check();

    const requestPromise = page.waitForRequest((request) =>
      new URL(request.url()).pathname.endsWith("/api/v1/sessions/sidebar-index"),
    );
    await page.goto(`/sessions/${encodeURIComponent(sessionId!)}`);
    const requestUrl = new URL((await requestPromise).url());

    expect(requestUrl.searchParams.get("date_from")).toBe(expectedFrom);
    expect(requestUrl.searchParams.get("date_to")).toBe(expectedTo);
    await expect(page).toHaveURL(/date_from=/);
    await expect(page).toHaveURL(/date_to=/);
  });

  test("rolling detail routes refresh bounds and preserve message targets", async ({ page }) => {
    const sessionId = await sp.sessionItems.first().getAttribute("data-session-id");
    expect(sessionId).toBeTruthy();
    await page.clock.setFixedTime(new Date("2026-07-10T12:00:00"));

    const requestPromise = page.waitForRequest((request) =>
      new URL(request.url()).pathname.endsWith("/api/v1/sessions/sidebar-index"),
    );
    await page.goto(
      `/sessions/${encodeURIComponent(sessionId!)}?msg=last&window_days=30&date_from=2026-01-01&date_to=2026-01-30`,
    );
    const requestUrl = new URL((await requestPromise).url());

    expect(requestUrl.searchParams.get("date_from")).toBe("2026-06-11");
    expect(requestUrl.searchParams.get("date_to")).toBe("2026-07-10");
    const routeUrl = new URL(page.url());
    expect(routeUrl.searchParams.get("msg")).toBe("last");
    expect(routeUrl.searchParams.get("window_days")).toBe("30");
    expect(routeUrl.searchParams.get("date_from")).toBe("2026-06-11");
    expect(routeUrl.searchParams.get("date_to")).toBe("2026-07-10");
  });

  test("explicit detail dates replace the shared range", async ({ page }) => {
    const sessionId = await sp.sessionItems.first().getAttribute("data-session-id");
    expect(sessionId).toBeTruthy();

    await page.locator(".kit-date-range-picker__trigger").click();
    await page.getByRole("button", { name: "90d", exact: true }).click();
    await page.getByRole("button", { name: "Settings" }).click();
    await page
      .getByRole("navigation", { name: "Settings" })
      .locator("button", { hasText: "Date ranges" })
      .click();
    await page.getByRole("switch", { name: "Link date ranges across pages" }).check();

    await page.goto(
      `/sessions/${encodeURIComponent(sessionId!)}?date_from=2026-05-01&date_to=2026-05-07`,
    );
    const requestPromise = page.waitForRequest((request) =>
      new URL(request.url()).pathname.endsWith("/api/v1/usage/summary"),
    );
    await clickNavTab(page, "Usage");
    const requestUrl = new URL((await requestPromise).url());

    expect(requestUrl.searchParams.get("from")).toBe("2026-05-01");
    expect(requestUrl.searchParams.get("to")).toBe("2026-05-07");
    await expect(page.locator(".kit-date-range-picker__trigger")).toContainText("2026-05-01");
    await expect(page.locator(".kit-date-range-picker__trigger")).toContainText("2026-05-07");
  });
});
