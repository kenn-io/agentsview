import { afterEach, beforeEach, expect, it, vi } from "vite-plus/test";
import type { LiveFrame, LivePort, LiveRequest } from "./liveHub.js";

const hubs = vi.hoisted(
  () =>
    [] as {
      batch: ReturnType<typeof vi.fn>;
      update: ReturnType<typeof vi.fn>;
      leave: ReturnType<typeof vi.fn>;
      acknowledge: ReturnType<typeof vi.fn>;
      close: ReturnType<typeof vi.fn>;
    }[],
);
vi.mock("./liveHub.js", () => ({
  LiveHub: class {
    batch = vi.fn((fn: () => void) => fn());
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

class FakeChannel {
  static channels = new Set<FakeChannel>();
  static deliverImmediately = true;
  static pending: Array<() => void> = [];
  onmessage: ((event: MessageEvent) => void) | null = null;
  constructor(public name: string) {
    FakeChannel.channels.add(this);
  }
  postMessage(data: unknown) {
    const cloned = structuredClone(data);
    for (const peer of FakeChannel.channels) {
      if (peer === this || peer.name !== this.name) continue;
      const deliver = () => peer.onmessage?.(new MessageEvent("message", { data: cloned }));
      if (FakeChannel.deliverImmediately) deliver();
      else FakeChannel.pending.push(deliver);
    }
  }
  static flush() {
    while (FakeChannel.pending.length > 0) {
      const next = FakeChannel.pending.shift()!;
      next();
    }
  }
  static reset() {
    FakeChannel.channels.clear();
    FakeChannel.pending = [];
    FakeChannel.deliverImmediately = true;
  }
}

class FakeLockManager {
  private queues = new Map<
    string,
    Array<{ callback: () => unknown; resolve: (v: unknown) => void; reject: (e: unknown) => void }>
  >();
  private held = new Set<string>();
  private forceRelease = new Map<string, () => void>();

  request(name: string, callback: () => unknown): Promise<unknown> {
    return new Promise((resolve, reject) => {
      const queue = this.queues.get(name) ?? [];
      queue.push({ callback, resolve, reject });
      this.queues.set(name, queue);
      this.pump(name);
    });
  }

  /** Simulates abrupt tab teardown releasing the currently held lock. */
  dropCurrent(name: string) {
    this.forceRelease.get(name)?.();
  }

  private pump(name: string) {
    if (this.held.has(name)) return;
    const queue = this.queues.get(name);
    const next = queue?.shift();
    if (!next) return;
    this.held.add(name);
    let released = false;
    const finish = () => {
      if (released) return;
      released = true;
      this.held.delete(name);
      this.forceRelease.delete(name);
      this.pump(name);
    };
    this.forceRelease.set(name, finish);
    Promise.resolve()
      .then(() => next.callback())
      .then(
        (value) => {
          finish();
          next.resolve(value);
        },
        (error) => {
          finish();
          next.reject(error);
        },
      );
  }
}

const request = (session?: string): LiveRequest => ({
  base: "http://example.test",
  token: "",
  credentialTime: 0,
  session,
});

beforeEach(() => {
  vi.useFakeTimers();
  FakeChannel.reset();
  vi.stubGlobal("BroadcastChannel", FakeChannel);
});

afterEach(() => {
  vi.clearAllTimers();
  vi.useRealTimers();
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
  FakeChannel.reset();
  hubs.length = 0;
});

it("routes a remote refresh receipt to the leader's matching port", () => {
  vi.stubGlobal("navigator", {});
  vi.spyOn(Math, "random").mockReturnValueOnce(0.1).mockReturnValueOnce(0.9);
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
  sendA(request("a"));
  sendB(request("b"));
  vi.advanceTimersByTime(250);
  const hub = hubs.find((candidate) => candidate.update.mock.calls.length === 2)!;
  expect(hub).toBeDefined();
  const [port] = hub.update.mock.calls.find(
    ([, value]) => (value as LiveRequest).session === "b",
  ) as [LivePort, LiveRequest];
  port.postMessage({ type: "session_updated", data: "b", receipt: "receipt" });
  vi.runAllTicks();
  expect(hub.acknowledge).toHaveBeenCalledWith(port, "receipt");
  sendA("close");
  sendB("close");
});

it("prevents split-brain leader hubs with Web Locks even when BroadcastChannel is delayed", async () => {
  const locks = new FakeLockManager();
  vi.stubGlobal("navigator", { locks });

  const leader = new LiveBroadcast("http://example.test");
  const sendLeader = leader.connect(() => {});
  sendLeader(request("s1"));
  await Promise.resolve();
  await Promise.resolve();

  expect(hubs).toHaveLength(1);
  expect(hubs[0]!.update).toHaveBeenCalledWith(
    expect.any(Object),
    expect.objectContaining({ session: "s1" }),
  );

  // Simulate slow page boot delaying BroadcastChannel delivery past the 200ms election window.
  FakeChannel.deliverImmediately = false;
  const followerFrames: LiveFrame[] = [];
  const follower = new LiveBroadcast("http://example.test");
  const sendFollower = follower.connect((frame) => {
    followerFrames.push(frame);
    if (frame.receipt) sendFollower({ type: "ack", receipt: frame.receipt });
  });
  sendFollower(request("s2"));
  vi.advanceTimersByTime(500);
  await Promise.resolve();

  // The follower never creates its own LiveHub while the leader holds the lock.
  expect(hubs).toHaveLength(1);

  // Once BroadcastChannel messages flush, the leader adds the follower's port and receives acks.
  FakeChannel.deliverImmediately = true;
  FakeChannel.flush();
  const followerCall = hubs[0]!.update.mock.calls.find(
    ([, value]) => (value as LiveRequest).session === "s2",
  ) as [LivePort, LiveRequest] | undefined;
  expect(followerCall).toBeDefined();
  followerCall![0].postMessage({ type: "session_ready", data: "s2", receipt: "r1" });
  expect(followerFrames).toEqual([{ type: "session_ready", data: "s2", receipt: "r1" }]);
  expect(hubs[0]!.acknowledge).toHaveBeenCalledWith(followerCall![0], "r1");

  // Closing the leader releases the Web Lock so the follower immediately takes over in one batch.
  locks.dropCurrent("agentsview-live:http://example.test");
  await Promise.resolve();
  await Promise.resolve();
  expect(hubs).toHaveLength(2);
  expect(hubs[1]!.batch).toHaveBeenCalledOnce();
  expect(hubs[1]!.update.mock.calls.map(([, value]) => (value as LiveRequest).session)).toEqual([
    "s1",
    "s2",
  ]);
});

it("keeps leadership across brief hide/show and yields after staying idle", async () => {
  const locks = new FakeLockManager();
  vi.stubGlobal("navigator", { locks });

  const leader = new LiveBroadcast("http://example.test");
  const sendLeader = leader.connect(() => {});
  sendLeader(request("s1"));
  await Promise.resolve();
  await Promise.resolve();
  expect(hubs).toHaveLength(1);

  const follower = new LiveBroadcast("http://example.test");
  const sendFollower = follower.connect(() => {});
  sendFollower(request("s1"));
  await Promise.resolve();

  // Momentary leave + rejoin keeps the lock without closing the hub.
  sendLeader("leave");
  vi.advanceTimersByTime(50);
  sendLeader(request("s1"));
  vi.advanceTimersByTime(1000);
  expect(hubs).toHaveLength(1);
  expect(hubs[0]!.close).not.toHaveBeenCalled();

  // Staying idle past the grace period releases the lock so the follower takes over.
  sendLeader("leave");
  await vi.advanceTimersByTimeAsync(1000);
  expect(hubs[0]!.close).toHaveBeenCalledOnce();
  expect(hubs).toHaveLength(2);
});

it("keeps the new lock holder's hub when a stale frame from the previous leader arrives", async () => {
  const locks = new FakeLockManager();
  vi.stubGlobal("navigator", { locks });
  vi.setSystemTime(new Date("2026-01-01T00:00:01.000Z"));
  const leader = new LiveBroadcast("http://example.test");
  const sendLeader = leader.connect(() => {});
  sendLeader(request("s1"));
  await Promise.resolve();
  await Promise.resolve();

  vi.setSystemTime(new Date("2026-01-01T00:00:02.000Z"));
  const followerFrames: LiveFrame[] = [];
  const follower = new LiveBroadcast("http://example.test");
  const sendFollower = follower.connect((frame) => followerFrames.push(frame));
  sendFollower(request("s2"));
  await Promise.resolve();
  expect(hubs).toHaveLength(1);
  const [port] = hubs[0]!.update.mock.calls.find(
    ([, value]) => (value as LiveRequest).session === "s2",
  ) as [LivePort, LiveRequest];

  // The old leader's frame is still queued when its lock passes to the follower.
  FakeChannel.deliverImmediately = false;
  port.postMessage({ type: "session_updated", data: "s2" });
  locks.dropCurrent("agentsview-live:http://example.test");
  await Promise.resolve();
  await Promise.resolve();
  expect(hubs).toHaveLength(2);

  FakeChannel.deliverImmediately = true;
  FakeChannel.flush();
  expect(hubs[1]!.close).not.toHaveBeenCalled();
  expect(followerFrames).toEqual([]);
});

it("steps down immediately in BroadcastChannel fallback when a superior leader appears", () => {
  vi.stubGlobal("navigator", {});
  vi.setSystemTime(new Date("2026-01-01T00:00:01.000Z"));
  const leader = new LiveBroadcast("http://example.test");
  const sendLeader = leader.connect(() => {});
  sendLeader(request("s1"));
  vi.advanceTimersByTime(200);
  expect(hubs).toHaveLength(1);

  // Isolate the follower so it briefly self-elects without hearing the leader.
  FakeChannel.deliverImmediately = false;
  vi.setSystemTime(new Date("2026-01-01T00:00:02.000Z"));
  const follower = new LiveBroadcast("http://example.test");
  const sendFollower = follower.connect(() => {});
  sendFollower(request("s2"));
  vi.advanceTimersByTime(200);
  expect(hubs).toHaveLength(2);
  expect(hubs[1]!.close).not.toHaveBeenCalled();

  // As soon as the leader's state reaches the follower, it steps down without waiting 200ms.
  FakeChannel.deliverImmediately = true;
  FakeChannel.flush();
  expect(hubs[1]!.close).toHaveBeenCalledOnce();
});

it("falls back to BroadcastChannel election if navigator.locks.request rejects", async () => {
  vi.stubGlobal("navigator", {
    locks: {
      request: () => Promise.reject(new DOMException("blocked", "SecurityError")),
    },
  });
  const tab = new LiveBroadcast("http://example.test");
  const send = tab.connect(() => {});
  send(request("s1"));
  await Promise.resolve();
  await Promise.resolve();
  vi.advanceTimersByTime(200);
  expect(hubs).toHaveLength(1);
});
