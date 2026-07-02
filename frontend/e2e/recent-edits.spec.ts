import { test, expect } from "@playwright/test";
import { clickNavTab } from "./helpers/nav";

// The fixture session created by createRecentEditsFixture in
// cmd/testfixture/main.go. It carries an Edit tool call on
// /src/server/handler.go with FilePath set directly so the feed
// filter (category IN ('Edit','Write') AND file_path IS NOT NULL)
// returns a row.
const FIXTURE_SESSION_ID = "test-session-recent-edits";

test.describe("Recent Edits feed", () => {
  test.beforeEach(async ({ page }) => {
    await page.goto("/");
    await expect(page.locator(".session-item").first()).toBeVisible({
      timeout: 5_000,
    });
  });

  test("page renders with a file row from fixture data", async ({
    page,
  }) => {
    // Recent Edits is a top-level TopBar tab since the kit-ui migration.
    await clickNavTab(page, "Recent Edits");

    // The page container and heading should appear.
    await expect(
      page.locator(".recent-edits-page"),
    ).toBeVisible({ timeout: 5_000 });
    await expect(
      page.locator(".recent-edits-page h2"),
    ).toHaveText("Recent Edits");

    // Feed must not be empty — the fixture seeds one Edit call.
    await expect(page.locator(".re-empty")).toHaveCount(0);
    await expect(
      page.locator(".re-file-row").first(),
    ).toBeVisible({ timeout: 5_000 });
  });

  test("expand file row and jump to session transcript", async ({
    page,
  }) => {
    // Navigate to Recent Edits via its TopBar tab.
    await clickNavTab(page, "Recent Edits");

    await expect(
      page.locator(".re-file-row").first(),
    ).toBeVisible({ timeout: 5_000 });

    // Expand the first file row.
    await page.locator(".re-file-row").first().click();

    // The edits panel should appear with at least one jump button.
    const editsPanel = page.locator(".re-edits").first();
    await expect(editsPanel).toBeVisible({ timeout: 3_000 });
    const editBtn = editsPanel.locator(".re-edit").first();
    await expect(editBtn).toBeVisible();

    // Click the edit to jump to the session transcript.
    await editBtn.click();

    // The message list must reflect the fixture session.
    const ml = page.locator(".message-list-scroll");
    await expect(ml).toHaveAttribute(
      "data-session-id",
      FIXTURE_SESSION_ID,
      { timeout: 5_000 },
    );
    await expect(ml).toHaveAttribute("data-loaded", "true", {
      timeout: 5_000,
    });
  });
});
