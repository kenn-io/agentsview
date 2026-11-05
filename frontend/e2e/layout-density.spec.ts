import { expect, test, type Page } from "@playwright/test";
import { SessionsPage } from "./pages/sessions-page";

async function toolRowHeights(page: Page, layout: string): Promise<number[]> {
  await page.addInitScript((value) => {
    localStorage.setItem("agentsview-message-layout", value);
  }, layout);
  const sp = new SessionsPage(page);
  await sp.goto();
  // Open sessions until one shows a grouped run of tool calls.
  const count = await sp.sessionItems.count();
  const rows = page.locator(".tool-group-body .tool-header-row, .pg-members .tool-header-row");
  for (let index = 0; index < count; index++) {
    await sp.selectSession(index);
    if ((await rows.count()) > 0) break;
  }
  await expect(rows.first()).toBeVisible();
  return rows.evaluateAll((elements) =>
    elements.slice(0, 6).map((element) => element.getBoundingClientRect().height),
  );
}

test("Compact packs grouped tool rows tighter than Default", async ({ browser }) => {
  const defaultPage = await browser.newPage();
  const defaults = await toolRowHeights(defaultPage, "default");
  await defaultPage.close();

  const compactPage = await browser.newPage();
  const compact = await toolRowHeights(compactPage, "compact");
  await compactPage.close();

  for (const height of defaults) expect(height).toBeCloseTo(24, 0);
  for (const height of compact) expect(height).toBeCloseTo(22, 0);
});
