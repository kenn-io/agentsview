import { afterEach, beforeEach, describe, expect, it, vi } from "vite-plus/test";

let message: (request: object, sender: object, respond: (result: unknown) => void) => boolean;
let click: (tab: object) => Promise<void>;
let removed: (id: number) => Promise<void>;
let consent: Record<string, string>;
const chrome = {
  action: { onClicked: { addListener: vi.fn((callback) => { click = callback; }) } },
  runtime: { onMessage: { addListener: vi.fn((callback) => { message = callback; }) } },
  storage: { session: { get: vi.fn(), set: vi.fn(), remove: vi.fn() } },
  tabs: {
    query: vi.fn(), create: vi.fn(), get: vi.fn(), remove: vi.fn(),
    onUpdated: { addListener: vi.fn(), removeListener: vi.fn() },
    onRemoved: { addListener: vi.fn((callback) => { removed = callback; }), removeListener: vi.fn() },
  },
  scripting: { executeScript: vi.fn() },
};
const origin = "http://localhost:8090";
const sender = { origin, tab: { id: 1 }, documentId: "document-a" };
function request(method: string, path?: string, from: { origin: string; tab: { id: number }; documentId?: string } = sender) {
  return new Promise((resolve) => {
    expect(message({ id: "a", method, path }, from, resolve)).toBe(true);
  });
}
beforeEach(async () => {
  vi.resetModules();
  vi.resetAllMocks();
  vi.stubGlobal("chrome", chrome);
  consent = { "1": "document-a" };
  chrome.storage.session.get.mockImplementation(async (key) => ({ [key]: consent[key] }));
  chrome.storage.session.set.mockImplementation(async (value) => { Object.assign(consent, value); });
  chrome.storage.session.remove.mockImplementation(async (key) => { delete consent[key]; });
  chrome.tabs.query.mockResolvedValue([{ id: 7 }]);
  chrome.tabs.get.mockResolvedValue({ id: 7, status: "complete" });
  chrome.tabs.create.mockResolvedValue({ id: 8 });
  chrome.scripting.executeScript.mockResolvedValue([{ result: { status: 200, body: "chats" } }]);
  await import("./worker.js");
});
afterEach(() => vi.unstubAllGlobals());

describe("Chrome host worker", () => {
  it.each([
    { ...sender, tab: { id: 2 } },
    { ...sender, documentId: "document-b" },
    { ...sender, documentId: undefined },
  ])("refuses requests without document consent: %j", async (from) => {
    expect(await request("fetch", "/api/organizations", from)).toEqual({ error: expect.stringContaining("toolbar button") });
    expect(chrome.tabs.query).not.toHaveBeenCalled();
    expect(chrome.tabs.create).not.toHaveBeenCalled();
    expect(chrome.scripting.executeScript).not.toHaveBeenCalled();
  });

  it("refuses a disallowed path before touching tabs", async () => {
    expect(await request("fetch", "/api/settings")).toEqual({ error: "Unsupported Claude fetch path" });
    expect(chrome.tabs.query).not.toHaveBeenCalled();
    expect(chrome.tabs.create).not.toHaveBeenCalled();
    expect(chrome.scripting.executeScript).not.toHaveBeenCalled();
  });

  it("grants only the clicked document and keeps consent across worker restarts", async () => {
    consent = {};
    chrome.scripting.executeScript.mockResolvedValueOnce([{ documentId: "document-a" }]);
    await click({ id: 1, url: "http://127.0.0.1:8081/sessions" });
    expect(chrome.scripting.executeScript).toHaveBeenCalledWith({ target: { tabId: 1 }, func: expect.any(Function) });
    vi.resetModules();
    await import("./worker.js");
    expect(await request("connect")).toEqual({ result: null });
    expect(await request("connect", undefined, { ...sender, tab: { id: 2 } })).toEqual({ error: expect.stringContaining("toolbar button") });
    chrome.storage.session.set.mockClear();
    await click({ url: "https://example.com" });
    expect(chrome.storage.session.set).not.toHaveBeenCalled();
  });

  it.each(["navigation", "reload"])("refuses the new document after %s", async () => {
    expect(await request("fetch", "/api/organizations", { ...sender, documentId: "document-new" })).toEqual({ error: expect.stringContaining("toolbar button") });
    expect(chrome.scripting.executeScript).not.toHaveBeenCalled();
  });

  it("revokes consent on tab close", async () => {
    await removed(1);
    expect(await request("fetch", "/api/organizations")).toEqual({ error: expect.stringContaining("toolbar button") });
    expect(chrome.scripting.executeScript).not.toHaveBeenCalled();
  });

  it("opens sign-in in Chrome", async () => {
    await request("connect");
    expect(chrome.tabs.create).toHaveBeenCalledExactlyOnceWith({ url: "https://claude.ai/login?return_url=%2Fnew" });
  });

  it("fetches in an existing Claude tab and leaves it open", async () => {
    expect(await request("fetch", "/api/organizations")).toEqual({ result: { status: 200, body: "chats" } });
    expect(chrome.tabs.query).toHaveBeenCalledWith({ url: "https://claude.ai/*", discarded: false });
    expect(chrome.scripting.executeScript).toHaveBeenNthCalledWith(1, { target: { tabId: 7 }, files: ["claude_fetch.js"], world: "ISOLATED" });
    expect(chrome.scripting.executeScript).toHaveBeenNthCalledWith(2, { target: { tabId: 7 }, world: "ISOLATED", func: expect.any(Function), args: ["https://claude.ai/api/organizations"] });
    expect(chrome.tabs.remove).not.toHaveBeenCalled();
  });

  it("leaves its inactive tab open after fetch", async () => {
    chrome.tabs.query.mockResolvedValue([]);
    await request("fetch", "/api/organizations");
    expect(chrome.tabs.create).toHaveBeenCalledWith({ url: "https://claude.ai/new", active: false });
    expect(chrome.tabs.remove).not.toHaveBeenCalled();
  });

  it("waits for the Claude tab to load before injecting the reader", async () => {
    chrome.tabs.get.mockResolvedValue({ id: 7, status: "loading" });
    const pending = request("fetch", "/api/organizations");
    await vi.waitFor(() => expect(chrome.tabs.onUpdated.addListener).toHaveBeenCalledOnce());
    expect(chrome.scripting.executeScript).not.toHaveBeenCalled();
    const updated = chrome.tabs.onUpdated.addListener.mock.calls[0]![0];
    updated(9, { status: "complete" });
    expect(chrome.scripting.executeScript).not.toHaveBeenCalled();
    updated(7, { status: "complete" });
    expect(await pending).toEqual({ result: { status: 200, body: "chats" } });
    expect(chrome.tabs.onUpdated.removeListener).toHaveBeenCalledWith(updated);
  });
});
