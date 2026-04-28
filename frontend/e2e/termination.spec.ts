import { test, expect } from "@playwright/test";
import { SessionsPage } from "./pages/sessions-page";

// The fixture marks `test-session-mixed-content-7` (project-beta,
// 7 messages) with termination_status = "tool_call_pending".
// All other fixture sessions have a NULL status.
const UNCLEAN_SESSION_ID = "test-session-mixed-content-7";

test.describe("session termination status", () => {
  test("unclean session shows banner on detail page", async ({
    page,
  }) => {
    await page.goto(`/sessions/${UNCLEAN_SESSION_ID}`);
    await expect(
      page.getByText(/tool call that never received a response/i),
    ).toBeVisible();
  });

  test("status filter narrows session list", async ({ page }) => {
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
  });

  test("Top Sessions table renders unclean status glyph", async ({
    page,
  }) => {
    // AnalyticsPage renders inside the right pane on bare "/"
    // when no session is selected.
    await page.goto("/");
    await expect(
      page.locator(".status-glyph--unclean").first(),
    ).toBeVisible();
  });
});
