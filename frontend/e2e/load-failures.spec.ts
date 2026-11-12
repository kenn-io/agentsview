import { test, expect, type Page } from "@playwright/test";

// Each route shows a failed read the same way: an alert naming the failure
// with a Retry that loads the page once the server recovers. The server is
// real; only the named read is made to fail until the test lets it through.

const FAILURE = "injected read failure";

async function failReads(page: Page, matches: (url: URL) => boolean): Promise<() => void> {
  let failing = true;
  await page.route(
    (url) => matches(url),
    async (route) => {
      if (failing && route.request().method() === "GET") {
        await route.fulfill({ status: 500, json: { error: FAILURE } });
        return;
      }
      await route.continue();
    },
  );
  return () => {
    failing = false;
  };
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
  { name: "trends", path: "/trends", read: (url) => url.pathname === "/api/v1/trends/terms" },
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
      const recover = await failReads(page, read);
      await page.goto(path);

      const alert = page.getByRole("alert").filter({ hasText: FAILURE }).first();
      await expect(alert).toBeVisible();
      await expect(alert).toContainText("Failed to load");
      await page.screenshot({ path: testInfo.outputPath(`${name}.png`) });

      recover();
      await alert.getByRole("button", { name: "Retry" }).click();
      await expect(page.getByRole("alert").filter({ hasText: FAILURE })).toHaveCount(0);
    });
  }
});
