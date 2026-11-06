import { expect, test } from "@playwright/test";

for (const width of [1280, 390]) {
  test(`lays out search by meaning at ${width}px`, async ({ page }) => {
    await page.setViewportSize({ width, height: 850 });
    const setup = () => ({
      enabled: true,
      action: "",
      provider: "built-in",
      missing_bytes: 0,
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
          last_error: "encoder unavailable",
        },
      }),
    );
    await page.route("**/api/v1/embeddings/generations*", (route) =>
      route.fulfill({ json: { generations: [] } }),
    );
    const openEmbeddings = async () => {
      await page.goto("/settings");
      await page
        .getByRole("navigation", { name: "Settings" })
        .getByRole("button", { name: /^Embeddings/ })
        .click();
    };
    await openEmbeddings();
    await expect(page.getByRole("alert")).toContainText("encoder unavailable");
    await expect(page.locator(".kit-search-by-meaning .kit-chip")).toHaveText("On");
    const errorBox = (await page.getByRole("alert").boundingBox())!;
    const generationsBox = (await page.locator(".embeddings-settings .subsection").boundingBox())!;
    expect(generationsBox.y - errorBox.y - errorBox.height).toBeGreaterThanOrEqual(16);
  });
}
