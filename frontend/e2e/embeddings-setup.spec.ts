import { expect, test } from "@playwright/test";

for (const width of [1280, 390]) {
  for (const entry of ["settings", "semantic", "hint"]) {
    test(`${entry} activates search by meaning at ${width}px`, async ({ page }) => {
      await page.setViewportSize({ width, height: 850 });
      let phase = "off";
      let actions = 0;
      const setup = () => ({ enabled: phase !== "off", action: phase === "off" || phase === "download" ? "turn-on" : "", provider: "built-in", missing_bytes: 295000000, installing: false, busy: phase === "download", downloaded_bytes: phase === "download" ? 100000000 : 0, total_bytes: 295000000 });
      await page.route("**/api/v1/embeddings/status*", (route) => route.fulfill({ json: {
        running: phase === "index", done: phase === "index" ? 25 : 0, total: phase === "index" ? 100 : 0, eta_milliseconds: 3000, estimate_ready: phase === "index", phase: "embedding", setup: setup(),
        ...(phase === "ready" ? { build_id: 920, started_at: `2026-01-01T00:00:${width === 390 ? "01" : "02"}Z`, last_result: { Activated: true, Fill: { Documents: 100, Chunks: 100 } } } : {}),
      } }));
      await page.route("**/api/v1/embeddings/setup", async (route) => {
        expect(route.request().postDataJSON()).toEqual({ action: "turn-on" });
        actions++;
        phase = "download";
        await route.fulfill({ status: 202, json: { started: true } });
      });
      await page.route("**/api/v1/embeddings/generations*", (route) => route.fulfill({ json: { generations: [] } }));
      await page.route("**/api/v1/search?**", (route) => route.fulfill({ json: { results: [] } }));
      await page.route("**/api/v1/search/content?**", (route) => phase === "ready"
        ? route.fulfill({ json: { matches: [{ session_id: "test-session", project: "Sample", agent: "claude", content: "reuse database connections", snippet: "reuse database connections", ordinal: 0 }], total: 1 } })
        : route.fulfill({ status: 501, json: { detail: "semantic search not available" } }));
      await page.goto(entry === "settings" ? "/settings" : "/");
      if (entry === "settings") {
        await page.getByRole("navigation", { name: "Settings" }).getByRole("button", { name: /^Embeddings/ }).click();
      } else {
        await page.getByRole("button", { name: /Search sessions/ }).click();
        if (entry === "semantic") await page.getByRole("radio", { name: "Semantic", exact: true }).click();
        await page.locator(".palette-input").fill("connections");
      }
      await expect(page.getByRole("button", { name: "Turn on", exact: true })).toBeVisible();
      expect(actions).toBe(0);
      await expect(page.getByText(/Downloads 295 MB/)).toBeVisible();
      await page.getByRole("button", { name: "Turn on", exact: true }).click();
      await expect(page.getByText(/Downloading/).first()).toBeVisible();
      expect(actions).toBe(1);
      phase = "index";
      await expect(page.getByText(/Indexing/).first()).toBeVisible();
      phase = "ready";
      if (entry === "settings") await expect(page.locator(".kit-search-by-meaning").getByText("On", { exact: true })).toBeVisible();
      else await expect(page.locator(".palette-results")).toContainText("reuse database connections");
    });
  }
}
