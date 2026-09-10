import { expect, test, type Page } from "@playwright/test";

const previewPayload = {
  sessions: 3,
  changed: 3,
  payloads: 5,
  stored_bytes: 8192,
  decoded_bytes: 4096,
  projects: [
    {
      project: "my-project",
      sessions: 2,
      changed: 2,
      payloads: 4,
      stored_bytes: 6144,
      decoded_bytes: 3072,
    },
    {
      project: "other-project",
      sessions: 1,
      changed: 1,
      payloads: 1,
      stored_bytes: 2048,
      decoded_bytes: 1024,
    },
  ],
};

const applyPayload = {
  sessions: 3,
  changed: 3,
  payloads: 5,
  stored_bytes: 8192,
  decoded_bytes: 4096,
  projects: previewPayload.projects,
};

async function mockStripImageRoutes(page: Page) {
  await page.route("**/api/v1/data/strip-images/preview", async (route) => {
    await route.fulfill({ json: previewPayload });
  });
  await page.route("**/api/v1/data/strip-images", async (route) => {
    const body = route.request().postDataJSON() as { confirmed?: boolean };
    if (body?.confirmed === true) {
      await route.fulfill({ json: applyPayload });
    } else {
      await route.fulfill({
        status: 400,
        json: { title: "Bad Request", detail: "confirmed must be true" },
      });
    }
  });
}

async function openSettingsToolImages(page: Page) {
  await page.goto("/settings");
  // Wait for settings to load
  await expect(page.locator(".settings-loading")).toHaveCount(0, { timeout: 10_000 });
  // Click the Tool-result images category
  const nav = page.getByRole("navigation", { name: "Settings" });
  await nav.locator("button", { hasText: "Tool-result images" }).click();
  await expect(page.getByRole("heading", { name: "Tool-result images" })).toBeVisible();
}

test.describe("Settings: Tool-result images", () => {
  test("opens panel and shows intro", async ({ page }) => {
    await mockStripImageRoutes(page);
    await openSettingsToolImages(page);
    await expect(page.getByText("Preview which stored tool results")).toBeVisible();
  });

  test("preview shows counts after clicking Preview", async ({ page }) => {
    await mockStripImageRoutes(page);
    await openSettingsToolImages(page);

    const previewBtn = page.getByRole("button", { name: "Preview" });
    await previewBtn.click();

    await expect(page.getByText("5", { exact: false })).toBeVisible();
    await expect(page.getByText("my-project")).toBeVisible();
  });

  test("cancel in confirm dialog goes back to previewed", async ({ page }) => {
    await mockStripImageRoutes(page);
    await openSettingsToolImages(page);

    await page.getByRole("button", { name: "Preview" }).click();
    await expect(page.getByText("my-project")).toBeVisible();

    await page.getByRole("button", { name: "Remove image payloads" }).click();
    await expect(page.getByText("Remove stored image payloads?")).toBeVisible();

    await page.getByRole("button", { name: "Cancel" }).click();
    await expect(page.getByText("Remove stored image payloads?")).toHaveCount(0);
    await expect(page.getByRole("button", { name: "Remove image payloads" })).toBeVisible();
  });

  test("confirm sends confirmed:true and shows completion", async ({ page }) => {
    let confirmedValue: boolean | undefined;
    await page.route("**/api/v1/data/strip-images/preview", async (route) => {
      await route.fulfill({ json: previewPayload });
    });
    await page.route("**/api/v1/data/strip-images", async (route) => {
      const body = route.request().postDataJSON() as { confirmed?: boolean };
      confirmedValue = body?.confirmed;
      await route.fulfill({ json: applyPayload });
    });

    await openSettingsToolImages(page);
    await page.getByRole("button", { name: "Preview" }).click();
    await expect(page.getByText("my-project")).toBeVisible();

    await page.getByRole("button", { name: "Remove image payloads" }).click();
    await expect(page.getByText("Remove stored image payloads?")).toBeVisible();

    await page.getByRole("button", { name: "Remove", exact: true }).click();
    await expect(page.getByText("Image removal completed.")).toBeVisible();

    expect(confirmedValue).toBe(true);
  });

  test("screenshots at 1280, 768, and 400 widths show no overflow", async ({ page }, testInfo) => {
    await mockStripImageRoutes(page);
    await openSettingsToolImages(page);
    await page.getByRole("button", { name: "Preview" }).click();
    await expect(page.getByText("my-project")).toBeVisible();

    for (const width of [1280, 768, 400]) {
      await page.setViewportSize({ width, height: 800 });
      await page.waitForTimeout(100);

      const noOverflow = await page.evaluate(
        () => document.documentElement.scrollWidth <= window.innerWidth,
      );
      expect(noOverflow, `overflow at width=${width}`).toBe(true);

      await page.screenshot({
        path: testInfo.outputPath(`settings-image-cleanup-${width}.png`),
      });
    }
  });
});
