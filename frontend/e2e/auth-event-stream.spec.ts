import { expect, test } from "@playwright/test";
import http from "node:http";
import { AUTH_SERVER_TOKEN, authServerURL, waitForAuthServer } from "./helpers/auth-server";

const authHeaders = { Authorization: `Bearer ${AUTH_SERVER_TOKEN}` };

test("the authenticated server rejects missing and wrong tokens", async ({ request, baseURL }) => {
  const server = authServerURL(baseURL!);
  await waitForAuthServer(server);
  const sessions = `${server}/api/v1/sessions?limit=1`;
  expect((await request.get(sessions)).status()).toBe(401);
  const wrong = { Authorization: `Bearer ${AUTH_SERVER_TOKEN}-wrong` };
  expect((await request.get(sessions, { headers: wrong })).status()).toBe(401);
  expect((await request.get(sessions, { headers: authHeaders })).status()).toBe(200);
});

for (const remote of [true, false]) {
  test(`live updates reach the page over an authenticated ${remote ? "remote" : "same-origin"} event stream`, async ({
    page,
    request,
    baseURL,
  }, testInfo) => {
    const server = authServerURL(baseURL!);
    await waitForAuthServer(server);
    const list = await request.get(`${server}/api/v1/sessions?limit=8`, { headers: authHeaders });
    expect(list.status()).toBe(200);
    const { sessions } = (await list.json()) as { sessions: { id: string }[] };
    // Browser projects share the authenticated server, so each project and
    // mode trashes its own session.
    const slot = (testInfo.project.name === "webkit" ? 2 : 0) + (remote ? 1 : 0);
    const sessionId = sessions[slot]!.id;
    const sessionURL = `${server}/api/v1/sessions/${encodeURIComponent(sessionId)}`;
    const upstream = new URL(server);
    const streams: { url: string; authorization?: string; status?: number }[] = [];
    const proxy = http.createServer((req, res) => {
      const url = req.url ?? "/";
      const stream = url.startsWith("/api/v1/events")
        ? { url, authorization: req.headers.authorization, status: undefined as number | undefined }
        : undefined;
      if (stream) streams.push(stream);
      const target = http.request(
        {
          hostname: upstream.hostname,
          port: upstream.port,
          path: url,
          method: req.method,
          headers: { ...req.headers, host: upstream.host },
        },
        (response) => {
          if (stream) stream.status = response.statusCode;
          res.writeHead(response.statusCode ?? 500, response.headers);
          response.pipe(res);
        },
      );
      target.on("error", () => res.destroy());
      res.on("close", () => target.destroy());
      req.pipe(target);
    });
    await new Promise<void>((resolve) => proxy.listen(0, "127.0.0.1", resolve));
    const address = proxy.address();
    if (!address || typeof address === "string") throw new Error("proxy did not bind");
    const proxyURL = `http://127.0.0.1:${address.port}`;

    try {
      expect((await request.delete(sessionURL, { headers: authHeaders })).status()).toBe(204);
      await page.addInitScript(
        ({ server, token, remote }) => {
          localStorage.setItem("agentsview-locale", "en");
          if (remote) {
            localStorage.setItem("agentsview-server-url", server);
            localStorage.setItem(`agentsview-auth-token::${server}`, token);
          } else {
            localStorage.setItem("agentsview-auth-token", token);
          }
        },
        { server: proxyURL, token: AUTH_SERVER_TOKEN, remote },
      );

      await page.goto(remote ? "/" : proxyURL);
      await expect(page.locator(".session-item").first()).toBeVisible();

      await expect.poll(() => streams.some((stream) => stream.status === 200)).toBe(true);
      const stream = streams.find((entry) => entry.status === 200)!;
      expect(stream.authorization).toBe(`Bearer ${AUTH_SERVER_TOKEN}`);
      expect(new URL(stream.url, proxyURL).searchParams.has("token")).toBe(false);

      const row = page.locator(`.session-item[data-session-id="${sessionId}"]`);
      await expect(row).toHaveCount(0);
      // Restoring emits a sessions event; only the stream can refresh the
      // sidebar here, since the fallback poll runs every five minutes.
      const restored = await request.post(`${sessionURL}/restore`, { headers: authHeaders });
      expect(restored.status()).toBe(204);
      await expect(row).toBeVisible();
    } finally {
      // Leave the shared server as it started if the test failed mid-way.
      await request.post(`${sessionURL}/restore`, { headers: authHeaders });
      await page.close();
      proxy.closeAllConnections();
      await new Promise<void>((resolve, reject) =>
        proxy.close((err) => (err ? reject(err) : resolve())),
      );
    }
  });
}
