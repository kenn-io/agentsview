import { test, expect, type Page } from "@playwright/test";

// Each route shows a failed read the same way: an alert naming the failure
// with a Retry that loads the page once the server recovers. The server is
// real; only the named read is made to fail until the test lets it through.

const FAILURE = "injected read failure";

type RecoveryWindow = Window & { readRecoveredByRetry?: boolean };

async function failReads(page: Page, matches: (url: URL) => boolean): Promise<void> {
  await page.route(
    (url) => matches(url),
    async (route) => {
      const recovered = await page.evaluate(() => (window as RecoveryWindow).readRecoveredByRetry);
      if (!recovered && route.request().method() === "GET") {
        await route.fulfill({ status: 500, json: { error: FAILURE } });
        return;
      }
      await route.continue();
    },
  );
}

async function recoverReadsOnRetry(page: Page): Promise<void> {
  // Transcript reads retry automatically after a 500. Keep those retries
  // failing until the user click reaches the page, or they can remove the
  // button while Playwright is still preparing the click. Listen on the
  // document because a failing automatic reload can also replace the button.
  await page.evaluate((failure) => {
    document.addEventListener(
      "click",
      (event) => {
        const button = event.target instanceof Element ? event.target.closest("button") : null;
        if (
          button?.textContent?.trim() === "Retry" &&
          button.closest('[role="alert"]')?.textContent?.includes(failure)
        ) {
          (window as RecoveryWindow).readRecoveredByRetry = true;
        }
      },
      { capture: true },
    );
  }, FAILURE);
}

const cases: Array<{ name: string; path: string; read: (url: URL) => boolean }> = [
  { name: "trash", path: "/trash", read: (url) => url.pathname === "/api/v1/trash" },
  { name: "pinned", path: "/pinned", read: (url) => url.pathname === "/api/v1/pins" },
  {
    name: "recent-edits",
    path: "/recent-edits",
    read: (url) => url.pathname === "/api/v1/recent-edits",
  },
  {
    name: "activity",
    path: "/activity",
    read: (url) => url.pathname === "/api/v1/activity/report",
  },
  { name: "usage", path: "/usage", read: (url) => url.pathname === "/api/v1/usage/summary" },
  {
    name: "quality",
    path: "/quality",
    read: (url) => url.pathname === "/api/v1/analytics/signals",
  },
  {
    name: "recall-generated",
    path: "/recall?tab=generated",
    read: (url) => url.pathname === "/api/v1/insights",
  },
  { name: "settings", path: "/settings", read: (url) => url.pathname === "/api/v1/settings" },
  {
    name: "sessions-sidebar",
    path: "/",
    read: (url) => url.pathname === "/api/v1/sessions/sidebar-index",
  },
  {
    name: "transcript",
    path: "/sessions/test-session-recent-edits",
    read: (url) => url.pathname === "/api/v1/sessions/test-session-recent-edits/messages",
  },
];

test.describe("Failed page loads", () => {
  for (const { name, path, read } of cases) {
    test(`${name} shows the failure and recovers through Retry`, async ({ page }, testInfo) => {
      await failReads(page, read);
      await page.goto(path);

      const alert = page.getByRole("alert").filter({ hasText: FAILURE }).first();
      await expect(alert).toBeVisible();
      await expect(alert).toContainText("Failed to load");
      await page.screenshot({ path: testInfo.outputPath(`${name}.png`) });

      const retry = alert.getByRole("button", { name: "Retry" });
      await recoverReadsOnRetry(page);
      const retried = page.waitForResponse(
        (response) =>
          read(new URL(response.url())) && response.request().method() === "GET" && response.ok(),
      );
      await retry.click();
      await retried;
      await expect(page.getByRole("alert").filter({ hasText: "Failed to load" })).toHaveCount(0);
    });
  }
});
