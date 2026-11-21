/// <reference types="node" />
import http from "node:http";
import { expect, test } from "@playwright/test";

for (const apiVersion of [27, 28]) {
  test(`saved remote connection with API ${apiVersion}`, async ({ context, page }) => {
    const streams: string[] = [];
    let messageRequests = 0;
    const serverPort = Number(process.env.AGENTSVIEW_E2E_PORT ?? 8090);
    const proxy = http.createServer((req, res) => {
      const path = req.url ?? "/";
      if (path.startsWith("/api/v1/events") || /\/sessions\/[^/]+\/watch/.test(path))
        streams.push(path);
      if (path.includes("/api/v1/sessions/test-session-small-5/messages")) messageRequests++;
      const upstream = http.request(
        {
          hostname: "127.0.0.1",
          port: serverPort,
          path,
          method: req.method,
          headers: {
            ...req.headers,
            host: `127.0.0.1:${serverPort}`,
            ...(req.headers.origin ? { origin: `http://127.0.0.1:${serverPort}` } : {}),
          },
        },
        (response) => {
          if (path === "/api/v1/version") {
            let body = "";
            response.setEncoding("utf8");
            response.on("data", (chunk) => {
              body += chunk;
            });
            response.on("end", () => {
              const data = JSON.stringify({ ...JSON.parse(body), version: "0.46.0", api_version: apiVersion });
              const headers = { ...response.headers, "content-length": Buffer.byteLength(data) };
              res.writeHead(response.statusCode ?? 500, headers);
              res.end(data);
            });
            return;
          }
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
    await page.addInitScript(() => {
      localStorage.setItem("agentsview-server-url", window.location.origin);
      Object.defineProperty(document, "hidden", {
        configurable: true,
        get: () => (window as Window & { __hidden?: boolean }).__hidden ?? false,
      });
    });
    try {
      await page.goto(`${base}/sessions/test-session-small-5`);
      if (apiVersion === 27) {
        await expect(
          page.getByText("Remote server: AgentsView 0.46.0 (API 27). Upgrade to API 28 or later."),
        ).toBeVisible();
        await expect(page.getByRole("button", { name: "Retry", exact: true })).toBeVisible();
        await page.waitForTimeout(750);
        expect(streams).toEqual([]);
      } else {
        await expect(
          page.getByText("Remote server: AgentsView 0.46.0 (API 27). Upgrade to API 28 or later."),
        ).toHaveCount(0);
        await expect
          .poll(() =>
            streams.some((url) => url.includes("/api/v1/events?session=test-session-small-5")),
          )
          .toBe(true);
        await expect.poll(() => messageRequests).toBeGreaterThan(0);
        const beforeReturn = messageRequests;
        await page.evaluate(() => {
          (window as Window & { __hidden?: boolean }).__hidden = true;
          document.dispatchEvent(new Event("visibilitychange"));
          (window as Window & { __hidden?: boolean }).__hidden = false;
          document.dispatchEvent(new Event("visibilitychange"));
        });
        await expect.poll(() => messageRequests).toBeGreaterThan(beforeReturn);
      }
    } finally {
      await context.close();
      await new Promise<void>((resolve, reject) =>
        proxy.close((error) => (error ? reject(error) : resolve())),
      );
    }
  });
}
