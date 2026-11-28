import { expect, test, type Page, type Request } from "@playwright/test";
import { clickNavTab } from "./helpers/nav";

async function holdFirstRoute(page: Page, pattern: string) {
  let releaseGate!: () => void;
  let reportStarted!: (request: Request) => void;
  let first = true;
  const gate = new Promise<void>((resolve) => {
    releaseGate = resolve;
  });
  const started = new Promise<Request>((resolve) => {
    reportStarted = resolve;
  });

  await page.route(pattern, async (route) => {
    if (!first) {
      await route.continue();
      return;
    }
    first = false;
    reportStarted(route.request());
    await gate;
    try {
      await route.continue();
    } catch (error) {
      if (!/already handled|cancel|closed/i.test(String(error))) throw error;
    }
  });

  let released = false;
  return {
    started,
    release() {
      if (released) return;
      released = true;
      releaseGate();
    },
  };
}

test("switching top-level tabs aborts the hidden activity read", async ({ page }) => {
  const held = await holdFirstRoute(page, "**/api/v1/activity/report*");
  try {
    await page.goto("/activity");
    const activityRequest = await held.started;
    const failed = page.waitForEvent("requestfailed", (request) => request === activityRequest);
    const usageStarted = page.waitForRequest((request) =>
      new URL(request.url()).pathname.endsWith("/api/v1/usage/summary"),
    );

    await clickNavTab(page, "Usage");

    await expect(page.locator(".usage-page")).toBeVisible();
    await usageStarted;
    expect((await failed).failure()).not.toBeNull();
  } finally {
    held.release();
  }
});
