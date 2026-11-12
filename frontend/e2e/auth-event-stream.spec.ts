import { expect, test } from "@playwright/test";
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
    expect((await request.delete(sessionURL, { headers: authHeaders })).status()).toBe(204);

    try {
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
        { server, token: AUTH_SERVER_TOKEN, remote },
      );
      const isStream = (url: string) => url.startsWith(`${server}/api/v1/events`);
      const streamRequest = page.waitForRequest((req) => isStream(req.url()));
      const streamResponse = page.waitForResponse((res) => isStream(res.url()));

      await page.goto(remote ? "/" : server);
      await expect(page.locator(".session-item").first()).toBeVisible();

      const req = await streamRequest;
      expect(await req.headerValue("authorization")).toBe(`Bearer ${AUTH_SERVER_TOKEN}`);
      expect(new URL(req.url()).searchParams.has("token")).toBe(false);
      expect((await streamResponse).status()).toBe(200);

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
    }
  });
}
