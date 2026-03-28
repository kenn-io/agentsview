import { test, expect } from "@playwright/test";
import { SessionsPage } from "./pages/sessions-page";

test.describe("Transcript strip", () => {
  let sp: SessionsPage;

  test.beforeEach(async ({ page }) => {
    sp = new SessionsPage(page);
    await sp.goto();
    await sp.selectFirstSession();
  });

  test("pills fill full height of transcript-strip container", async ({
    page,
  }) => {
    const strip = page.locator(".transcript-strip");
    await expect(strip).toBeVisible();

    const activePill = strip.locator(".pill.active");
    await expect(activePill).toBeVisible();

    const stripBox = await strip.boundingBox();
    const pillBox = await activePill.boundingBox();

    expect(stripBox).toBeTruthy();
    expect(pillBox).toBeTruthy();

    // The pill should fill the container height (minus the 1px border
    // on each side = 2px total).
    const stripInner = stripBox!.height - 2;
    expect(pillBox!.height).toBeGreaterThanOrEqual(stripInner);
  });
});
