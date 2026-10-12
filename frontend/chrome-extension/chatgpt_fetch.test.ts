import { readFileSync } from "node:fs";
import { runInNewContext } from "node:vm";
import { describe, expect, it, vi } from "vite-plus/test";

const source = readFileSync("chrome-extension/chatgpt_fetch.js", "utf8");

function loadFetch(fetch: typeof globalThis.fetch) {
  const context: Record<string, unknown> = {
    AbortController,
    atob,
    TextDecoder,
    TextEncoder,
    clearTimeout,
    fetch,
    setTimeout,
  };
  runInNewContext(`${source}\nglobalThis.chatgptFetch = chatgptFetch;`, context);
  return context.chatgptFetch as (url: string) => Promise<unknown>;
}

describe("ChatGPT page fetch", () => {
  it("uses the page session token only for the approved API request", async () => {
    const fetch = vi
      .fn<typeof globalThis.fetch>()
      .mockResolvedValueOnce(new Response(JSON.stringify({ accessToken: "synthetic-test-value" })))
      .mockResolvedValueOnce(new Response('{"items":[]}'));
    const chatgptFetch = loadFetch(fetch);

    const response = await chatgptFetch(
      "https://chatgpt.com/backend-api/conversations?offset=0&limit=50&order=updated",
    );

    expect(fetch).toHaveBeenNthCalledWith(
      1,
      "https://chatgpt.com/api/auth/session",
      expect.objectContaining({ credentials: "include" }),
    );
    expect(fetch).toHaveBeenNthCalledWith(
      2,
      "https://chatgpt.com/backend-api/conversations?offset=0&limit=50&order=updated",
      expect.objectContaining({
        credentials: "include",
        headers: { Authorization: "Bearer synthetic-test-value" },
      }),
    );
    expect(response).toEqual({ status: 200, body: '{"items":[]}', retryAfter: undefined });
    expect(JSON.stringify(response)).not.toContain("synthetic-test-value");
  });

  it("does not make API requests when the page has no access token", async () => {
    const fetch = vi.fn<typeof globalThis.fetch>().mockResolvedValue(new Response("{}"));
    const chatgptFetch = loadFetch(fetch);

    await expect(
      chatgptFetch(
        "https://chatgpt.com/backend-api/conversation/22222222-2222-4222-8222-222222222222",
      ),
    ).resolves.toEqual({ status: 401, body: "" });
    expect(fetch).toHaveBeenCalledOnce();
  });

  it("reuses a JWT access token across requests until it nears expiry", async () => {
    const claims = Buffer.from(JSON.stringify({ exp: Math.floor(Date.now() / 1000) + 3600 })).toString("base64url");
    const token = `eyJhbGciOiJub25lIn0.${claims}.synthetic`;
    const fetch = vi.fn<typeof globalThis.fetch>().mockImplementation(async (input) =>
      String(input).endsWith("/api/auth/session")
        ? new Response(JSON.stringify({ accessToken: token }))
        : new Response('{"items":[]}'),
    );
    const chatgptFetch = loadFetch(fetch);

    await chatgptFetch("https://chatgpt.com/backend-api/conversations?offset=0&limit=50&order=updated");
    await chatgptFetch("https://chatgpt.com/backend-api/conversation/22222222-2222-4222-8222-222222222222");

    const sessionCalls = fetch.mock.calls.filter(([input]) => String(input).endsWith("/api/auth/session"));
    expect(sessionCalls).toHaveLength(1);
    expect(fetch).toHaveBeenCalledTimes(3);
  });

  it("does not cache a token whose expiry cannot be read", async () => {
    const fetch = vi.fn<typeof globalThis.fetch>().mockImplementation(async (input) =>
      String(input).endsWith("/api/auth/session")
        ? new Response(JSON.stringify({ accessToken: "synthetic-test-value" }))
        : new Response('{"items":[]}'),
    );
    const chatgptFetch = loadFetch(fetch);

    await chatgptFetch("https://chatgpt.com/backend-api/conversations?offset=0&limit=50&order=updated");
    await chatgptFetch("https://chatgpt.com/backend-api/conversations?offset=50&limit=50&order=updated");

    const sessionCalls = fetch.mock.calls.filter(([input]) => String(input).endsWith("/api/auth/session"));
    expect(sessionCalls).toHaveLength(2);
  });
});
