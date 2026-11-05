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
  });

  for (const width of [320, 375]) {
    test(`keeps the file name visible at ${width}px`, async ({ page }) => {
      await page.setViewportSize({ width, height: 800 });
      await page.goto("/recent-edits");
      const row = page.locator(".re-file-row").first();
      await expect(row).toBeVisible({ timeout: 5_000 });
      await expect(row.locator(".re-project")).toHaveAttribute("title", /.+/);

      const layout = await row.evaluate((element) => {
        const rowRect = element.getBoundingClientRect();
        const rect = (selector: string) => element.querySelector(selector)!.getBoundingClientRect();
        const path = element.querySelector<HTMLElement>(".re-path")!;
        const project = rect(".re-project");
        const pathRect = path.getBoundingClientRect();
        const count = rect(".re-count");
        return {
          pathWidth: pathRect.width,
          pathText: path.textContent ?? "",
          pathClipped: path.scrollWidth > path.clientWidth + 1,
          projectAbovePath: project.bottom <= pathRect.top + 1,
          countInsideRow: count.right <= rowRect.right + 1,
        };
      });
      // The fixture's short path ("server/handler.go") fits whole.
      expect(layout.pathText).toContain("handler.go");
      expect(layout.pathClipped).toBe(false);
      expect(layout.pathWidth).toBeGreaterThan(80);
      expect(layout.projectAbovePath).toBe(true);
      expect(layout.countInsideRow).toBe(true);
    });
  }

  test("expand file row and jump to session transcript", async ({ page }) => {
    // Navigate to Recent Edits via its TopBar tab.
    await clickNavTab(page, "Recent Edits");

    await expect(page.locator(".re-file-row").first()).toBeVisible({ timeout: 5_000 });

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
    await expect(ml).toHaveAttribute("data-session-id", FIXTURE_SESSION_ID, { timeout: 5_000 });
    await expect(ml).toHaveAttribute("data-loaded", "true", {
      timeout: 5_000,
    });
  });
});
