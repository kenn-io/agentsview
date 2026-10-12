import { readFileSync } from "node:fs";
import { afterEach, beforeEach, describe, expect, it, vi } from "vite-plus/test";

let message: (request: object) => void;
let disconnect: () => void;
const port = {
  postMessage: vi.fn(),
  onMessage: {
    addListener: vi.fn((callback) => {
      message = callback;
    }),
  },
  onDisconnect: {
    addListener: vi.fn((callback) => {
      disconnect = callback;
    }),
  },
};
const chrome = {
  runtime: {
    getURL: vi.fn((file: string) => `chrome-extension://test/${file}`),
    connectNative: vi.fn(() => port),
    onStartup: { addListener: vi.fn() },
    onInstalled: { addListener: vi.fn() },
  },
  tabs: {
    query: vi.fn(),
    create: vi.fn(),
    get: vi.fn(),
    remove: vi.fn(),
    onUpdated: { addListener: vi.fn(), removeListener: vi.fn() },
    onRemoved: { addListener: vi.fn(), removeListener: vi.fn() },
  },
  scripting: { executeScript: vi.fn() },
};
beforeEach(async () => {
  vi.resetModules();
  vi.clearAllMocks();
  vi.stubGlobal("chrome", chrome);
    chrome.scripting.executeScript.mockResolvedValueOnce([{ result: true }]).mockResolvedValueOnce([{ result: { status: 200, body: '"'.repeat(32 * 1024 * 1024) } }]);
    expect(await request()).toEqual({ id: "a", platform: "claude-ai", version: 2, status: 413 });
  });

  it.each([null, {}, { status: "200" }])("rejects a fetch reply without numeric status: %s", async (result) => {
    chrome.scripting.executeScript.mockResolvedValueOnce([{ result: true }]).mockResolvedValueOnce([{ result }]);
    expect(await request()).toEqual({ id: "a", platform: "claude-ai", version: 2, status: 0, error: "The browser page changed during Sync; try Sync again" });
  });

  it("routes ChatGPT requests through a ChatGPT page with its own fetch script", async () => {
    const chatgptFetch = vi.fn(() => ({ status: 200, body: "conversations" }));
    chrome.scripting.executeScript.mockImplementation(async (options) => {
      if (options.files) {
        vi.stubGlobal("chatgptFetch", chatgptFetch);
        return [];
      }
      return [{ result: await options.func(...(options.args ?? [])) ?? null }];
    });
    expect(await request("/backend-api/conversations?offset=0&limit=50&order=updated", "chatgpt")).toEqual({
      id: "a", platform: "chatgpt", version: 2, status: 200, body: "conversations",
    });
    expect(chrome.tabs.query).toHaveBeenCalledWith({ url: "https://chatgpt.com/*", discarded: false });
    expect(chrome.scripting.executeScript).toHaveBeenNthCalledWith(1, { target: { tabId: 7 }, files: ["chatgpt_fetch.js"], world: "ISOLATED" });
    expect(chatgptFetch).toHaveBeenCalledExactlyOnceWith("https://chatgpt.com/backend-api/conversations?offset=0&limit=50&order=updated");
  });
