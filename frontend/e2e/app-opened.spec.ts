import { test, expect, type Request, type Route } from "@playwright/test";
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
            Object.keys(localStorage).filter((key) =>
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

test.describe("a closed tab's pending visit", () => {
  for (const status of ["queued", "disabled"] as const) {
    test(`two reopened tabs claim it once after the ${status} receipt`, async ({
      page,
      context,
    }) => {
      const checks: Route[] = [];
      const visits: unknown[] = [];
      await context.route(`**${TELEMETRY_PATH}`, async (route) => {
        const event = route.request().postDataJSON();
        if (event.event === "app_opened") {
          checks.push(route);
          return;
        }
        if (event.event === "visit_ended") visits.push(event);
        await route.fulfill({ status: 202, json: { status } });
      });
      await new SessionsPage(page).goto();
      await expect.poll(() => checks.length).toBe(1);
      await page.close();

      const second = await context.newPage();
      const third = await context.newPage();
      await Promise.all([new SessionsPage(second).goto(), new SessionsPage(third).goto()]);
      await expect.poll(() => checks.length).toBe(3);
      expect(
        await second.evaluate(() =>
          Object.keys(localStorage).filter((key) =>
            key.startsWith("agentsview.telemetry.pending-visit:"),
          ),
        ),
      ).toHaveLength(1);
      expect(visits).toEqual([]);

      if (status === "queued") {
        // Hold the real browser lock until both tabs try to claim the visit.
        await second.evaluate(
          () =>
            new Promise<void>((locked) => {
              const key = Object.keys(localStorage).find((key) =>
                key.startsWith("agentsview.telemetry.pending-visit:"),
              )!;
              const lockName = key.slice(0, key.lastIndexOf(":") + 1);
              void navigator.locks.request(
                lockName,
                () =>
                  new Promise<void>((release) => {
                    (window as typeof window & { releaseVisitLock: () => void }).releaseVisitLock =
                      release;
                    locked();
                  }),
              );
            }),
        );
      }

      await Promise.all(
        checks.slice(1).map((route) => route.fulfill({ status: 202, json: { status } })),
      );
      if (status === "queued") {
        await expect
          .poll(() =>
            second.evaluate(
              async () =>
                (await navigator.locks.query()).pending?.filter((lock) =>
                  lock.name?.startsWith("agentsview.telemetry.pending-visit:"),
                ).length,
            ),
          )
          .toBe(2);
        expect(visits).toEqual([]);
        await second.evaluate(() =>
          (window as typeof window & { releaseVisitLock: () => void }).releaseVisitLock(),
        );
        await expect
          .poll(() => visits)
          .toEqual([
            { event: "visit_ended", properties: { surface: "web", duration_bucket: "under_1m" } },
          ]);
      }
      await expect
        .poll(() =>
          second.evaluate(() =>
            Object.keys(localStorage).filter((key) =>
              key.startsWith("agentsview.telemetry.pending-visit:"),
            ),
          ),
        )
        .toEqual([]);
      await second.waitForTimeout(1000);
      expect(visits).toHaveLength(status === "queued" ? 1 : 0);
    });
  }
});
