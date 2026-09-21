import { expect, test, type Page, type Response } from "@playwright/test";
import * as path from "node:path";
import * as fs from "node:fs";

async function openSettledSettings(page: Page) {
  const responses: Response[] = [];
  const settingsLoaded = new Promise<void>((resolve) => {
    const onResponse = (response: Response) => {
      if (new URL(response.url()).pathname !== "/api/v1/settings") return;
      responses.push(response);
      if (responses.length < 2) return;
      page.off("response", onResponse);
      void Promise.all(responses.map((item) => item.finished())).then(() => {
        resolve();
      });
    };
    page.on("response", onResponse);
  });

  await page.goto("/settings");
  await settingsLoaded;
  await page.evaluate(
    () =>
      new Promise<void>((resolve) => {
        requestAnimationFrame(() => requestAnimationFrame(() => resolve()));
      }),
  );
  await expect(page.locator(".settings-loading")).toHaveCount(0);
}

async function openArchiveContent(page: Page) {
  await openSettledSettings(page);
  const nav = page.getByRole("navigation", { name: "Settings" });
  await nav.locator("button", { hasText: "Archive content" }).click();
  await expect(page.getByRole("heading", { name: "Archive content" })).toBeVisible();
}

// The chromium and webkit projects share one daemon and run this file
// concurrently, so a real write from either would race the other's assertions.
// Every test that does not measure persistence fulfils the PUT itself.
async function stubSettingsWrite(page: Page) {
  await page.route("**/api/v1/settings", async (route) => {
    if (route.request().method() !== "PUT") return route.fallback();
    const patch = JSON.parse(route.request().postData() ?? "{}");
    const current = await (await page.request.get("/api/v1/settings")).json();
    await route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify({ ...current, ...patch }),
    });
  });
}

test.describe("Settings archive content", () => {
  test("renders Keep checked and no restart notice on load", async ({ page }) => {
    await openArchiveContent(page);

    const keepRadio = page.getByRole("radio", { name: "Keep" });
    const dropRadio = page.getByRole("radio", { name: "Drop" });
    await expect(keepRadio).toHaveAttribute("aria-checked", "true");
    await expect(dropRadio).toHaveAttribute("aria-checked", "false");
    await expect(page.getByRole("radio", { name: "Offload" })).toHaveAttribute(
      "aria-checked",
      "false",
    );
    await expect(page.locator(".archive-content-settings").getByRole("status")).toHaveCount(0);
  });

  test("selecting Drop issues PUT and shows restart notice", async ({ page }) => {
    await stubSettingsWrite(page);
    await openArchiveContent(page);

    // Only the outbound request is asserted here. The response is the stub's
    // own echo, so the server's answer is checked in the persistence test.
    let putBody: string | null = null;
    page.on("request", (req) => {
      if (req.method() === "PUT" && new URL(req.url()).pathname === "/api/v1/settings") {
        putBody = req.postData();
      }
    });

    const dropRadio = page.getByRole("radio", { name: "Drop" });
    await dropRadio.click();

    await expect.poll(() => putBody).not.toBeNull();
    expect(JSON.parse(putBody!)).toMatchObject({ tool_result_images: "drop" });

    await expect(page.getByRole("radio", { name: "Drop" })).toHaveAttribute("aria-checked", "true");
    await expect(page.locator(".archive-content-settings").getByRole("status")).toBeVisible();
  });

  test.describe("persistence", () => {
    // The only test here that must reach the real daemon. It runs on one
    // project so the two projects cannot write the shared archive at once, and
    // restores the default even when an assertion above it fails.
    test.skip(({ browserName }) => browserName !== "chromium", "writes shared daemon state");

    test.afterEach(async ({ request, baseURL }) => {
      const restored = await request.put(`${baseURL}/api/v1/settings`, {
        headers: { Origin: baseURL! },
        data: { tool_result_images: "keep" },
      });
      // A silent restore failure would leave the daemon on offload and turn the
      // retry into a pass that asserts nothing.
      expect(restored.ok()).toBe(true);
    });

    test("Offload selection persists after page reload", async ({ page }) => {
      await openArchiveContent(page);

      // Fails loudly if a prior restore left the daemon on offload, because
      // clicking an already-selected option issues no PUT at all.
      await expect(page.getByRole("radio", { name: "Keep" })).toHaveAttribute(
        "aria-checked",
        "true",
      );

      const putEcho = page.waitForResponse(
        (response) =>
          response.request().method() === "PUT" &&
          new URL(response.url()).pathname === "/api/v1/settings",
      );
      await page.getByRole("radio", { name: "Offload" }).click();
      expect(await (await putEcho).json()).toMatchObject({ tool_result_images: "offload" });
      await expect(page.getByRole("radio", { name: "Offload" })).toHaveAttribute(
        "aria-checked",
        "true",
      );

      await openSettledSettings(page);
      const nav = page.getByRole("navigation", { name: "Settings" });
      await nav.locator("button", { hasText: "Archive content" }).click();

      await expect(page.getByRole("radio", { name: "Offload" })).toHaveAttribute(
        "aria-checked",
        "true",
      );
    });
  });

  for (const width of [1280, 768, 400]) {
    test(`no horizontal overflow at ${width}px`, async ({ page }, testInfo) => {
      await page.setViewportSize({ width, height: 800 });
      await openArchiveContent(page);

      const overflow = await page.evaluate(
        () => document.documentElement.scrollWidth <= innerWidth,
      );
      expect(overflow).toBe(true);

      const screenshotDir = path.join(
        testInfo.project.outputDir ?? "test-results",
        "captures",
        "1622-6",
      );
      fs.mkdirSync(screenshotDir, { recursive: true });
      await page.screenshot({
        path: path.join(screenshotDir, `layout-${width}.png`),
        fullPage: false,
      });
    });
  }
});
