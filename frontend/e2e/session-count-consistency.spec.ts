import { test, expect } from "@playwright/test";
import { SessionsPage } from "./pages/sessions-page";

// The test fixture seeds 13 root sessions with messages, plus 3 subagent
// sessions, 1 fork, and 1 empty session. Navigation surfaces show only the
// 13 root sessions. Analytics also counts the 3 subagents, so it shows 16.
// Forks and empty sessions are excluded everywhere.
const EXPECTED_ROOT_SESSIONS = 13;
const EXPECTED_ANALYTICS_SESSIONS = 16; // 13 root + 3 subagents

test.describe("Session count consistency", () => {
  let sp: SessionsPage;
  let summaryUrl: URL;

  test.beforeEach(async ({ page }) => {
    sp = new SessionsPage(page);
    const summary = page.waitForResponse((response) => response.url().includes("/analytics/summary?") && response.ok());
    await sp.goto();
    summaryUrl = new URL((await summary).url());
  });

  test("navigation counts agree and analytics counts subagents on top", async ({ page }) => {
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

    const sessionsCard = summaryCards
      .locator(".card")
      .filter({ has: page.locator(".card-label", { hasText: /^Sessions$/ }) });

    // Navigation surfaces (list, status bar) show only root sessions
    // and must agree. The analytics summary counts subagents on top, so
    // it is higher by exactly the subagent count. Forks and empty
    // sessions stay excluded everywhere.
    expect(listCount, "session list").toBe(EXPECTED_ROOT_SESSIONS);
    expect(statsCount, "status bar").toBe(EXPECTED_ROOT_SESSIONS);
    await expect(sessionsCard.locator(".card-value"), "analytics summary").toHaveText(
      String(EXPECTED_ANALYTICS_SESSIONS),
    );
    const dates = ["from", "to"].map((key) => new Date(`${summaryUrl.searchParams.get(key)}T00:00:00`).toLocaleDateString("en", { month: "short", day: "numeric", year: "numeric" }));
    await expect(sessionsCard.locator(".card-sub")).toHaveText(`${dates[0]} – ${dates[1]}\u00a0· incl.\u00a0subagents`);
  });
});
