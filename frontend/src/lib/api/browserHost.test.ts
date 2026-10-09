import { afterEach, describe, expect, it, vi } from "vite-plus/test";
import { getBrowserHost } from "./browserHost.js";

afterEach(() => {
  delete document.documentElement.dataset.agentsviewClaudeHost;
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
});

describe("browser host discovery", () => {
  it("returns undefined without Tauri or the extension", () => {
    expect(getBrowserHost()).toBeUndefined();
  });

  it("matches concurrent fetch replies by id and ignores foreign replies", async () => {
    document.documentElement.dataset.agentsviewClaudeHost = "chrome";
    const post = vi.spyOn(window, "postMessage").mockImplementation(() => {});
    const host = getBrowserHost()!;
    const first = host.fetch("/api/organizations");
    const second = host.fetch("/other");
    const firstId = post.mock.calls[0]![0].id;
    const secondId = post.mock.calls[1]![0].id;
    expect(firstId).not.toBe(secondId);
    expect(post.mock.calls[0]).toEqual([{ type: "agentsview-claude-request", id: firstId, method: "fetch", path: "/api/organizations" }, location.origin]);
    function reply(id: string, body: string, source: Window | null = window, origin = location.origin) {
      window.dispatchEvent(new MessageEvent("message", { source, origin, data: { type: "agentsview-claude-reply", id, result: { status: 200, body } } }));
    }
    reply(firstId, "foreign", null);
    reply(firstId, "foreign", window, "https://example.com");
    reply("unrelated", "foreign");
    reply(secondId, "second");
    reply(firstId, "first");
    await expect(first).resolves.toEqual({ status: 200, body: "first" });
    await expect(second).resolves.toEqual({ status: 200, body: "second" });
  });

  it("rejects bridge errors", async () => {
    document.documentElement.dataset.agentsviewClaudeHost = "chrome";
    vi.spyOn(window, "postMessage").mockImplementation((data) => {
      window.dispatchEvent(new MessageEvent("message", { source: window, origin: location.origin, data: { type: "agentsview-claude-reply", id: data.id, error: "Allow Sync" } }));
    });
    await expect(getBrowserHost()!.connect()).rejects.toThrow("Allow Sync");
    await expect(getBrowserHost()!.close()).rejects.toThrow("Allow Sync");
  });
});
