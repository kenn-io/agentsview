import { test, expect } from "@playwright/test";
import { SessionsPage } from "./pages/sessions-page";

// The test fixture seeds 12 root sessions with messages (including the
// duration, recent-edits, and project-reclassification fixtures), plus 3 subagent
// sessions, 1 fork, and 1 empty session. Navigation surfaces (session
// list, status bar) show only the 12 root sessions. The analytics
// summary additionally counts the 3 subagents, whose messages and
// tokens are real spend, so it shows 13. Forks and empty sessions are
// excluded everywhere.
const EXPECTED_ROOT_SESSIONS = 12;
const EXPECTED_ANALYTICS_SESSIONS = 15; // 12 root + 3 subagents

test.describe("Session count consistency", () => {
  let sp: SessionsPage;

  test.beforeEach(async ({ page }) => {
    sp = new SessionsPage(page);
    await sp.goto();
  });

  test("navigation counts agree and analytics counts subagents on top", async ({
    page,
  }) => {
    // 1. Session list header count
    const headerText = await sp.sessionListHeader.textContent();
    const listMatch = headerText?.match(/(\d[\d,]*)\s+sessions/);
    expect(listMatch, "session list header must show a count").toBeTruthy();
    const listCount = parseInt(listMatch![1].replace(/,/g, ""), 10);

    // 2. Status bar (bottom left) — uses /api/v1/stats
    const statusBar = page.locator(".kit-status-bar__section--left");
    await expect(statusBar).toContainText("sessions", { timeout: 5_000 });
    const statusText = await statusBar.textContent();
    const statsMatch = statusText?.match(/(\d[\d,]*)\s+sessions/);
    expect(statsMatch, "status bar must show a session count").toBeTruthy();
    const statsCount = parseInt(statsMatch![1].replace(/,/g, ""), 10);

    // 3. Analytics summary card — uses /api/v1/analytics/summary
    //    The analytics page is visible by default (no session selected).
    const summaryCards = page.locator(".summary-cards");
    await expect(summaryCards).toBeVisible({ timeout: 5_000 });

    // Wait for the summary to finish loading.
    const sessionsCard = summaryCards
      .locator(".card")
      .filter({ has: page.locator(".card-label", { hasText: /^Sessions$/ }) });
    await expect(
      sessionsCard.locator(".card-value"),
    ).not.toHaveText("--", { timeout: 10_000 });
    await expect(
      sessionsCard.locator(".skeleton-value"),
    ).toHaveCount(0, { timeout: 5_000 });

    const cardValue = await sessionsCard
      .locator(".card-value")
      .textContent();
    const analyticsCount = parseInt(
      cardValue?.replace(/,/g, "") ?? "0",
      10,
    );

    // Navigation surfaces (list, status bar) show only root sessions
    // and must agree. The analytics summary counts subagents on top, so
    // it is higher by exactly the subagent count. Forks and empty
    // sessions stay excluded everywhere.
    expect(listCount, "session list").toBe(EXPECTED_ROOT_SESSIONS);
    expect(statsCount, "status bar").toBe(EXPECTED_ROOT_SESSIONS);
    expect(analyticsCount, "analytics summary").toBe(
      EXPECTED_ANALYTICS_SESSIONS,
    );
  });
});
