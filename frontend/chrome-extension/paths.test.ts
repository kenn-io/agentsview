import { readFileSync } from "node:fs";
import { afterAll, beforeEach, describe, expect, it, vi } from "vite-plus/test";

const getURL = vi.fn(() => "chrome-extension://test/claude_ai_requests.txt");
const fetchShapes = vi.fn(async () => new Response(readFileSync("../internal/importer/claude_ai_requests.txt", "utf8")));
vi.stubGlobal("chrome", { runtime: { getURL } });
vi.stubGlobal("fetch", fetchShapes);
let allowedPath: (path: string) => Promise<boolean>;
beforeEach(async () => {
  vi.resetModules();
  ({ allowedPath } = await import("./paths.js"));
});
afterAll(() => vi.unstubAllGlobals());

const base = "/api/organizations/11111111-1111-4111-8111-111111111111";
const list = `${base}/chat_conversations_v2?limit=50&offset=0`;
const detail = `${base}/chat_conversations/22222222-2222-4222-8222-222222222222?tree=True&rendering_mode=messages&consistency=strong&render_all_tools=true&include_inline_comparison=true`;

describe("Claude request allowlist", () => {
  it("loads the packaged request shapes", async () => {
    expect(await allowedPath("/api/organizations")).toBe(true);
    expect(getURL).toHaveBeenCalledExactlyOnceWith("claude_ai_requests.txt");
    expect(fetchShapes).toHaveBeenCalledExactlyOnceWith("chrome-extension://test/claude_ai_requests.txt");
  });

  it.each([
    "/api/organizations", list, list.replace("offset=0", "offset=50"), detail,
    detail.replace("11111111-1111-4111-8111-111111111111", "abcdefab-abcd-abcd-abcd-abcdefabcdef"),
    detail.replace("22222222-2222-4222-8222-222222222222", "ABCDEFAB-ABCD-ABCD-ABCD-ABCDEFABCDEF"),
  ])("accepts %s", async (path) => expect(await allowedPath(path)).toBe(true));

  it.each([
    "", "/api", "/api/../settings", "/api/organizations/../organizations",
    "//example.com/api/organizations", "https://claude.ai/api/organizations",
    "/api/organizations?extra=true", `${base}/settings`,
    list.replace("limit=50", "limit=100"), list.replace("offset=0", "offset=-1"),
    `${list}&extra=true`, `${list}#fragment`,
    list.replace("11111111-1111-4111-8111-111111111111", "%2e%2e"),
    detail.replace("22222222-2222-4222-8222-222222222222", ".."),
    detail.replace("22222222-2222-4222-8222-222222222222", "%2e%2e"),
    detail.replace("22222222-2222-4222-8222-222222222222", "one/../../settings"),
    detail.replace("tree=True", "tree=False"), `${detail}&extra=true`,
    list.replace("offset=0", "offset="), list.replace("offset=0", "offset=1.5"),
  ])("refuses %s", async (path) => expect(await allowedPath(path)).toBe(false));
});
