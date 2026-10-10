import { readFileSync } from "node:fs";
import { afterEach, beforeEach, describe, expect, it, vi } from "vite-plus/test";

let message: (request: object) => void;
let disconnect: () => void;
const port = {
  postMessage: vi.fn(),
  onMessage: { addListener: vi.fn((callback) => { message = callback; }) },
  onDisconnect: { addListener: vi.fn((callback) => { disconnect = callback; }) },
};
const chrome = {
  runtime: {
    getURL: vi.fn(() => "chrome-extension://test/claude_ai_requests.txt"),
    connectNative: vi.fn(() => port),
    onStartup: { addListener: vi.fn() },
    onInstalled: { addListener: vi.fn() },
  },
  tabs: {
    query: vi.fn(), create: vi.fn(), get: vi.fn(), remove: vi.fn(),
    onUpdated: { addListener: vi.fn(), removeListener: vi.fn() },
    onRemoved: { addListener: vi.fn(), removeListener: vi.fn() },
  },
  scripting: { executeScript: vi.fn() },
};
beforeEach(async () => {
  vi.resetModules();
  vi.clearAllMocks();
  vi.stubGlobal("chrome", chrome);
  vi.stubGlobal("fetch", vi.fn(async () => new Response(readFileSync("../internal/importer/claude_ai_requests.txt", "utf8"))));
  chrome.tabs.query.mockResolvedValue([{ id: 7 }]);
  chrome.tabs.get.mockResolvedValue({ id: 7, status: "complete" });
  chrome.tabs.create.mockResolvedValue({ id: 8 });
  chrome.scripting.executeScript.mockImplementation(async (options) => options.args ? [{ result: { status: 200, body: "chats" } }] : [{ result: true }]);
  await import("./worker.js");
});
afterEach(() => { vi.unstubAllGlobals(); vi.useRealTimers(); });

async function request(path = "/api/organizations") {
  message({ id: "a", path, version: 1 });
  await vi.waitFor(() => expect(port.postMessage).toHaveBeenCalledOnce());
  return port.postMessage.mock.calls[0]![0];
}

describe("Chrome native host worker", () => {
  it.each([
    [undefined, "Restart AgentsView if you upgraded it, run agentsview chrome setup, reload the extension at chrome://extensions, then Sync again."],
    [0, "Restart AgentsView if you upgraded it, run agentsview chrome setup, reload the extension at chrome://extensions, then Sync again."],
    [2, "Restart AgentsView if you upgraded it, run agentsview chrome setup, reload the extension at chrome://extensions, then Sync again."],
    [1, "Unsupported Claude fetch path"],
  ])("refuses before touching tabs", async (version, error) => {
    message({ id: "a", path: "/api/settings", version });
    await vi.waitFor(() => expect(port.postMessage).toHaveBeenCalledOnce());
    expect(port.postMessage).toHaveBeenCalledWith({ id: "a", version: 1, status: 0, error });
    expect(chrome.tabs.query).not.toHaveBeenCalled();
    expect(chrome.tabs.create).not.toHaveBeenCalled();
    expect(chrome.scripting.executeScript).not.toHaveBeenCalled();
    if (version !== 1) expect(fetch).not.toHaveBeenCalled();
  });

  it("fetches in an existing Claude tab and replies by id", async () => {
    expect(await request()).toEqual({ id: "a", version: 1, status: 200, body: "chats" });
    expect(chrome.runtime.connectNative).toHaveBeenCalledExactlyOnceWith("io.kenn.agentsview");
    expect(chrome.tabs.query).toHaveBeenCalledWith({ url: "https://claude.ai/*", discarded: false });
    expect(chrome.scripting.executeScript).toHaveBeenLastCalledWith({ target: { tabId: 7 }, world: "ISOLATED", func: expect.any(Function), args: ["https://claude.ai/api/organizations"] });
    expect(chrome.tabs.remove).not.toHaveBeenCalled();
  });

  it("injects claudeFetch into a fresh tab and leaves it open", async () => {
    const claudeFetch = vi.fn(() => ({ status: 200, body: "chats" }));
    chrome.scripting.executeScript.mockImplementation(async (options) => {
      if (options.files) {
        vi.stubGlobal("claudeFetch", claudeFetch);
        return [];
      }
      return [{ result: await options.func(...(options.args ?? [])) ?? null }];
    });
    chrome.tabs.query.mockResolvedValue([]);
    expect(await request()).toEqual({ id: "a", version: 1, status: 200, body: "chats" });
    expect(chrome.tabs.create).toHaveBeenCalledWith({ url: "https://claude.ai/new", active: false });
    expect(chrome.tabs.remove).not.toHaveBeenCalled();
    expect(chrome.scripting.executeScript).toHaveBeenNthCalledWith(1, { target: { tabId: 8 }, files: ["claude_fetch.js"], world: "ISOLATED" });
    expect(claudeFetch).toHaveBeenCalledExactlyOnceWith("https://claude.ai/api/organizations");
  });

  it("waits for the Claude tab to load before injecting the reader", async () => {
    chrome.tabs.get.mockResolvedValue({ id: 7, status: "loading" });
    const pending = request();
    await vi.waitFor(() => expect(chrome.tabs.onUpdated.addListener).toHaveBeenCalledOnce());
    expect(chrome.scripting.executeScript).not.toHaveBeenCalled();
    const updated = chrome.tabs.onUpdated.addListener.mock.calls[0]![0];
    updated(9, { status: "complete" });
    expect(chrome.scripting.executeScript).not.toHaveBeenCalled();
    updated(7, { status: "complete" });
    expect(await pending).toEqual({ id: "a", version: 1, status: 200, body: "chats" });
    expect(chrome.tabs.onUpdated.removeListener).toHaveBeenCalledWith(updated);
  });

  it("replaces an oversized serialized reply with 413", async () => {
    chrome.scripting.executeScript.mockResolvedValueOnce([{ result: true }]).mockResolvedValueOnce([{ result: { status: 200, body: '"'.repeat(32 * 1024 * 1024) } }]);
    expect(await request()).toEqual({ id: "a", version: 1, status: 413 });
  });

  it.each([null, {}, { status: "200" }])("rejects a fetch reply without numeric status: %s", async (result) => {
    chrome.scripting.executeScript.mockResolvedValueOnce([{ result: true }]).mockResolvedValueOnce([{ result }]);
    expect(await request()).toEqual({ id: "a", version: 1, status: 0, error: "Claude.ai page changed during Sync; try Sync again" });
  });

  it("reconnects five seconds after disconnect without duplicating startup connections", async () => {
    vi.useFakeTimers();
    chrome.runtime.onStartup.addListener.mock.calls[0]![0]();
    chrome.runtime.onInstalled.addListener.mock.calls[0]![0]();
    expect(chrome.runtime.connectNative).toHaveBeenCalledOnce();
    disconnect();
    await vi.advanceTimersByTimeAsync(4999);
    expect(chrome.runtime.connectNative).toHaveBeenCalledOnce();
    await vi.advanceTimersByTimeAsync(1);
    expect(chrome.runtime.connectNative).toHaveBeenCalledTimes(2);
  });
});
