import { expect, test } from "@playwright/test";
import { SessionsPage } from "./pages/sessions-page";

const states = [
  "not-set-up", "disconnected", "setup-expanded", "ready", "syncing", "done",
  "signed-out", "other-profile", "extension-update", "app-update", "failed",
  "desktop", "desktop-signed-out",
] as const;
for (const theme of ["light", "dark"]) {
  for (const state of states) {
    test(`${theme} Claude.ai ${state}`, async ({ page }, testInfo) => {
      const desktop = state.startsWith("desktop");
      await page.addInitScript(({ theme, desktop, syncing }) => {
        localStorage.setItem("theme", theme);
        localStorage.setItem("agentsview-locale", "en");
        if (desktop) {
          Object.assign(window, { __TAURI__: { core: { invoke: async () => undefined } } });
        }
        if (syncing) {
          const fetch = window.fetch;
          window.fetch = async (input, options) => {
            if (String(input).includes("/api/v1/import/claude-ai/sync")) {
              return new Response(new ReadableStream({
                start(controller) {
                  controller.enqueue(new TextEncoder().encode('event: progress\ndata: {"imported":128,"updated":0,"skipped":0,"errors":0}\n\n'));
                  options?.signal?.addEventListener("abort", () => controller.error(new DOMException("Aborted", "AbortError")), { once: true });
                },
              }), { headers: { "Content-Type": "text/event-stream" } });
            }
            return fetch(input, options);
          };
        }
      }, { theme, desktop, syncing: state === "syncing" });
      let polls = 0;
      await page.route("**/api/v1/import/claude-ai/chrome", async (route) => {
        polls++;
        await route.fulfill({ json: {
          installed: state !== "not-set-up",
          connected: !["not-set-up", "disconnected", "setup-expanded"].includes(state),
          other_profile: state === "other-profile",
        } });
      });
      await page.route("**/api/v1/import/claude-ai/sync**", async (route) => {
        const code = state === "extension-update" ? "claude_ai_chrome_host_update_required"
          : state === "app-update" ? "claude_ai_agentsview_update_required"
          : ["signed-out", "other-profile", "desktop-signed-out"].includes(state) ? "claude_ai_auth_required" : undefined;
        const event = state === "done" ? 'event: done\ndata: {"imported":12,"updated":3,"skipped":297,"errors":0}\n\n'
          : `event: error\ndata: ${JSON.stringify({ error: "Network interrupted", code })}\n\n`;
        await route.fulfill({ contentType: "text/event-stream", body: event });
      });
      await new SessionsPage(page).goto();
      await page.locator(".import-btn").click();
      const dialog = page.getByRole("dialog");
      const sync = dialog.getByRole("button", { name: "Sync", exact: true });
      if (["syncing", "done", "signed-out", "other-profile", "extension-update", "app-update", "failed", "desktop-signed-out"].includes(state)) {
        await expect(sync).toBeEnabled();
        await sync.click();
      }
      if (state === "not-set-up") {
        await expect(dialog.getByText("Not set up", { exact: true })).toBeVisible();
        await expect(sync).toBeDisabled();
        await expect(dialog.getByText("agentsview chrome setup", { exact: true })).toBeVisible();
      } else if (state === "disconnected" || state === "setup-expanded") {
        await expect(dialog.getByText("Not connected", { exact: true })).toBeVisible();
        await expect(sync).toBeDisabled();
        if (state === "setup-expanded") {
          await dialog.getByRole("button", { name: "Setup steps" }).click();
          await expect(dialog.getByText("agentsview chrome setup", { exact: true })).toBeVisible();
        }
      } else if (state === "syncing") {
        await expect(dialog.getByText("128 conversations processed...")).toBeVisible();
        await expect(dialog.getByRole("button", { name: "Stop" })).toBeVisible();
      } else if (state === "done") {
        await expect(dialog.getByText("312 conversations processed")).toBeVisible();
        for (const count of ["12", "3", "297"]) await expect(dialog.getByText(count, { exact: true })).toBeVisible();
      } else if (["signed-out", "other-profile", "desktop-signed-out"].includes(state)) {
        await expect(dialog.getByText("Signed out", { exact: true })).toBeVisible();
        await expect(dialog.getByRole("button", { name: "Sign in" })).toBeVisible();
        if (state === "other-profile") await expect(dialog.getByText("Another Chrome profile also has the extension. AgentsView uses the profile that connected first.")).toBeVisible();
      } else if (state === "extension-update" || state === "app-update") {
        await expect(dialog.getByText("Update needed", { exact: true })).toBeVisible();
        if (state === "app-update") await expect(sync).toBeDisabled();
        else await expect(dialog.getByText("agentsview chrome setup", { exact: true })).toBeVisible();
      } else if (state === "failed") {
        await expect(dialog.getByRole("alert")).toContainText("Sync failed");
        await expect(dialog.getByRole("button", { name: "Retry" })).toBeVisible();
      } else if (state === "ready") {
        await expect(dialog.getByText("Connected", { exact: true })).toBeVisible();
        await expect(sync).toBeEnabled();
      }
      if (desktop) {
        await expect(dialog.getByText("Account", { exact: true })).toBeVisible();
        expect(polls).toBe(0);
      }
      const box = await dialog.boundingBox();
      expect(box!.width).toBe(460);
      await dialog.screenshot({ path: testInfo.outputPath(`${theme}-${state}.png`), animations: "disabled" });
    });
  }
}
