import { expect, test } from "@playwright/test";
import { RuntimeErrorMonitor } from "./helpers/runtime-error-monitor";

test.describe("Runtime error diagnostics", () => {
  test("adds a source location to console errors", async ({ page }) => {
    const monitor = new RuntimeErrorMonitor(page);
    await page.route("**/diagnostic-console-error.js", async (route) => {
      await route.fulfill({
        contentType: "application/javascript",
        body: 'console.error("diagnostic console failure");',
      });
    });
    await page.goto("/");

    await page.evaluate(async () => {
      const script = document.createElement("script");
      script.src = "/diagnostic-console-error.js";
      const loaded = new Promise<void>((resolve, reject) => {
        script.addEventListener("load", () => resolve());
        script.addEventListener("error", () =>
          reject(new Error("diagnostic script failed to load")),
        );
      });
      document.head.append(script);
      await loaded;
    });

    await expect
      .poll(() =>
        monitor
          .all()
          .some((message) =>
            /^diagnostic console failure \(\/diagnostic-console-error\.js:\d+:\d+\)$/.test(
              message,
            ),
          ),
      )
      .toBe(true);
  });

  test("ignores browser resource errors from external origins", async ({
    page,
  }) => {
    const monitor = new RuntimeErrorMonitor(page);
    await page.route(
      "https://fonts.gstatic.com/diagnostic-font.woff2",
      async (route) => {
        await route.fulfill({ status: 404, body: "missing" });
      },
    );
    await page.goto("/");

    await page.evaluate(async () => {
      const style = document.createElement("style");
      style.textContent = `
        @font-face {
          font-family: "Diagnostic Font";
          src: url("https://fonts.gstatic.com/diagnostic-font.woff2") format("woff2");
        }
      `;
      document.head.append(style);
      await document.fonts
        .load('16px "Diagnostic Font"')
        .catch(() => undefined);
    });

    expect(monitor.all()).toEqual([]);
  });

  test("identifies a failed HTTP response", async ({ page }) => {
    const monitor = new RuntimeErrorMonitor(page);
    await page.route("**/diagnostic-http-failure*", async (route) => {
      await route.fulfill({ status: 404, body: "missing" });
    });
    await page.goto("/");

    const status = await page.evaluate(async () => {
      const response = await fetch(
        "/diagnostic-http-failure?token=private-value",
      );
      return response.status;
    });

    expect(status).toBe(404);
    await expect
      .poll(() => monitor.diagnostics())
      .toContain(
        "HTTP 404 GET /diagnostic-http-failure?token=[redacted] [fetch]",
      );
  });

  test("identifies a request without an HTTP response", async ({ page }) => {
    const monitor = new RuntimeErrorMonitor(page);
    await page.route("**/diagnostic-network-failure", async (route) => {
      await route.abort("failed");
    });
    await page.goto("/");

    await page.evaluate(async () => {
      await fetch("/diagnostic-network-failure").catch(() => undefined);
    });

    await expect
      .poll(() =>
        monitor
          .diagnostics()
          .some((message) =>
            /^REQUEST FAILED GET \/diagnostic-network-failure \[fetch\]: .+/.test(
              message,
            ),
          ),
      )
      .toBe(true);
  });
});
