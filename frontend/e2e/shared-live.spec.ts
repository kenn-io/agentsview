/// <reference types="node" />
import http from "node:http";
import { expect, test } from "@playwright/test";

for (const fallback of ["worker", "broadcast"] as const) {
  test(`several visible session tabs share live updates (fallback: ${fallback})`, async ({
    context,
    page,
  }) => {
    const active = new Set<http.ServerResponse>();
    const requested: string[] = [];
    const watches: string[] = [];
    const workerStatuses: number[] = [];
    let realAuthorization: string | undefined;
    const streamAuthorizations: (string | undefined)[] = [];
    const proxy = http.createServer((req, res) => {
      const url = req.url ?? "/";
      const authorization = req.headers.authorization;
      if (authorization && authorization !== "Bearer rotated-token")
        realAuthorization = authorization;
      if (/^\/api\/v1\/sessions\/[^/]+\/watch(?:\?|$)/.test(url)) watches.push(url);
      if (url.startsWith("/api/v1/events")) {
        active.add(res);
        requested.push(url);
        streamAuthorizations.push(authorization);
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
            ...(authorization === "Bearer rotated-token"
              ? { authorization: realAuthorization }
              : {}),
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
    await context.addInitScript((disableWorker) => {
      if (disableWorker !== "worker")
        Object.defineProperty(window, "SharedWorker", { value: undefined });
      Object.defineProperty(document, "hidden", {
        configurable: true,
        get: () => (window as Window & { __hidden?: boolean }).__hidden ?? false,
      });
      Object.defineProperty(document, "visibilityState", {
        configurable: true,
        get: () => ((window as Window & { __hidden?: boolean }).__hidden ? "hidden" : "visible"),
      });
    }, fallback);
    try {
      let messageRequests = 0;
      page.on("request", (request) => {
        if (request.url().includes("/api/v1/sessions/test-session-small-5/messages"))
          messageRequests++;
      });
      const pages = [page, await context.newPage(), await context.newPage()];
      const ids = ["test-session-small-5", "test-session-small-2", "test-session-medium-8"];
      for (const tab of pages) {
        await tab.goto(`${base}/sessions/${ids[0]}`);
        await expect(tab).toHaveURL(new RegExp(ids[0]!));
      }
      await page.waitForTimeout(400);
      await expect.poll(() => active.size).toBe(1);
      expect(workerStatuses).toEqual(fallback === "worker" ? [200] : []);
      expect(requested[0]).toBe("/api/v1/events?session=test-session-small-5");
      expect(new Set(new URL(requested.at(-1)!, base).searchParams.getAll("session"))).toEqual(
        new Set([ids[0]]),
      );
      const fourth = await context.newPage();
      await fourth.goto(`${base}/`, { timeout: 8000 });
      await fourth.keyboard.press("Control+g");
      const dialog = fourth.getByRole("dialog", { name: "Go to session" });
      await expect(dialog).toBeVisible();
      await fourth
        .getByRole("textbox", { name: "Session ID or UUID" })
        .fill("test-session-small-5");
      await fourth.keyboard.press("Enter");
      await expect(fourth).toHaveURL(/test-session-small-5/);
      await fourth.evaluate(() => localStorage.setItem("agentsview-auth-token", "rotated-token"));
      await expect.poll(() => streamAuthorizations.at(-1)).toBe("Bearer rotated-token");
      expect(page.isClosed()).toBe(false);
      await page.waitForTimeout(400);
      await expect.poll(() => active.size).toBe(1);
      await expect.poll(() => messageRequests).toBeGreaterThan(0);
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
      await page.waitForTimeout(400);
      await expect.poll(() => active.size).toBe(1);
      const count = requested.length;
      const beforeSecondReturn = messageRequests;
      await page.evaluate(() => {
        (window as Window & { __hidden?: boolean }).__hidden = true;
        document.dispatchEvent(new Event("visibilitychange"));
        (window as Window & { __hidden?: boolean }).__hidden = false;
        document.dispatchEvent(new Event("visibilitychange"));
      });
      await expect.poll(() => messageRequests).toBeGreaterThan(beforeSecondReturn);
      await page.waitForTimeout(400);
      expect(requested.length).toBe(count);
      for (const index of [1, 2]) {
        await pages[index]!.goto(`${base}/sessions/${ids[index]}`);
        await expect(pages[index]!).toHaveURL(new RegExp(ids[index]!));
      }
      await expect
        .poll(() => new Set(new URL(requested.at(-1)!, base).searchParams.getAll("session")))
        .toEqual(new Set(ids));
      const beforeLeaderClose = requested.length;
      await page.close();
      if (fallback !== "worker")
        await expect.poll(() => requested.length).toBeGreaterThan(beforeLeaderClose);
      await expect.poll(() => active.size).toBe(1);
      await expect
        .poll(() => new Set(new URL(requested.at(-1)!, base).searchParams.getAll("session")))
        .toEqual(new Set(ids));
      expect(watches).toEqual([]);
    } finally {
      await context.close();
      await new Promise<void>((resolve, reject) =>
        proxy.close((err) => (err ? reject(err) : resolve())),
      );
    }
  });
}
