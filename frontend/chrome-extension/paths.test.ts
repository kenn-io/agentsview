import { readFileSync } from "node:fs";
import { afterAll, beforeEach, describe, expect, it, vi } from "vite-plus/test";

const getURL = vi.fn((file: string) => `chrome-extension://test/${file}`);
const fetchShapes = vi.fn(async (url: string) => {
  const file = url.endsWith("chatgpt_requests.txt")
    ? "chatgpt_requests.txt"
    : "claude_ai_requests.txt";
  return new Response(readFileSync(`../internal/importer/${file}`, "utf8"));
});
vi.stubGlobal("chrome", { runtime: { getURL } });
vi.stubGlobal("fetch", fetchShapes);
let allowedPath: (platform: string, path: string) => Promise<boolean>;
beforeEach(async () => {
  vi.resetModules();
  ({ allowedPath } = await import("./paths.js"));
});
afterAll(() => vi.unstubAllGlobals());

const base = "/api/organizations/11111111-1111-4111-8111-111111111111";
const list = `${base}/chat_conversations_v2?limit=50&offset=0`;
const detail = `${base}/chat_conversations/22222222-2222-4222-8222-222222222222?tree=True&rendering_mode=messages&consistency=strong&render_all_tools=true&include_inline_comparison=true`;

describe("Claude request allowlist", () => {
  it.each(["network", "HTTP"])("retries after a %s shapes load failure", async (failure) => {
    if (failure === "network") fetchShapes.mockRejectedValueOnce(new Error("Network unavailable"));
    else fetchShapes.mockResolvedValueOnce(new Response(null, { status: 503 }));
    await expect(allowedPath("claude-ai", "/api/organizations")).rejects.toThrow();
    expect(await allowedPath("claude-ai", "/api/organizations")).toBe(true);
    expect(await allowedPath("claude-ai", "/api/settings")).toBe(false);
  });

  it("loads the packaged request shapes", async () => {
    expect(await allowedPath("claude-ai", "/api/organizations")).toBe(true);
    expect(getURL).toHaveBeenCalledExactlyOnceWith("claude_ai_requests.txt");
    expect(fetchShapes).toHaveBeenCalledExactlyOnceWith(
      "chrome-extension://test/claude_ai_requests.txt",
    );
  });

  it.each([
    "/api/organizations",
    list,
    list.replace("offset=0", "offset=50"),
    detail,
    detail.replace("11111111-1111-4111-8111-111111111111", "abcdefab-abcd-abcd-abcd-abcdefabcdef"),
    detail.replace("22222222-2222-4222-8222-222222222222", "ABCDEFAB-ABCD-ABCD-ABCD-ABCDEFABCDEF"),
  ])("accepts %s", async (path) => expect(await allowedPath("claude-ai", path)).toBe(true));

  it.each([
    "",
    "/api",
    "/api/../settings",
    "/api/organizations/../organizations",
    "//example.com/api/organizations",
    "https://claude.ai/api/organizations",
    "/api/organizations?extra=true",
    `${base}/settings`,
    list.replace("limit=50", "limit=100"),
    list.replace("offset=0", "offset=-1"),
    `${list}&extra=true`,
    `${list}#fragment`,
    list.replace("11111111-1111-4111-8111-111111111111", "%2e%2e"),
    detail.replace("22222222-2222-4222-8222-222222222222", ".."),
    detail.replace("22222222-2222-4222-8222-222222222222", "%2e%2e"),
    detail.replace("22222222-2222-4222-8222-222222222222", "one/../../settings"),
    detail.replace("tree=True", "tree=False"),
    `${detail}&extra=true`,
    list.replace("offset=0", "offset="),
    list.replace("offset=0", "offset=1.5"),
  ])("refuses %s", async (path) => expect(await allowedPath("claude-ai", path)).toBe(false));
});

describe("ChatGPT request allowlist", () => {
  const listPath = (offset: string) =>
    `/backend-api/conversations?offset=${offset}&limit=50&order=updated`;
  const detailPath = (id: string) => `/backend-api/conversation/${id}`;

  it.each([listPath("0"), listPath("50"), detailPath("22222222-2222-4222-8222-222222222222")])(
    "accepts %s",
    async (path) => expect(await allowedPath("chatgpt", path)).toBe(true),
  );

  it.each([
    "",
    "/api/auth/session",
    "/backend-api/auth/session",
    "/backend-api/accounts/check",
    "https://chatgpt.com/backend-api/conversations?offset=0&limit=50&order=updated",
    listPath("-1"),
    listPath("1.5"),
    listPath("1&extra=true"),
    listPath("0").replace("limit=50", "limit=100"),
    `${listPath("0")}&extra=true`,
    detailPath("not-a-uuid"),
    detailPath("../settings"),
    "/backend-api/conversation/22222222-2222-4222-8222-222222222222?include=all",
    "/api/organizations/11111111-1111-4111-8111-111111111111/chat_conversations_v2?limit=50&offset=0",
  ])("refuses %s", async (path) => expect(await allowedPath("chatgpt", path)).toBe(false));

  it("rejects unknown platforms without loading a request file", async () => {
    expect(await allowedPath("gemini", listPath("0"))).toBe(false);
  });
});
