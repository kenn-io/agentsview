import { test, expect } from "@playwright/test";
import { SessionsPage } from "./pages/sessions-page";

// The fixture marks `test-session-mixed-content-7` (project-beta,
// 7 messages) with termination_status = "tool_call_pending".
// All other fixture sessions have a NULL status.
const UNCLEAN_SESSION_ID = "test-session-mixed-content-7";

test.describe("session termination status", () => {
  test("status filter narrows session list to unclean", async ({
    page,
  }) => {
    const sp = new SessionsPage(page);
    await sp.goto();

    // Open sidebar filters and click the Unclean pill.
    await page.locator(".filter-btn").click();
    await page
      .locator(".filter-dropdown .pill-btn", { hasText: /^Unclean$/ })
      .click();

    // Active-filter chip surfaces in the AnalyticsPage right pane
    // (no session selected).
    await expect(
      page.getByText(/Status:\s*Unclean/i),
    ).toBeVisible();

    // The fixture has exactly one unclean session.
    await expect(sp.sessionItems).toHaveCount(1);
    await expect(sp.sessionListHeader).toContainText(
      "1 sessions",
    );

    // Surviving session renders the unclean StatusDot.
    await expect(
      page.locator(".status-dot--unclean").first(),
    ).toBeVisible();
  });

  test("Top Sessions table renders unclean status dot", async ({
    page,
  }) => {
    // AnalyticsPage renders inside the right pane on bare "/"
    // when no session is selected.
    await page.goto("/");
    await expect(
      page.locator(".status-dot--unclean").first(),
    ).toBeVisible();
  });

  test("unclean session is reachable by direct URL", async ({
    page,
  }) => {
    // The detail page no longer shows a banner — the StatusDot in
    // the sidebar conveys the same signal — but the session must
    // still be navigable by ID.
    await page.goto(`/sessions/${UNCLEAN_SESSION_ID}`);
    await expect(
      page.locator(`.session-item[data-session-id="${UNCLEAN_SESSION_ID}"]`),
    ).toBeVisible();
  });
});
