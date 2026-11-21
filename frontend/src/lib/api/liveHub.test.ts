import { beforeEach, describe, expect, it, vi } from "vite-plus/test";
import { EventSource } from "eventsource";
import { LiveHub, type LiveFrame, type LiveRequest } from "./liveHub.js";

vi.mock("eventsource", () => ({ EventSource: vi.fn() }));

class FakeSource extends EventTarget {
  closed = false;
  constructor(public url: URL) {
    super();
  }
  close() {
    this.closed = true;
  }
  emit(type: string, data = "") {
    this.dispatchEvent(new MessageEvent(type, { data }));
  }
}

describe("shared live stream", () => {
  let sources: FakeSource[];
  const request = (session?: string): LiveRequest => ({
    base: "http://example.test",
    token: "token",
    visible: true,
    session,
  });
  beforeEach(() => {
    sources = [];
    vi.mocked(EventSource).mockImplementation(function (url) {
      const source = new FakeSource(new URL(url));
      sources.push(source);
      return source as unknown as EventSource;
    });
  });

  it("shares a stream and routes updates to the matching session", () => {
    const hub = new LiveHub();
    const archive: LiveFrame[] = [];
    const a: LiveFrame[] = [];
    const b: LiveFrame[] = [];
    hub.update({ postMessage: (frame) => archive.push(frame) }, request());
    hub.update({ postMessage: (frame) => a.push(frame) }, request("a"));
    hub.update({ postMessage: (frame) => b.push(frame) }, request("b"));
    const source = sources.at(-1)!;
    expect(source.url.searchParams.getAll("session")).toEqual(["a", "b"]);
    source.emit("session_ready", "a");
    source.emit("session_ready", "b");
    source.emit("events_ready", "{}");
    source.emit("session_updated", "a");
    source.emit("data_changed", '{"scope":"messages"}');
    expect(a.map((frame) => frame.type)).toEqual(["session_ready", "session_updated"]);
    expect(b.map((frame) => frame.type)).toEqual(["session_ready"]);
    expect(archive.map((frame) => frame.type)).toEqual(["events_ready", "data_changed"]);
  });

  it("replays session snapshots when the new stream takes over", () => {
    const hub = new LiveHub();
    const a: LiveFrame[] = [];
    const b: LiveFrame[] = [];
    hub.update({ postMessage: (frame) => a.push(frame) }, request("a"));
    const old = sources[0]!;
    old.emit("session_ready", "a");
    old.emit("events_ready", "{}");
    hub.update({ postMessage: (frame) => b.push(frame) }, request("b"));
    const next = sources[1]!;
    next.emit("session_ready", "a");
    next.emit("session.timing", '{"session_id":"a","turn_count":2}');
    next.emit("session_updated", "a");
    next.emit("session_ready", "b");
    expect(old.closed).toBe(false);
    next.emit("events_ready", "{}");
    expect(old.closed).toBe(true);
    expect(a.map((frame) => frame.type)).toEqual([
      "session_ready",
      "session_ready",
      "session.timing",
      "session_updated",
    ]);
    expect(a[2]?.data).toBe('{"session_id":"a","turn_count":2}');
    expect(b.map((frame) => frame.type)).toEqual(["session_ready"]);
  });

  it("replays the latest timing when a hidden tab returns", () => {
    const hub = new LiveHub();
    const frames: LiveFrame[] = [];
    const port = { postMessage: (frame: LiveFrame) => frames.push(frame) };
    const other = { postMessage: vi.fn() };
    hub.update(port, request("a"));
    hub.update(other, request("a"));
    const source = sources.at(-1)!;
    source.emit("session_ready", "a");
    source.emit("events_ready", "{}");
    hub.update(port, { ...request("a"), visible: false });
    source.emit("session_updated", "a");
    source.emit("session.timing", '{"session_id":"a","turn_count":4}');
    hub.update(port, request("a"));
    expect(frames).toEqual([
      { type: "session_ready", data: "a" },
      { type: "session_ready", data: "a" },
      { type: "session.timing", data: '{"session_id":"a","turn_count":4}' },
    ]);
    hub.leave(port);
    hub.leave(other);
    const reopened: LiveFrame[] = [];
    const newPort = { postMessage: (frame: LiveFrame) => reopened.push(frame) };
    hub.update(newPort, request("a"));
    expect(reopened).toEqual([{ type: "session_ready", data: "a" }]);
    hub.leave(newPort);
  });

  it("does not send candidate errors to ports served by the current stream", () => {
    const hub = new LiveHub();
    const archive = { postMessage: vi.fn() };
    const a = { postMessage: vi.fn() };
    const b = { postMessage: vi.fn() };
    hub.update(archive, request());
    hub.update(a, request("a"));
    const old = sources.at(-1)!;
    old.emit("session_ready", "a");
    old.emit("events_ready", "{}");
    archive.postMessage.mockClear();
    a.postMessage.mockClear();
    hub.update(b, request("b"));
    const next = sources.at(-1)!;
    for (let i = 0; i < 5; i++) next.emit("error");
    expect(archive.postMessage).not.toHaveBeenCalled();
    expect(a.postMessage).not.toHaveBeenCalled();
    expect(b.postMessage).toHaveBeenLastCalledWith({ type: "permanent_failure" });
    expect(old.closed).toBe(false);
  });
  it("closes after repeated errors and signals a permanent failure", () => {
    const hub = new LiveHub();
    const frames: LiveFrame[] = [];
    const port = { postMessage: (frame: LiveFrame) => frames.push(frame) };
    hub.update(port, request());
    const source = sources[0]!;
    for (let i = 0; i < 5; i++) source.emit("error");
    expect(source.closed).toBe(true);
    expect(frames.at(-1)).toEqual({ type: "permanent_failure" });
    hub.leave(port);
  });

  it("stops sending updates after a port leaves", () => {
    const hub = new LiveHub();
    const frames: LiveFrame[] = [];
    const port = { postMessage: (frame: LiveFrame) => frames.push(frame) };
    hub.update(port, request("a"));
    const source = sources[0]!;
    source.emit("session_ready", "a");
    source.emit("events_ready", "{}");
    hub.leave(port);
    source.emit("session_updated", "a");
    expect(frames.map((frame) => frame.type)).toEqual(["session_ready"]);
  });
});
