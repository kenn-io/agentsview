import { readFileSync } from "node:fs";
import { runInNewContext } from "node:vm";
import { describe, expect, it, vi } from "vite-plus/test";

const source =
  readFileSync("chrome-extension/claude_fetch.js", "utf8") +
  "\n" +
  readFileSync("chrome-extension/chatgpt_fetch.js", "utf8");

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
  it("fetches the page session token and sends it as Bearer without echoing it", async () => {
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

  it.each([
    { name: "readable expiry", readableExpiry: true, wantSessionCalls: 1, wantFetchCalls: 3 },
    { name: "unreadable expiry", readableExpiry: false, wantSessionCalls: 2, wantFetchCalls: 4 },
  ])(
    "caches tokens according to $name",
    async ({ readableExpiry, wantSessionCalls, wantFetchCalls }) => {
      const claims = Buffer.from(
        JSON.stringify({ exp: Math.floor(Date.now() / 1000) + 3600 }),
      ).toString("base64url");
      const token = readableExpiry
        ? `eyJhbGciOiJub25lIn0.${claims}.synthetic`
        : "synthetic-test-value";
      const fetch = vi
        .fn<typeof globalThis.fetch>()
        .mockImplementation(async (input) =>
          String(input).endsWith("/api/auth/session")
            ? new Response(JSON.stringify({ accessToken: token }))
            : new Response('{"items":[]}'),
        );
      const chatgptFetch = loadFetch(fetch);

      await chatgptFetch(
        "https://chatgpt.com/backend-api/conversations?offset=0&limit=50&order=updated",
      );
      await chatgptFetch(
        "https://chatgpt.com/backend-api/conversation/22222222-2222-4222-8222-222222222222",
      );

      const sessionCalls = fetch.mock.calls.filter(([input]) =>
        String(input).endsWith("/api/auth/session"),
      );
      expect(sessionCalls).toHaveLength(wantSessionCalls);
      expect(fetch).toHaveBeenCalledTimes(wantFetchCalls);
    },
  );
  it("fetches a new session after the conversation API rejects the cached token", async () => {
    const claims = Buffer.from(
      JSON.stringify({ exp: Math.floor(Date.now() / 1000) + 3600 }),
    ).toString("base64url");
    const token = `eyJhbGciOiJub25lIn0.${claims}.synthetic`;
    let conversationStatus = 401;
    const fetch = vi
      .fn<typeof globalThis.fetch>()
      .mockImplementation(async (input) =>
        String(input).endsWith("/api/auth/session")
          ? new Response(JSON.stringify({ accessToken: token }))
          : new Response('{"items":[]}', { status: conversationStatus }),
      );
    const chatgptFetch = loadFetch(fetch);
    const url = "https://chatgpt.com/backend-api/conversations?offset=0&limit=50&order=updated";

    await expect(chatgptFetch(url)).resolves.toMatchObject({ status: 401 });
    conversationStatus = 200;
    await chatgptFetch(url);

    const sessionCalls = fetch.mock.calls.filter(([input]) =>
      String(input).endsWith("/api/auth/session"),
    );
    expect(sessionCalls).toHaveLength(2);
  });

  it("forwards Retry-After from a rate-limited session response", async () => {
    const fetch = vi
      .fn<typeof globalThis.fetch>()
      .mockResolvedValue(new Response("{}", { status: 429, headers: { "retry-after": "120" } }));
    const chatgptFetch = loadFetch(fetch);

    await expect(
      chatgptFetch("https://chatgpt.com/backend-api/conversations?offset=0&limit=50&order=updated"),
    ).resolves.toEqual({ status: 429, body: "{}", retryAfter: "120" });
    expect(fetch).toHaveBeenCalledOnce();
  });
  it.each(["session", "conversation"])(
    "caps oversized %s responses through the shared page reader",
    async (target) => {
      const fetch = vi.fn<typeof globalThis.fetch>();
      if (target === "conversation") {
        fetch.mockResolvedValueOnce(
          new Response(JSON.stringify({ accessToken: "synthetic-test-value" })),
        );
      }
      fetch.mockResolvedValueOnce(new Response(new Uint8Array(32 * 1024 * 1024 + 1)));
      await expect(
        loadFetch(fetch)(
          "https://chatgpt.com/backend-api/conversation/22222222-2222-4222-8222-222222222222",
        ),
      ).resolves.toEqual({ status: 413, body: "" });
      expect(fetch).toHaveBeenCalledTimes(target === "session" ? 1 : 2);
    },
  );
});
