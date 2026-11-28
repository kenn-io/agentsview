import { expect, type Page } from "@playwright/test";

export async function waitForVisitPageReady(page: Page): Promise<void> {
  // WebKit cancels unfinished dashboard reads during navigation. Their error
  // reports can fail too and stop visit tracking before the browser's pagehide
  // event. Finish those reads so the test measures delivery with reporting enabled.
  const dashboard = page.locator(".analytics-content");
  await expect(dashboard).toHaveAttribute("aria-busy", "false");
  await expect(dashboard.locator(".loading-state")).toHaveCount(0);
}
