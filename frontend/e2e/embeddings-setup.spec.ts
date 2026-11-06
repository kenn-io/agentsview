import { expect, test } from "@playwright/test";

for (const width of [1280, 390]) {
  for (const entry of width === 390 ? ["settings"] : ["settings", "semantic"]) {
    test(`${entry} lays out search by meaning at ${width}px`, async ({ page }) => {
      await page.setViewportSize({ width, height: 850 });
      let phase = "off";
      const setup = () => ({
        enabled: phase !== "off",
        action: phase.startsWith("ollama") ? "use-built-in" : phase === "off" ? "turn-on" : "",
        provider: phase.startsWith("ollama") ? "ollama" : "built-in",
        rebuild_required: phase === "ollama-other",
        missing_bytes: phase === "off" || phase.startsWith("ollama") ? 295000000 : 0,
        installing: false,
        busy: false,
        downloaded_bytes: 0,
        total_bytes: 0,
      });
      await page.route("**/api/v1/embeddings/status*", (route) =>
        route.fulfill({
          json: {
            running: false,
            done: 0,
            total: 0,
            setup: setup(),
            ...(phase === "failed" ? { last_error: "encoder unavailable" } : {}),
          },
        }),
      );
      await page.route("**/api/v1/embeddings/generations*", (route) =>
        route.fulfill({ json: { generations: [] } }),
      );
      await page.route("**/api/v1/search?**", (route) => route.fulfill({ json: { results: [] } }));
      await page.route("**/api/v1/search/content?**", (route) =>
        route.fulfill({
          status: 501,
          json: { error: "semantic search not available", code: "semantic_setup_required" },
        }),
      );
      await page.goto(entry === "settings" ? "/settings" : "/");
      if (entry === "settings") {
        await page
          .getByRole("navigation", { name: "Settings" })
          .getByRole("button", { name: /^Embeddings/ })
          .click();
      } else {
        await page.getByRole("button", { name: /Search sessions/ }).click();
        if (width === 1280 && entry === "semantic") {
          await test.step("preserves the compact palette key badge", async () => {
            const escapeBadge = page.locator(".palette-input-wrap kbd").first();
            expect
              .soft(await escapeBadge.evaluate((el) => getComputedStyle(el).fontFamily))
              .toBe(
                await page
                  .locator(".palette-input")
                  .evaluate((el) => getComputedStyle(el).fontFamily),
              );
          });
        }
        if (entry === "semantic")
          await page.getByRole("radio", { name: "Semantic", exact: true }).click();
        await page.locator(".palette-input").fill("connections");
      }
      if (entry === "settings") {
        phase = "failed";
        await page.reload();
        await page
          .getByRole("navigation", { name: "Settings" })
          .getByRole("button", { name: /^Embeddings/ })
          .click();
        await expect(page.getByRole("alert")).toContainText("encoder unavailable");
        const failedChip = page.locator(".kit-search-by-meaning .kit-chip");
        await expect(failedChip).toHaveText("On");
        await expect(failedChip).toHaveCSS("text-transform", "none");
        const retry = page.getByRole("button", { name: "Retry", exact: true });
        const errorBox = (await page.getByRole("alert").boundingBox())!;
        const retryBox = (await retry.boundingBox())!;
        expect(Math.max(0, retryBox.y - errorBox.y - errorBox.height)).toBeLessThanOrEqual(8);
        const generationsBox = (await page
          .locator(".embeddings-settings .subsection")
          .boundingBox())!;
        expect(
          generationsBox.y - Math.max(errorBox.y + errorBox.height, retryBox.y + retryBox.height),
        ).toBeGreaterThanOrEqual(16);
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
      }
    });
  }
}
