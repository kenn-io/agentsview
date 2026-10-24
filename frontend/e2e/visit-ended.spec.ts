import { test, expect } from "@playwright/test";
import { SessionsPage } from "./pages/sessions-page";
import { TOKEN, visitServer } from "./helpers/visit-server";

test("the daemon's disabled receipt stops all browser telemetry through a reload", async ({
  page,
}) => {
  const events: string[] = [];
  page.on("request", (request) => {
    if (
      request.method() === "POST" &&
      new URL(request.url()).pathname === "/api/v1/telemetry/events"
    ) {
      events.push(request.postDataJSON().event);
    }
  });
  const receipt = page.waitForResponse(
    (response) =>
      response.request().method() === "POST" &&
      new URL(response.url()).pathname === "/api/v1/telemetry/events",
  );
  await new SessionsPage(page).goto();
  const response = await receipt;
  expect(response.status()).toBe(202);
  expect(await response.json()).toEqual({ status: "disabled" });
  expect(response.request().postDataJSON()).toEqual({
    event: "app_opened",
    properties: { surface: "web" },
  });
  await page.evaluate(() => {
    window.dispatchEvent(new Event("pagehide"));
    window.dispatchEvent(new Event("pageshow"));
    window.dispatchEvent(new Event("focus"));
  });
  await page.reload();
  expect(events).toEqual(["app_opened"]);
});

for (const { remote, action } of [
  { remote: false, action: "close" },
  { remote: true, action: "reload" },
]) {
  test(`visit delivery reaches the ${remote ? "authenticated remote" : "local"} endpoint on ${action}`, async ({
    page,
    baseURL,
  }) => {
    const server = await visitServer(baseURL!, remote);
    try {
      const enabled = page.waitForResponse(
        (response) =>
          response.request().method() === "POST" &&
          new URL(response.url()).pathname === "/api/v1/telemetry/events" &&
          response.request().postDataJSON()?.event === "app_opened",
      );
      if (remote) {
        await page.addInitScript(
          ({ url, token }) => {
            localStorage.setItem("agentsview-server-url", url);
            localStorage.setItem(`agentsview-auth-token::${url}`, token);
          },
          { url: server.url, token: TOKEN },
        );
        await new SessionsPage(page).goto();
      } else {
        await page.goto(server.url);
        await expect(new SessionsPage(page).sessionItems.first()).toBeVisible();
      }
      await enabled;
      server.preflights.length = 0;
      if (action === "close") await page.close();
      else await page.reload();
      await expect
        .poll(() => server.visits.map(({ payload }) => payload))
        .toEqual([
          {
            event: "visit_ended",
            properties: { surface: "web", duration_bucket: "under_1m" },
          },
        ]);
      if (remote) {
        expect(server.visits[0]?.authorization).toBe(`Bearer ${TOKEN}`);
        expect(server.preflights).toContainEqual(
          expect.stringMatching(/authorization.*content-type/i),
        );
      }
    } finally {
      await server.close();
    }
  });
}
