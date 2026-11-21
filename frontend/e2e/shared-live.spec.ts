import { expect, test } from "@playwright/test";

test("several visible session tabs leave connections for opening another session", async ({
  context,
  page,
}) => {
  await context.addInitScript(() => {
    Object.defineProperty(document, "hidden", { configurable: true, get: () => false });
    Object.defineProperty(document, "visibilityState", {
      configurable: true,
      get: () => "visible",
    });
  });
  const pages = [page, await context.newPage(), await context.newPage()];
  for (const tab of pages) {
    await tab.goto("/sessions/test-session-small-5");
    await expect(tab).toHaveURL(/test-session-small-5/);
  }
  await page.waitForTimeout(2000);
  const fourth = await context.newPage();
  await fourth.goto("/", { timeout: 8000 });
  await fourth.keyboard.press("Control+g");
  const dialog = fourth.getByRole("dialog", { name: "Go to session" });
  await expect(dialog).toBeVisible();
  await fourth.getByRole("textbox", { name: "Session ID or UUID" }).fill("test-session-small-5");
  await fourth.keyboard.press("Enter");
  await expect(fourth).toHaveURL(/test-session-small-5/);
});
