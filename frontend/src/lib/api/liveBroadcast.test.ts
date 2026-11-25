import { afterEach, expect, it, vi } from "vite-plus/test";
import type { LiveFrame, LiveRequest } from "./liveHub.js";

const hubs = vi.hoisted(
  () =>
    [] as {
      update: ReturnType<typeof vi.fn>;
      leave: ReturnType<typeof vi.fn>;
      acknowledge: ReturnType<typeof vi.fn>;
      close: ReturnType<typeof vi.fn>;
    }[],
);
vi.mock("./liveHub.js", () => ({
  LiveHub: class {
    update = vi.fn();
    leave = vi.fn();
    acknowledge = vi.fn();
    close = vi.fn();
    constructor() {
      hubs.push(this);
    }
  },
}));

import { LiveBroadcast } from "./liveBroadcast.js";

afterEach(() => {
  vi.clearAllTimers();
  vi.useRealTimers();
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
  hubs.length = 0;
});

it("routes a remote refresh receipt to the leader's matching port", () => {
  vi.useFakeTimers();
  vi.spyOn(Math, "random").mockReturnValueOnce(0.1).mockReturnValueOnce(0.9);
  const channels: Channel[] = [];
  class Channel {
    onmessage: ((event: MessageEvent) => void) | null = null;
    constructor() {
      channels.push(this);
    }
    postMessage(data: unknown) {
      for (const channel of channels)
        if (channel !== this) channel.onmessage?.(new MessageEvent("message", { data }));
    }
  }
  vi.stubGlobal("BroadcastChannel", Channel);
  const a = new LiveBroadcast("http://example.test");
  const b = new LiveBroadcast("http://example.test");
  let sendA!: ReturnType<LiveBroadcast["connect"]>;
  let sendB!: ReturnType<LiveBroadcast["connect"]>;
  sendA = a.connect((frame: LiveFrame) => {
    if (frame.receipt) sendA({ type: "ack", receipt: frame.receipt });
  });
  sendB = b.connect((frame: LiveFrame) => {
    if (frame.receipt) sendB({ type: "ack", receipt: frame.receipt });
  });
  const request = (session: string): LiveRequest => ({
    base: "http://example.test",
    token: "",
    credentialTime: 0,
    session,
  });
  sendA(request("a"));
  sendB(request("b"));
  vi.advanceTimersByTime(250);
  const hub = hubs.find((candidate) => candidate.update.mock.calls.length === 2)!;
  expect(hub).toBeDefined();
  const [port] = hub.update.mock.calls.find(([, value]) => (value as LiveRequest).session === "b")!;
  port.postMessage({ type: "session_updated", data: "b", receipt: "receipt" });
  vi.runAllTicks();
  expect(hub.acknowledge).toHaveBeenCalledWith(port, "receipt");
  sendA("close");
  sendB("close");
});
