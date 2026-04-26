import { test, expect } from "@playwright/test";
import { SessionsPage } from "./pages/sessions-page";

// Test-fixture assumptions: project-alpha has 2 sessions,
// project-beta has 3, project-duration has 1 (the duration UX
// showcase), totalling 9 sessions across all projects.
const TOTAL_SESSIONS = 9;
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
    await page.locator('.nav-btn[aria-label="Usage"]').click();
    await expect(page).toHaveURL(/\/usage/);

    // Return to Sessions. The bare /sessions navigation should
    // re-acquire the filter from localStorage and reflect it
    // back into the URL so it matches what's displayed.
    await page.locator('.nav-btn[aria-label="Sessions"]').click();
    await expect(page).toHaveURL(/[?&]project=project-alpha/);
  });
});
