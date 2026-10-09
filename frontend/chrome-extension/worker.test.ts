import { afterEach, beforeEach, describe, expect, it, vi } from "vite-plus/test";

let message: (request: object, sender: object, respond: (result: unknown) => void) => boolean;
let click: (tab: object) => Promise<void>;
const chrome = {
  action: { onClicked: { addListener: vi.fn((callback) => { click = callback; }) } },
  runtime: { onMessage: { addListener: vi.fn((callback) => { message = callback; }) } },
  storage: { local: { get: vi.fn(), set: vi.fn() } },
  tabs: {
    query: vi.fn(), create: vi.fn(), get: vi.fn(), remove: vi.fn(),
    onUpdated: { addListener: vi.fn(), removeListener: vi.fn() },
    onRemoved: { addListener: vi.fn(), removeListener: vi.fn() },
  },
  scripting: { executeScript: vi.fn() },
};
const origin = "http://localhost:8090";
const sender = { origin, frameId: 0 };
function request(method: string, path?: string, from = sender) {
  return new Promise((resolve) => {
    expect(message({ id: "a", method, path }, from, resolve)).toBe(true);
  });
}
beforeEach(async () => {
  vi.resetModules();
  vi.resetAllMocks();
  vi.stubGlobal("chrome", chrome);
  chrome.storage.local.get.mockResolvedValue({ allowedOrigins: [origin] });
  chrome.tabs.query.mockResolvedValue([{ id: 7 }]);
  chrome.tabs.get.mockResolvedValue({ id: 7, status: "complete" });
  chrome.tabs.create.mockResolvedValue({ id: 8 });
  chrome.scripting.executeScript.mockResolvedValue([{ result: { status: 200, body: "chats" } }]);
  await import("./worker.js");
});
afterEach(() => vi.unstubAllGlobals());

describe("Chrome host worker", () => {
  it.each([
    { origin: "http://localhost:8091", frameId: 0 },
    { origin, frameId: 1 },
  ])("refuses requests without top-frame consent: %j", async (from) => {
    expect(await request("fetch", "/api/organizations", from)).toEqual({ error: expect.stringContaining("toolbar button") });
    expect(chrome.tabs.query).not.toHaveBeenCalled();
    expect(chrome.tabs.create).not.toHaveBeenCalled();
    expect(chrome.scripting.executeScript).not.toHaveBeenCalled();
  });

  it("refuses a disallowed path before touching tabs", async () => {
    expect(await request("fetch", "/api/settings")).toEqual({ error: "Error: Unsupported Claude fetch path" });
    expect(chrome.tabs.query).not.toHaveBeenCalled();
    expect(chrome.tabs.create).not.toHaveBeenCalled();
    expect(chrome.scripting.executeScript).not.toHaveBeenCalled();
  });

  it("stores consent for the clicked loopback origin", async () => {
    await click({ url: "http://127.0.0.1:8081/sessions" });
    expect(chrome.storage.local.set).toHaveBeenCalledWith({ allowedOrigins: [origin, "http://127.0.0.1:8081"] });
    chrome.storage.local.set.mockClear();
    await click({ url: "https://example.com" });
    expect(chrome.storage.local.set).not.toHaveBeenCalled();
  });

  it("opens sign-in in Chrome", async () => {
    await request("connect");
    expect(chrome.tabs.create).toHaveBeenCalledExactlyOnceWith({ url: "https://claude.ai/login?return_url=%2Fnew" });
  });

  it("fetches in an existing Claude tab and leaves it open", async () => {
    expect(await request("fetch", "/api/organizations")).toEqual({ result: { status: 200, body: "chats" } });
    expect(chrome.tabs.query).toHaveBeenCalledWith({ url: "https://claude.ai/*" });
    expect(chrome.scripting.executeScript).toHaveBeenNthCalledWith(1, { target: { tabId: 7 }, files: ["claude_fetch.js"], world: "ISOLATED" });
    expect(chrome.scripting.executeScript).toHaveBeenNthCalledWith(2, { target: { tabId: 7 }, world: "ISOLATED", func: expect.any(Function), args: ["https://claude.ai/api/organizations"] });
    await request("close");
    expect(chrome.tabs.remove).not.toHaveBeenCalled();
  });

  it("closes only its own inactive tab", async () => {
    chrome.tabs.query.mockResolvedValue([]);
    await request("fetch", "/api/organizations");
    expect(chrome.tabs.create).toHaveBeenCalledWith({ url: "https://claude.ai/new", active: false });
    await request("close");
    await request("close");
    expect(chrome.tabs.remove).toHaveBeenCalledExactlyOnceWith(8);
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
