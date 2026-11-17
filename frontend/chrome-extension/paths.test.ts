import { readdirSync, readFileSync } from "node:fs";
import { afterAll, beforeEach, describe, expect, it, vi } from "vite-plus/test";
import { platforms } from "./platforms.js";

const requests = "../internal/importer/browser_requests";
const getURL = vi.fn((file: string) => `chrome-extension://test/${file}`);
const fetchShapes = vi.fn(async (url: string) => {
  const file = url.replace("chrome-extension://test/requests/", "");
  return new Response(readFileSync(`${requests}/${file}`, "utf8"));
});
vi.stubGlobal("chrome", { runtime: { getURL } });
vi.stubGlobal("fetch", fetchShapes);
let allowedPath: (platform: string, path: string) => Promise<boolean>;
beforeEach(async () => {
  vi.resetModules();
  ({ allowedPath } = await import("./paths.js"));
});
afterAll(() => vi.unstubAllGlobals());

const names = Object.keys(platforms);

describe("browser platform registry", () => {
  it("has one allowlist per platform", () => {
    const files = readdirSync(requests).map((file: string) => file.replace(/\.txt$/, ""));
    expect([...names].sort()).toEqual([...files].sort());
  });

  it("asks host permission for exactly the platform origins", () => {
    const manifest = JSON.parse(readFileSync("chrome-extension/manifest.json", "utf8"));
    const origins = Object.values(platforms).map(({ origin }) => `${origin}/*`);
    expect([...manifest.host_permissions].sort()).toEqual([...origins].sort());
  });
});

// Go writes these fixtures from the paths Sync builds (TestBrowserPathFixtures),
// so accepting them proves the extension allows what the server requests.
describe.each(names)("%s paths Sync builds", (platform) => {
  const fixture: string[] = readFileSync(
    `../internal/importer/testdata/browser_paths/${platform}.txt`,
    "utf8",
  )
    .trim()
    .split("\n");

  it.each(fixture)("accepts %s", async (path: string) => {
    expect(await allowedPath(platform, path)).toBe(true);
  });

  it.each(fixture)("refuses mutations of %s", async (path: string) => {
    for (const mutated of [
      `${path}&extra=true`,
      `${path}#fragment`,
      `${path}/..`,
      `//example.com${path}`,
      `https://example.com${path}`,
      `${path}/`,
    ])
      expect(await allowedPath(platform, mutated)).toBe(false);
  });

  it("refuses its paths for another platform", async () => {
    expect(await allowedPath("unknown", fixture[0]!)).toBe(false);
  });
});

describe("request shape loading", () => {
  it.each(["network", "HTTP"])("retries after a %s shapes load failure", async (failure) => {
    if (failure === "network") fetchShapes.mockRejectedValueOnce(new Error("Network unavailable"));
    else fetchShapes.mockResolvedValueOnce(new Response(null, { status: 503 }));
    await expect(allowedPath("claude-ai", "/api/organizations")).rejects.toThrow();
    expect(await allowedPath("claude-ai", "/api/organizations")).toBe(true);
    expect(await allowedPath("claude-ai", "/api/settings")).toBe(false);
  });

  it("loads the packaged request shapes once", async () => {
    expect(await allowedPath("claude-ai", "/api/organizations")).toBe(true);
    expect(await allowedPath("claude-ai", "/api/organizations")).toBe(true);
    expect(getURL).toHaveBeenCalledExactlyOnceWith("requests/claude-ai.txt");
    expect(fetchShapes).toHaveBeenCalledExactlyOnceWith(
      "chrome-extension://test/requests/claude-ai.txt",
    );
  });

  it("rejects an allowlist placeholder the platform does not define", async () => {
    fetchShapes.mockResolvedValueOnce(new Response("/api/{unknown}\n"));
    await expect(allowedPath("claude-ai", "/api/x")).rejects.toThrow("Unknown request placeholder");
  });
});

const base = "/api/organizations/11111111-1111-4111-8111-111111111111";
const list = `${base}/chat_conversations_v2?limit=50&offset=0`;
const detail = `${base}/chat_conversations/22222222-2222-4222-8222-222222222222?tree=True&rendering_mode=messages&consistency=strong&render_all_tools=true&include_inline_comparison=true`;

describe("Claude.ai request allowlist", () => {
  it.each([
    detail.replace("11111111-1111-4111-8111-111111111111", "abcdefab-abcd-abcd-abcd-abcdefabcdef"),
    detail.replace("22222222-2222-4222-8222-222222222222", "ABCDEFAB-ABCD-ABCD-ABCD-ABCDEFABCDEF"),
  ])("accepts %s", async (path) => expect(await allowedPath("claude-ai", path)).toBe(true));

  it.each([
    "",
    "/api",
    "/api/../settings",
    "/api/organizations/../organizations",
    "/api/organizations?extra=true",
    `${base}/settings`,
    list.replace("limit=50", "limit=100"),
    list.replace("offset=0", "offset=-1"),
    list.replace("11111111-1111-4111-8111-111111111111", "%2e%2e"),
    detail.replace("22222222-2222-4222-8222-222222222222", ".."),
    detail.replace("22222222-2222-4222-8222-222222222222", "%2e%2e"),
    detail.replace("22222222-2222-4222-8222-222222222222", "one/../../settings"),
    detail.replace("tree=True", "tree=False"),
    list.replace("offset=0", "offset="),
    list.replace("offset=0", "offset=1.5"),
  ])("refuses %s", async (path) => expect(await allowedPath("claude-ai", path)).toBe(false));
});

describe("Gemini request allowlist", () => {
  const rpc = "/_/BardChatUi/data/batchexecute?rpcids=";
  const list = (cursor = "") => `${rpc}MaZiqc&cursor=${cursor}`;
  const detail = (chat: string, cursor = "") => `${rpc}hNvQHb&chat=${chat}&cursor=${cursor}`;

  it.each([
    "",
    "/app",
    `${rpc}ESY5D&cursor=`,
    `${list()}&rpcids=ESY5D`,
    list("a&rpcids=ESY5D"),
    list("a/b"),
    detail("0123456789abcdef"),
    detail("c_../app"),
    detail("c_0123%2F"),
  ])("refuses %s", async (path) => expect(await allowedPath("gemini-apps", path)).toBe(false));
});
