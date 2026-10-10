import { expect, test } from "@playwright/test";
import { SessionsPage } from "./pages/sessions-page";

test("connects from Chrome status and syncs through Chrome", async ({ page }) => {
  await page.addInitScript(() => localStorage.setItem("agentsview-locale", "en"));
  await page.route("**/api/v1/import/claude-ai/chrome", (route) => route.fulfill({ json: {
    connected: true,
  } }));
  await page.route("**/api/v1/import/claude-ai/sync**", (route) => route.fulfill({
    contentType: "text/event-stream",
    body: 'event: done\ndata: {"imported":12,"updated":3,"skipped":297,"errors":0}\n\n',
  }));
  await new SessionsPage(page).goto();
  await page.locator(".import-btn").click();
  const dialog = page.getByRole("dialog");
  await expect(dialog.getByText("Connected", { exact: true })).toBeVisible();
  const request = page.waitForRequest((request) => request.method() === "POST" &&
    new URL(request.url()).pathname === "/api/v1/import/claude-ai/sync" &&
    new URL(request.url()).searchParams.get("browser") === "chrome");
  await dialog.getByRole("button", { name: "Sync", exact: true }).click();
  await request;
  await expect(dialog.getByText("312 conversations processed")).toBeVisible();
});
