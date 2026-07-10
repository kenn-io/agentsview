import { test, expect } from "@playwright/test";
import { SessionsPage } from "./pages/sessions-page";

test.describe("Navigation", () => {
  let sp: SessionsPage;

  test.beforeEach(async ({ page }) => {
    sp = new SessionsPage(page);
    await sp.goto();
  });

  test("keyboard ] navigates to next session", async () => {
    await sp.sessionItems.first().click();
    await expect(sp.sessionItems.first()).toHaveClass(/active/);

    await sp.pressNextSessionShortcut();
    await expect(sp.sessionItems.nth(1)).toHaveClass(/active/);
  });

  test("keyboard [ navigates to previous session", async () => {
    await sp.sessionItems.nth(1).click();
    await expect(sp.sessionItems.nth(1)).toHaveClass(/active/);

    await sp.pressPreviousSessionShortcut();
    await expect(sp.sessionItems.first()).toHaveClass(/active/);
  });

  test("analytics page shows when no session selected", async () => {
    await expect(sp.analyticsPage).toBeVisible();
    await expect(sp.analyticsToolbar).toBeVisible();
    await expect(sp.exportBtn).toContainText("Export CSV");
  });

  test("Shift+J and Shift+K navigate visible user prompts", async ({ page }, testInfo) => {
    const session = page
      .locator(".session-item")
      .filter({ hasText: "project-beta" })
      .filter({ hasText: "3" });
    await session.first().click();
    await expect(sp.messageRows).toHaveCount(6);

    const users = sp.messageRows.filter({
      has: page.locator(".message.is-user"),
    });
    await users.first().click();
    await expect(users.first()).toHaveClass(/selected/);

    const assistants = sp.messageRows.filter({
      has: page.locator(".message:not(.is-user)"),
    });
    await page.keyboard.press("j");
    await expect(assistants.first()).toHaveClass(/selected/);
    await users.first().click();

    await page.keyboard.press("Shift+J");
    await expect(users.nth(1)).toHaveClass(/selected/);
    await page.keyboard.press("Shift+K");
    await expect(users.first()).toHaveClass(/selected/);

    await users.nth(1).click();
    await sp.toggleSortOrder();
    await expect(users.nth(1)).toHaveClass(/selected/);
    await page.keyboard.press("Shift+J");
    await expect(users.nth(2)).toHaveClass(/selected/);
    await users.nth(1).click();
    await page.keyboard.press("Shift+K");
    await expect(users.nth(0)).toHaveClass(/selected/);

    await page.keyboard.press("?");
    await expect(page.getByText("Next user prompt")).toBeVisible();
    await expect(page.getByText("Previous user prompt")).toBeVisible();

    const shortcutsModal = page.getByRole("dialog", {
      name: "Keyboard Shortcuts",
    });
    for (const width of [1280, 768, 400]) {
      await page.setViewportSize({ width, height: 800 });
      const box = await shortcutsModal.boundingBox();
      expect(box).not.toBeNull();
      expect(box!.x).toBeGreaterThanOrEqual(0);
      expect(box!.x + box!.width).toBeLessThanOrEqual(width);
      await page.screenshot({
        path: testInfo.outputPath(`shortcuts-${width}.png`),
      });
    }
  });
});
