import { readFileSync } from "node:fs";
import { afterEach, beforeEach, describe, expect, it, vi } from "vite-plus/test";

const script = readFileSync("chrome-extension/bridge.js", "utf8");
const sendMessage = vi.fn();
let listener: EventListener;

beforeEach(() => {
  sendMessage.mockReset().mockResolvedValue({ result: { status: 200, body: "chats" } });
  vi.stubGlobal("chrome", { runtime: { sendMessage } });
  const add = vi.spyOn(window, "addEventListener");
  window.eval(script);
  listener = add.mock.calls.find(([type]) => type === "message")![1] as EventListener;
});
afterEach(() => {
  window.removeEventListener("message", listener);
  delete document.documentElement.dataset.agentsviewClaudeHost;
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
});

describe("Chrome page bridge", () => {
  it("marks the host and relays a same-window request and reply", async () => {
    const post = vi.spyOn(window, "postMessage").mockImplementation(() => {});
    window.dispatchEvent(new MessageEvent("message", {
      source: window, origin: location.origin,
      data: { type: "agentsview-claude-request", id: "a", method: "fetch", path: "/api/organizations" },
    }));
    await vi.waitFor(() => expect(post).toHaveBeenCalledWith({ type: "agentsview-claude-reply", id: "a", result: { status: 200, body: "chats" } }, location.origin));
    expect(sendMessage).toHaveBeenCalledExactlyOnceWith({ id: "a", method: "fetch", path: "/api/organizations" });
    expect(document.documentElement.dataset.agentsviewClaudeHost).toBe("chrome");
  });

  it("refuses a foreign source", () => {
    window.dispatchEvent(new MessageEvent("message", {
      source: null,
      origin: location.origin,
      data: { type: "agentsview-claude-request", id: "a", method: "connect" },
    }));
    expect(sendMessage).not.toHaveBeenCalled();
  });
});
