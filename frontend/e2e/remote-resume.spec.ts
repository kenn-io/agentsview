import { test, expect } from "@playwright/test";
import {
  createMockSessions,
  handleSessionsRoute,
  sessionsRoutePattern,
} from "./helpers/mock-sessions";

test.skip(
  ({ browserName }) => browserName !== "chromium",
  "The clipboard proof uses Chromium permissions.",
);

for (const width of [1280, 768, 400]) {
  test(`remote copy menu and clipboard at ${width}`, async ({ page }, testInfo) => {
    await page.setViewportSize({ width, height: 900 });
    await page
      .context()
      .grantPermissions(["clipboard-read", "clipboard-write"], { origin: "http://127.0.0.1:8090" });
    const session = {
      ...createMockSessions(1, "remote", () => "project")[0]!,
      id: "devbox1~claude:abc-123",
      agent: "claude",
      machine: "devbox1",
      first_message: "Remote resume command proof",
    };
    const command = "cd '/home/user/project' && claude --resume abc-123";
    const resumeBodies: unknown[] = [];
    const fileActions: string[] = [];
    await page.route(
      sessionsRoutePattern,
      handleSessionsRoute([{ sessions: [session], project: null }]),
    );
    await page.route("**/api/v1/openers", (route) =>
      route.fulfill({
        json: {
          openers: [
            { id: "kitty", name: "Kitty", kind: "terminal", bin: "kitty" },
            { id: "code", name: "VS Code", kind: "editor", bin: "code" },
            { id: "finder", name: "Finder", kind: "files", bin: "open" },
            { id: "claude-desktop", name: "Claude Desktop", kind: "action", bin: "open" },
          ],
        },
      }),
    );
    await page.route("**/api/v1/sessions/*/resume", async (route) => {
      expect(decodeURIComponent(new URL(route.request().url()).pathname)).toBe(
        `/api/v1/sessions/${session.id}/resume`,
      );
      expect(route.request().method()).toBe("POST");
      resumeBodies.push(route.request().postDataJSON());
      await route.fulfill({ json: { launched: false, command, cwd: "/home/user/project" } });
    });
    await page.route("**/api/v1/sessions/*/open", async (route) => {
      fileActions.push(route.request().url());
      await route.fulfill({ status: 400, json: { error: "cannot open remote session" } });
    });
    await page.route("**/api/v1/sessions/*/messages*", (route) =>
      route.fulfill({ json: { messages: [], total: 0 } }),
    );
    await page.route("**/api/v1/sessions/*/directory", (route) =>
      route.fulfill({ json: { path: "" } }),
    );
    await page.goto(`http://127.0.0.1:8090/sessions/${encodeURIComponent(session.id)}`);
    await page.locator(".resume-btn").click();
    const menu = page.locator(".open-menu");
    await expect(menu).toBeVisible();
    await expect(menu.getByRole("button")).toHaveCount(1);
    await expect(menu.getByRole("button", { name: "Copy command", exact: true })).toBeVisible();
    await expect(menu.locator(".open-menu-divider")).toHaveCount(0);
    const bounds = await menu.boundingBox();
    expect(bounds).not.toBeNull();
    expect(bounds!.x).toBeGreaterThanOrEqual(0);
    expect(bounds!.x + bounds!.width).toBeLessThanOrEqual(width);
    expect(bounds!.y + bounds!.height).toBeLessThanOrEqual(900);
    expect(await menu.evaluate((el) => el.scrollWidth <= el.clientWidth)).toBe(true);
    await page.screenshot({
      path: testInfo.outputPath(`remote-menu-${width}.png`),
      fullPage: true,
    });
    await page.keyboard.press("1");
    expect(resumeBodies).toEqual([]);
    await menu.getByRole("button", { name: "Copy command", exact: true }).click();
    await expect.poll(() => page.evaluate(() => navigator.clipboard.readText())).toBe(command);
    expect(resumeBodies).toEqual([{ command_only: true }]);
    expect(fileActions).toEqual([]);
    await expect(page.locator(".resume-btn")).toContainText("Command copied!");
  });
}
