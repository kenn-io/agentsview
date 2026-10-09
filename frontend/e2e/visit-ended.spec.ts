import { createServer } from "node:http";
import { test, expect } from "@playwright/test";
import { SessionsPage } from "./pages/sessions-page";

const SERVER_KEY = "agentsview-server-url";
const REVISION_KEY = "agentsview-server-url-revision";
const TOKEN = "visit-test-token";

async function visitServer(upstream: string, authenticated = false) {
  const visits: {
    payload: unknown;
    authorization: string | undefined;
    status: number;
    body: unknown;
  }[] = [];
  const preflights: string[] = [];
  const server = createServer(async (req, res) => {
    res.setHeader("Access-Control-Allow-Origin", req.headers.origin ?? "*");
    res.setHeader("Access-Control-Allow-Headers", "Authorization, Content-Type");
    res.setHeader("Access-Control-Allow-Methods", "GET, POST, OPTIONS");
    res.setHeader("Access-Control-Max-Age", "0");
    if (req.method === "OPTIONS") {
      if (req.url === "/api/v1/telemetry/events")
        preflights.push(req.headers["access-control-request-headers"] as string);
      res.writeHead(204).end();
      return;
    }
    if (req.url === "/away") {
      res.setHeader("Content-Type", "text/html");
      res.end("<!doctype html><title>Away</title>");
      return;
    }
    // A finite event response lets the browser cache the page between visits.
    if (req.url === "/api/v1/events") {
      res.writeHead(204).end();
      return;
    }
    if (authenticated && req.headers.authorization !== `Bearer ${TOKEN}`) {
      res.writeHead(401).end();
      return;
    }
    try {
      const chunks: Buffer[] = [];
      for await (const chunk of req) chunks.push(Buffer.from(chunk));
      const body = Buffer.concat(chunks);
      const response = await fetch(new URL(req.url!, upstream), {
        method: req.method,
        headers: {
          Origin: new URL(upstream).origin,
          ...(body.length ? { "Content-Type": "application/json" } : {}),
        },
        body: body.length ? body : undefined,
      });
      const bytes = Buffer.from(await response.arrayBuffer());
      const payload = body.length ? JSON.parse(body.toString()) : undefined;
      if (req.url === "/api/v1/telemetry/events" && payload?.event === "visit_ended") {
        visits.push({
          payload,
          authorization: req.headers.authorization,
          status: response.status,
          body: JSON.parse(bytes.toString()),
        });
      }
      res.setHeader(
        "Content-Type",
        response.headers.get("Content-Type") ?? "application/octet-stream",
      );
      res.writeHead(response.status).end(bytes);
    } catch (error) {
      res.writeHead(502).end(String(error));
    }
  });
  await new Promise<void>((resolve) => server.listen(0, "127.0.0.1", resolve));
  const address = server.address();
  if (!address || typeof address === "string") throw new Error("Missing visit server address");
  return {
    url: `http://127.0.0.1:${address.port}`,
    visits,
    preflights,
    close: async () => {
      server.closeAllConnections();
      await new Promise<void>((resolve, reject) =>
        server.close((error) => (error ? reject(error) : resolve())),
      );
    },
  };
}

for (const remote of [false, true]) {
  for (const action of ["close", "reload"] as const) {
    test(`visit delivery reaches the ${remote ? "authenticated remote" : "local"} endpoint on ${action}`, async ({
      page,
      baseURL,
    }) => {
      const server = await visitServer(baseURL!, remote);
      try {
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
        server.preflights.length = 0;
        if (action === "close") await page.close();
        else await page.reload();
        await expect
          .poll(() => server.visits.map(({ payload, status, body }) => ({ payload, status, body })))
          .toEqual([
            {
              payload: {
                event: "visit_ended",
                properties: { surface: "web", duration_bucket: "under_1m" },
              },
              status: 202,
              body: { status: "disabled" },
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
}

test.describe("Back restoration", () => {
  test("Back restoration keeps the new visit after queued server changes", async ({
    playwright,
    baseURL,
    browserName,
  }) => {
    test.skip(
      browserName !== "chromium",
      "This regression exercises Chromium's back/forward cache.",
    );
    const server = await visitServer(baseURL!);
    const browser = await playwright.chromium.launch({
      channel: "chromium",
      ignoreDefaultArgs: ["--disable-back-forward-cache"],
    });
    const context = await browser.newContext({ viewport: { width: 1600, height: 900 } });
    const page = await context.newPage();
    const otherTab = await context.newPage();
    await page.bringToFront();
    try {
      await page.addInitScript(() => {
        let offset = 0;
        const now = performance.now.bind(performance);
        performance.now = () => now() + offset;
        Object.assign(window, {
          advanceVisit: (ms: number) => {
            offset += ms;
          },
          restored: false,
          serverEvents: [],
        });
        window.addEventListener("pageshow", (event) =>
          Object.assign(window, { restored: event.persisted }),
        );
        window.addEventListener("storage", (event) => {
          if (event.key === "agentsview-server-url")
            (window as unknown as { serverEvents: unknown[] }).serverEvents.push(event.newValue);
        });
      });
      await page.goto(server.url);
      await expect(new SessionsPage(page).sessionItems.first()).toBeVisible();
      await page.evaluate(() =>
        (window as unknown as { advanceVisit(ms: number): void }).advanceVisit(120_000),
      );
      await page.goto(`${server.url}/away`);
      await expect.poll(() => server.visits.length).toBe(1);
      await otherTab.goto(`${server.url}/away`);
      await otherTab.evaluate(
        ({ serverKey, revisionKey }) => {
          localStorage.setItem(serverKey, "https://example.com/remote");
          localStorage.setItem(revisionKey, "away");
          localStorage.removeItem(serverKey);
          localStorage.setItem(revisionKey, "back");
        },
        { serverKey: SERVER_KEY, revisionKey: REVISION_KEY },
      );
      await page.goBack({ waitUntil: "commit" });
      await expect
        .poll(() => page.evaluate(() => (window as unknown as { restored: boolean }).restored))
        .toBe(true);
      await expect
        .poll(() =>
          page.evaluate(() => (window as unknown as { serverEvents: unknown[] }).serverEvents),
        )
        .toEqual(["https://example.com/remote", null]);
      await page.evaluate(() =>
        (window as unknown as { advanceVisit(ms: number): void }).advanceVisit(30_000),
      );
      await page.close();
      await expect
        .poll(() => server.visits.map((visit) => visit.payload))
        .toEqual([
          { event: "visit_ended", properties: { surface: "web", duration_bucket: "1_to_5m" } },
          { event: "visit_ended", properties: { surface: "web", duration_bucket: "under_1m" } },
        ]);
    } finally {
      await otherTab.close();
      await browser.close();
      await server.close();
    }
  });
});
