import { readFileSync } from "node:fs";
import { runInNewContext } from "node:vm";
import { describe, expect, it, vi } from "vite-plus/test";

const source = readFileSync("chrome-extension/chatgpt_fetch.js", "utf8");

function loadFetch(fetch: typeof globalThis.fetch) {
  const context: Record<string, unknown> = {
    AbortController,
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
});
