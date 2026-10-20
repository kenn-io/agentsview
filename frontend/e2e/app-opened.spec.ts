import { test, expect, type Request } from "@playwright/test";
import { SessionsPage } from "./pages/sessions-page";

const TELEMETRY_PATH = "/api/v1/telemetry/events";

function isTelemetryPost(request: Request): boolean {
  return (
    request.method() === "POST" &&
    new URL(request.url()).pathname === TELEMETRY_PATH &&
    request.postDataJSON()?.event === "app_opened"
  );
}

test("loading the UI reports app_opened once and a same-day focus sends nothing more", async ({
  page,
}) => {
  const recorded: Request[] = [];
  page.on("request", (request) => {
    if (isTelemetryPost(request)) recorded.push(request);
  });

  const response = page.waitForResponse((res) => isTelemetryPost(res.request()));
  await new SessionsPage(page).goto();
  const res = await response;

  expect(res.status()).toBe(202);
  expect(await res.json()).toEqual({ status: "disabled" });
  expect(res.request().postDataJSON()).toEqual({
    event: "app_opened",
    properties: { surface: "web" },
  });

  await page.evaluate(() => {
    window.dispatchEvent(new Event("focus"));
    window.dispatchEvent(new Event("pagehide"));
  });
  await page.reload();
  // Absence can't be awaited, so give a stray request time to show up.
  await page.waitForTimeout(1000);
  expect(recorded).toHaveLength(1);
});

test.describe("a visit ended before the permission receipt", () => {
  for (const status of ["queued", "disabled"] as const) {
    test(`survives a real reload until the ${status} receipt`, async ({ page }) => {
      let opens = 0;
      const visits: unknown[] = [];
      await page.route(`**${TELEMETRY_PATH}`, async (route) => {
        const event = route.request().postDataJSON();
        if (event.event === "visit_ended") visits.push(event);
        if (event.event === "app_opened" && ++opens === 1) return;
        await route.fulfill({ status: 202, json: { status } });
      });
      const firstCheck = page.waitForRequest(isTelemetryPost);
      await new SessionsPage(page).goto();
      await firstCheck;
      expect(visits).toEqual([]);
      await page.reload();
      await expect.poll(() => opens).toBe(2);
      if (status === "queued") {
        await expect
          .poll(() => visits)
          .toEqual([
            { event: "visit_ended", properties: { surface: "web", duration_bucket: "under_1m" } },
          ]);
      }
      await expect
        .poll(() =>
          page.evaluate(() =>
            Object.keys(sessionStorage).filter((key) =>
              key.startsWith("agentsview.telemetry.pending-visit:"),
            ),
          ),
        )
        .toEqual([]);
      await page.evaluate(() => window.dispatchEvent(new Event("focus")));
      // A duplicate or a disabled visit would arrive after the receipt.
      await page.waitForTimeout(1000);
      expect(visits).toHaveLength(status === "queued" ? 1 : 0);
    });
  }
});
