/// <reference types="node" />
import http from "node:http";
import { expect, test } from "@playwright/test";

test("several visible session tabs share live updates", async ({ context, page }) => {
  const active = new Set<http.ServerResponse>();
  const requested: string[] = [];
  const watches: string[] = [];
  const workerStatuses: number[] = [];
  const proxy = http.createServer((req, res) => {
    const url = req.url ?? "/";
    if (/^\/api\/v1\/sessions\/[^/]+\/watch(?:\?|$)/.test(url)) watches.push(url);
    if (url.startsWith("/api/v1/events")) {
      active.add(res);
      requested.push(url);
      res.on("close", () => active.delete(res));
    }
    const upstream = http.request(
      {
        hostname: "127.0.0.1",
        port: Number(process.env.AGENTSVIEW_E2E_PORT ?? 8090),
        path: url,
        method: req.method,
        headers: {
          ...req.headers,
          host: `127.0.0.1:${process.env.AGENTSVIEW_E2E_PORT ?? 8090}`,
          ...(req.headers.origin
            ? { origin: `http://127.0.0.1:${process.env.AGENTSVIEW_E2E_PORT ?? 8090}` }
            : {}),
        },
      },
      (response) => {
        if (url.includes("/live.worker-")) workerStatuses.push(response.statusCode ?? 0);
        res.writeHead(response.statusCode ?? 500, response.headers);
        response.pipe(res);
      },
    );
    upstream.on("error", () => {
      if (!res.destroyed) res.destroy();
    });
    res.on("close", () => upstream.destroy());
    req.pipe(upstream);
  });
  await new Promise<void>((resolve) => proxy.listen(0, "127.0.0.1", resolve));
  const address = proxy.address();
  if (!address || typeof address === "string") throw new Error("proxy did not bind");
  const base = `http://127.0.0.1:${address.port}`;
  await context.addInitScript(() => {
    Object.defineProperty(document, "hidden", {
      configurable: true,
      get: () => (window as Window & { __hidden?: boolean }).__hidden ?? false,
    });
    Object.defineProperty(document, "visibilityState", {
      configurable: true,
      get: () => ((window as Window & { __hidden?: boolean }).__hidden ? "hidden" : "visible"),
    });
  });
  try {
    let messageRequests = 0;
    page.on("request", (request) => {
      if (request.url().includes("/api/v1/sessions/test-session-small-5/messages"))
        messageRequests++;
    });
    const pages = [page, await context.newPage(), await context.newPage()];
    for (const tab of pages) {
      await tab.goto(`${base}/sessions/test-session-small-5`);
      await expect(tab).toHaveURL(/test-session-small-5/);
    }
    await page.waitForTimeout(400);
    await expect.poll(() => active.size).toBe(1);
    expect(workerStatuses).toEqual([200]);
    expect(requested.at(-1)).toBe("/api/v1/events?session=test-session-small-5");
    const fourth = await context.newPage();
    await fourth.goto(`${base}/`, { timeout: 8000 });
    await fourth.keyboard.press("Control+g");
    const dialog = fourth.getByRole("dialog", { name: "Go to session" });
    await expect(dialog).toBeVisible();
    await fourth.getByRole("textbox", { name: "Session ID or UUID" }).fill("test-session-small-5");
    await fourth.keyboard.press("Enter");
    await expect(fourth).toHaveURL(/test-session-small-5/);
    await page.waitForTimeout(400);
    await expect.poll(() => active.size).toBe(1);
    await expect.poll(() => messageRequests).toBeGreaterThan(0);
    const count = requested.length;
    const beforeReturn = messageRequests;
    await page.evaluate(() => {
      (window as Window & { __hidden?: boolean }).__hidden = true;
      document.dispatchEvent(new Event("visibilitychange"));
    });
    await page.evaluate(() => {
      (window as Window & { __hidden?: boolean }).__hidden = false;
      document.dispatchEvent(new Event("visibilitychange"));
    });
    await expect.poll(() => messageRequests).toBeGreaterThan(beforeReturn);
    await expect.poll(() => active.size).toBe(1);
    await expect.poll(() => requested.length).toBeGreaterThan(count);
    await expect.poll(() => active.size).toBe(1);
    expect(watches).toEqual([]);
    console.log(
      `SharedWorker: ${workerStatuses[0]}, live streams: ${active.size}, session watch streams: ${watches.length}, startup: ${requested[0]}`,
    );
  } finally {
    await context.close();
    await new Promise<void>((resolve, reject) =>
      proxy.close((err) => (err ? reject(err) : resolve())),
    );
  }
});
