import { test, expect } from "@playwright/test";
import { SessionsPage } from "./pages/sessions-page";
import { clickNavTab } from "./helpers/nav";

// Test-fixture assumptions: project-alpha has 2 sessions,
// project-beta has 3, project-duration has 1 (the duration UX
// showcase), project-edits has 1 (the recent-edits fixture),
// totalling 10 sessions across all projects.
const TOTAL_SESSIONS = 10;
const ALPHA_SESSIONS = 2;
const BETA_SESSIONS = 3;

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
      await expect(sp.sessionListHeader).toContainText(
        `${expectedCount} sessions`,
      );
      await expect(sp.sessionItems).toHaveCount(expectedCount);
    });
  }

  test("URL updates when filter changes on bare /sessions", async ({
    page,
  }) => {
    await sp.filterByProject("project-alpha");
    await expect(page).toHaveURL(/[?&]project=project-alpha/);
  });

  test("URL re-syncs filter from localStorage on tab switch back", async ({
    page,
  }) => {
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

  test("restored rolling dates reach the first Sessions request", async ({
    page,
  }) => {
    await page.locator(".kit-date-range-picker__trigger").click();
    await page.getByRole("button", { name: "90d", exact: true }).click();
    await expect(page).toHaveURL(/window_days=90/);
    const selectedUrl = new URL(page.url());
    const expectedFrom = selectedUrl.searchParams.get("date_from");
    const expectedTo = selectedUrl.searchParams.get("date_to");
    expect(expectedFrom).toMatch(/^\d{4}-\d{2}-\d{2}$/);
    expect(expectedTo).toMatch(/^\d{4}-\d{2}-\d{2}$/);

    await clickNavTab(page, "Insights");
    await expect(page).toHaveURL(/\/insights/);

    const requestPromise = page.waitForRequest((request) =>
      new URL(request.url()).pathname.endsWith(
        "/api/v1/sessions/sidebar-index",
      )
    );
    await clickNavTab(page, "Sessions");
    const requestUrl = new URL((await requestPromise).url());

    expect(requestUrl.searchParams.get("date_from")).toBe(expectedFrom);
    expect(requestUrl.searchParams.get("date_to")).toBe(expectedTo);
  });

  test("linked dates reach the first request on direct detail entry", async ({
    page,
  }) => {
    const sessionId = await sp.sessionItems.first().getAttribute(
      "data-session-id",
    );
    expect(sessionId).toBeTruthy();

    await page.locator(".kit-date-range-picker__trigger").click();
    await page.getByRole("button", { name: "90d", exact: true }).click();
    await expect(page).toHaveURL(/window_days=90/);
    const selectedUrl = new URL(page.url());
    const expectedFrom = selectedUrl.searchParams.get("date_from");
    const expectedTo = selectedUrl.searchParams.get("date_to");

    await page.getByRole("button", { name: "Settings" }).click();
    await page
      .getByRole("checkbox", { name: "Link date ranges across pages" })
      .check();

    const requestPromise = page.waitForRequest((request) =>
      new URL(request.url()).pathname.endsWith(
        "/api/v1/sessions/sidebar-index",
      )
    );
    await page.goto(`/sessions/${encodeURIComponent(sessionId!)}`);
    const requestUrl = new URL((await requestPromise).url());

    expect(requestUrl.searchParams.get("date_from")).toBe(expectedFrom);
    expect(requestUrl.searchParams.get("date_to")).toBe(expectedTo);
    await expect(page).toHaveURL(/date_from=/);
    await expect(page).toHaveURL(/date_to=/);
  });

  test("rolling detail routes refresh bounds and preserve message targets", async ({
    page,
  }) => {
    const sessionId = await sp.sessionItems.first().getAttribute(
      "data-session-id",
    );
    expect(sessionId).toBeTruthy();
    await page.clock.setFixedTime(new Date("2026-07-10T12:00:00"));

    const requestPromise = page.waitForRequest((request) =>
      new URL(request.url()).pathname.endsWith(
        "/api/v1/sessions/sidebar-index",
      )
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
    const sessionId = await sp.sessionItems.first().getAttribute(
      "data-session-id",
    );
    expect(sessionId).toBeTruthy();

    await page.locator(".kit-date-range-picker__trigger").click();
    await page.getByRole("button", { name: "90d", exact: true }).click();
    await page.getByRole("button", { name: "Settings" }).click();
    await page
      .getByRole("checkbox", { name: "Link date ranges across pages" })
      .check();

    await page.goto(
      `/sessions/${encodeURIComponent(sessionId!)}?date_from=2026-05-01&date_to=2026-05-07`,
    );
    const requestPromise = page.waitForRequest((request) =>
      new URL(request.url()).pathname.endsWith("/api/v1/usage/summary")
    );
    await clickNavTab(page, "Usage");
    const requestUrl = new URL((await requestPromise).url());

    expect(requestUrl.searchParams.get("from")).toBe("2026-05-01");
    expect(requestUrl.searchParams.get("to")).toBe("2026-05-07");
    await expect(
      page.locator(".kit-date-range-picker__trigger"),
    ).toContainText("2026-05-01");
    await expect(
      page.locator(".kit-date-range-picker__trigger"),
    ).toContainText("2026-05-07");
  });
});
