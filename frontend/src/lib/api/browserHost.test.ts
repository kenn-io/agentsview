import { afterEach, describe, expect, it, vi } from "vite-plus/test";
import { getBrowserHost } from "./browserHost.js";

afterEach(() => {
  delete document.documentElement.dataset.agentsviewClaudeHost;
  vi.useRealTimers();
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
});

describe("browser host discovery", () => {
  it("times out a silent bridge after 100 seconds and removes its listener", async () => {
    vi.useFakeTimers();
    document.documentElement.dataset.agentsviewClaudeHost = "chrome";
    vi.spyOn(window, "postMessage").mockImplementation(() => {});
    const add = vi.spyOn(window, "addEventListener");
    const remove = vi.spyOn(window, "removeEventListener");
    const request = getBrowserHost()!.fetch("/api/organizations");
    const rejection = expect(request).rejects.toThrow("Claude host request timed out");
    await vi.advanceTimersByTimeAsync(99_999);
    expect(remove).not.toHaveBeenCalled();
    await vi.advanceTimersByTimeAsync(1);
    await rejection;
    expect(remove).toHaveBeenCalledExactlyOnceWith("message", add.mock.calls[0]![1]);
    expect(vi.getTimerCount()).toBe(0);
  });

  it("resolves a reply before the deadline and clears its timer", async () => {
    vi.useFakeTimers();
    document.documentElement.dataset.agentsviewClaudeHost = "chrome";
    const post = vi.spyOn(window, "postMessage").mockImplementation(() => {});
    const request = getBrowserHost()!.fetch("/api/organizations");
    await vi.advanceTimersByTimeAsync(99_999);
    window.dispatchEvent(new MessageEvent("message", { source: window, data: { type: "agentsview-claude-reply", id: post.mock.calls[0]![0].id, result: { status: 200, body: "[]" } } }));
    await expect(request).resolves.toEqual({ status: 200, body: "[]" });
    expect(vi.getTimerCount()).toBe(0);
  });

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
  });

  it("rejects a reply without a result or error", async () => {
    document.documentElement.dataset.agentsviewClaudeHost = "chrome";
    vi.spyOn(window, "postMessage").mockImplementation((data) => {
      window.dispatchEvent(new MessageEvent("message", { source: window, data: { type: "agentsview-claude-reply", id: data.id } }));
    });
    await expect(getBrowserHost()!.fetch("/api/organizations")).rejects.toThrow("Claude host returned no result");
  });

  it("accepts null results for connect", async () => {
    document.documentElement.dataset.agentsviewClaudeHost = "chrome";
    vi.spyOn(window, "postMessage").mockImplementation((data) => {
      window.dispatchEvent(new MessageEvent("message", { source: window, data: { type: "agentsview-claude-reply", id: data.id, result: null } }));
    });
    await expect(getBrowserHost()!.connect()).resolves.toBeNull();
  });

  it("closes locally in Chrome", async () => {
    document.documentElement.dataset.agentsviewClaudeHost = "chrome";
    const post = vi.spyOn(window, "postMessage");
    const host = getBrowserHost()!;
    await expect(host.close()).resolves.toBeUndefined();
    expect(post).not.toHaveBeenCalled();
  });
});
