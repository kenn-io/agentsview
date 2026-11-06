import { expect, test } from "@playwright/test";

for (const width of [1280, 390]) {
  for (const entry of ["settings", "semantic", "hint"]) {
    test(`${entry} activates search by meaning at ${width}px`, async ({ page }) => {
      await page.setViewportSize({ width, height: 850 });
      let phase = "off";
      const actions: unknown[] = [];
      const setup = () => ({
        enabled: phase !== "off",
        action: phase.startsWith("ollama")
          ? "use-built-in"
          : phase === "off" || phase === "download"
            ? "turn-on"
            : "",
        provider: phase.startsWith("ollama") ? "ollama" : "built-in",
        rebuild_required: phase === "ollama-other",
        missing_bytes:
          phase === "off" || phase === "download" || phase.startsWith("ollama") ? 295000000 : 0,
        installing: false,
        busy: phase === "download",
        downloaded_bytes: phase === "download" ? 100000000 : 0,
        total_bytes: phase === "download" ? 295000000 : 0,
      });
      await page.route("**/api/v1/embeddings/status*", (route) =>
        route.fulfill({
          json: {
            running: phase === "index",
            done: phase === "index" ? 25 : 0,
            total: phase === "index" ? 100 : 0,
            eta_milliseconds: 3000,
            estimate_ready: phase === "index",
            phase: "embedding",
            setup: setup(),
            ...(phase === "failed" ? { last_error: "encoder unavailable" } : {}),
            ...(phase === "ready"
              ? {
                  build_id: 920,
                  started_at: `2026-01-01T00:00:${width === 390 ? "01" : "02"}Z`,
                  last_result: { Activated: true, Fill: { Documents: 100, Chunks: 100 } },
                }
              : {}),
          },
        }),
      );
      await page.route("**/api/v1/embeddings/setup", async (route) => {
        actions.push(route.request().postDataJSON());
        phase = "download";
        await route.fulfill({ status: 202, json: { started: true } });
      });
      await page.route("**/api/v1/embeddings/build", async (route) => {
        expect(route.request().postDataJSON()).toEqual({});
        phase = "index";
        await route.fulfill({ status: 202, json: { started: true } });
      });
      await page.route("**/api/v1/embeddings/generations*", (route) =>
        route.fulfill({ json: { generations: [] } }),
      );
      await page.route("**/api/v1/search?**", (route) => route.fulfill({ json: { results: [] } }));
      await page.route("**/api/v1/search/content?**", (route) =>
        phase === "ready"
          ? route.fulfill({
              json: {
                matches: [
                  {
                    session_id: "test-session",
                    project: "Sample",
                    agent: "claude",
                    content: "reuse database connections",
                    snippet: "reuse database connections",
                    ordinal: 0,
                  },
                ],
                total: 1,
              },
            })
          : route.fulfill({ status: 501, json: { error: "semantic search not available" } }),
      );
      await page.goto(entry === "settings" ? "/settings" : "/");
      if (entry === "settings") {
        await page
          .getByRole("navigation", { name: "Settings" })
          .getByRole("button", { name: /^Embeddings/ })
          .click();
      } else {
        await page.getByRole("button", { name: /Search sessions/ }).click();
        const escapeBadge = page.locator(".palette-input-wrap kbd").first();
        expect
          .soft(await escapeBadge.evaluate((el) => getComputedStyle(el).fontFamily))
          .toBe(
            await page.locator(".palette-input").evaluate((el) => getComputedStyle(el).fontFamily),
          );
        if (entry === "semantic")
          await page.getByRole("radio", { name: "Semantic", exact: true }).click();
        await page.locator(".palette-input").fill("connections");
      }
      await expect(page.getByRole("button", { name: "Turn on", exact: true })).toBeVisible();
      expect(actions).toHaveLength(0);
      await page.getByRole("button", { name: "Turn on", exact: true }).click();
      await expect(page.locator(".kit-search-by-meaning [role=progressbar]")).toHaveAttribute(
        "aria-valuemax",
        "295000000",
      );
      expect(actions).toEqual([{ action: "turn-on" }]);
      phase = "index";
      await expect(page.locator(".kit-search-by-meaning [role=progressbar]")).toHaveAttribute(
        "aria-valuenow",
        "25",
      );
      const name = page.locator(".kit-search-by-meaning__name");
      const indexingNameTop = entry === "settings" ? (await name.boundingBox())!.y : 0;
      phase = "ready";
      if (entry === "settings") {
        await expect(
          page.locator(".kit-search-by-meaning").getByText("On", { exact: true }),
        ).toBeVisible();
        phase = "failed";
        await page.reload();
        await page
          .getByRole("navigation", { name: "Settings" })
          .getByRole("button", { name: /^Embeddings/ })
          .click();
        await expect(page.getByRole("alert")).toContainText("encoder unavailable");
        expect((await name.boundingBox())!.y).toBe(indexingNameTop);
        const retry = page.getByRole("button", { name: "Retry", exact: true });
        const errorBox = (await page.getByRole("alert").boundingBox())!;
        const retryBox = (await retry.boundingBox())!;
        expect(Math.max(0, retryBox.y - errorBox.y - errorBox.height)).toBeLessThanOrEqual(8);
        await retry.click();
        await expect(page.locator(".kit-search-by-meaning [role=progressbar]")).toHaveAttribute(
          "aria-valuenow",
          "25",
        );
        for (const provider of ["ollama", "ollama-other"]) {
          phase = provider;
          await page.reload();
          await page
            .getByRole("navigation", { name: "Settings" })
            .getByRole("button", { name: /^Embeddings/ })
            .click();
          const disclosure = page.getByText(/Downloads 295 MB/);
          await expect(disclosure).toBeVisible();
          const hint = page.locator(
            ".kit-search-by-meaning__line, .kit-search-by-meaning__warning",
          );
          expect
            .soft(await disclosure.evaluate((el) => getComputedStyle(el).fontSize))
            .toBe(await hint.evaluate((el) => getComputedStyle(el).fontSize));
          if (provider === "ollama")
            expect
              .soft(await disclosure.evaluate((el) => getComputedStyle(el).color))
              .toBe(await hint.evaluate((el) => getComputedStyle(el).color));
        }
      } else
        await expect(page.locator(".palette-results")).toContainText("reuse database connections");
    });
  }
}
