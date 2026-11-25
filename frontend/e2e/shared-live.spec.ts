/// <reference types="node" />
import http from "node:http";
import { expect, test } from "@playwright/test";

for (const fallback of ["worker", "broadcast"] as const) {
  test(`several visible session tabs share live updates (fallback: ${fallback})`, async ({
    context,
    page,
  }) => {
    const active = new Map<http.ServerResponse, string>();
    const requested: string[] = [];
    const watches: string[] = [];
    const workerStatuses: number[] = [];
    const proxy = http.createServer((req, res) => {
      const url = req.url ?? "/";
      if (/^\/api\/v1\/sessions\/[^/]+\/watch(?:\?|$)/.test(url)) watches.push(url);
      if (url.startsWith("/api/v1/events")) {
        active.set(res, url);
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
    const singleActiveSessions = () => {
      if (active.size !== 1) return null;
      const [url] = active.values();
      return new Set(new URL(url!, base).searchParams.getAll("session"));
    };
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
      const pages = [page, await context.newPage(), await context.newPage()];
      const ids = ["test-session-small-5", "test-session-small-2", "test-session-medium-8"];
      for (const tab of pages) {
        await tab.goto(`${base}/sessions/${ids[0]}`);
        await expect(tab).toHaveURL(new RegExp(ids[0]!));
      }
      await expect.poll(singleActiveSessions).toEqual(new Set([ids[0]]));
      if (fallback === "worker") await expect.poll(() => workerStatuses).toEqual([200]);
      expect(requested[0]).toBe("/api/v1/events?session=test-session-small-5");
      const fourth = await context.newPage();
      await fourth.goto(`${base}/`, { timeout: 8000 });
      await fourth.keyboard.press("Control+g");
      const dialog = fourth.getByRole("dialog", { name: "Go to session" });
      await expect(dialog).toBeVisible();
      const input = dialog.getByRole("textbox", { name: "Session ID or UUID" });
      await input.fill("test-session-small-5");
      await input.press("Enter");
      await expect(fourth).toHaveURL(/test-session-small-5/);
      await expect.poll(() => active.size).toBe(1);
      await page.evaluate(() => {
        (window as Window & { __hidden?: boolean }).__hidden = true;
        document.dispatchEvent(new Event("visibilitychange"));
      });
      await page.evaluate(() => {
        (window as Window & { __hidden?: boolean }).__hidden = false;
        document.dispatchEvent(new Event("visibilitychange"));
      });
      await expect.poll(() => active.size).toBe(1);
      const sessionRequests = [0, 0];
      let beforeThird = 0;
      for (const index of [1, 2]) {
        pages[index]!.on("request", (request) => {
          if (request.url().includes(`/api/v1/sessions/${ids[index]}/messages`))
            sessionRequests[index - 1]!++;
        });
        await pages[index]!.goto(`${base}/sessions/${ids[index]}`);
        await expect(pages[index]!).toHaveURL(new RegExp(ids[index]!));
        if (index === 1) {
          await expect.poll(singleActiveSessions).toEqual(new Set([ids[0], ids[1]]));
          await expect.poll(() => sessionRequests[0]).toBeGreaterThan(0);
          beforeThird = sessionRequests[0]!;
        }
        if (index === 2) {
          await expect.poll(singleActiveSessions).toEqual(new Set(ids));
          expect(sessionRequests[0]).toBe(beforeThird);
        }
      }
      await expect.poll(singleActiveSessions).toEqual(new Set(ids));
      await expect.poll(() => sessionRequests.every((count) => count > 0)).toBe(true);
      for (const index of [1, 2]) {
        const beforeUpdate = sessionRequests[index - 1]!;
        const otherBefore = sessionRequests[2 - index]!;
        for (const response of active.keys())
          response.write(`event: session_updated\ndata: ${ids[index]}\n\n`);
        await expect.poll(() => sessionRequests[index - 1]).toBeGreaterThan(beforeUpdate);
        await page.waitForTimeout(250);
        expect(sessionRequests[2 - index]).toBe(otherBefore);
      }
      const beforeLeaderClose = requested.length;
      await page.close();
      if (fallback !== "worker")
        await expect.poll(() => requested.length).toBeGreaterThan(beforeLeaderClose);
      await expect.poll(singleActiveSessions).toEqual(new Set(ids));
      expect(watches).toEqual([]);
    } finally {
      await context.close();
      await new Promise<void>((resolve, reject) =>
        proxy.close((err) => (err ? reject(err) : resolve())),
      );
    }
  });
}
