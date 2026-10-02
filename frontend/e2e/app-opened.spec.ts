import { test, expect, type Request } from "@playwright/test";
import { SessionsPage } from "./pages/sessions-page";

const TELEMETRY_PATH = "/api/v1/telemetry/events";

function isTelemetryPost(request: Request): boolean {
  return request.method() === "POST" && new URL(request.url()).pathname === TELEMETRY_PATH;
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
  expect(res.request().postDataJSON()).toEqual({ event: "app_opened" });

  await page.evaluate(() => window.dispatchEvent(new Event("focus")));
  // Absence can't be awaited, so give a stray request time to show up.
  await page.waitForTimeout(1000);
  expect(recorded).toHaveLength(1);
});
