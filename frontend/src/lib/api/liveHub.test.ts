import { afterEach, beforeEach, describe, expect, it, vi } from "vite-plus/test";
import { EventSource } from "eventsource";
import { LiveHub, type LiveFrame, type LiveRequest } from "./liveHub.js";

vi.mock("eventsource", () => ({ EventSource: vi.fn() }));

class FakeSource extends EventTarget {
  closed = false;
  readyState = 0;
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
    session,
  });
  beforeEach(() => {
    vi.useFakeTimers();
    sources = [];
    vi.mocked(EventSource).mockImplementation(function (url) {
      const source = new FakeSource(new URL(url));
      sources.push(source);
      return source as unknown as EventSource;
    });
  });
  afterEach(() => {
    vi.clearAllTimers();
    vi.useRealTimers();
  });

  it("replaces the stream for joins and routes real readiness and updates", () => {
    const hub = new LiveHub();
    const archive: LiveFrame[] = [];
    const a: LiveFrame[] = [];
    const b: LiveFrame[] = [];
    const archivePort = { postMessage: (frame: LiveFrame) => archive.push(frame) };
    const aPort = { postMessage: (frame: LiveFrame) => a.push(frame) };
    const bPort = { postMessage: (frame: LiveFrame) => b.push(frame) };
    hub.update(archivePort, request());
    const old = sources[0]!;
    hub.update(aPort, request("a"));
    hub.update(bPort, request("b"));
    expect(sources).toHaveLength(1);
    vi.advanceTimersByTime(250);
    expect(old.closed).toBe(true);
    const next = sources[1]!;
    expect(next.url.searchParams.getAll("session")).toEqual(["a", "b"]);
    expect(a).toEqual([]);
    old.emit("session_ready", "a");
    next.emit("session_ready", "a");
    next.emit("session.timing", '{"session_id":"a","turn_count":2}');
    next.emit("session_ready", "b");
    next.emit("events_ready", "{}");
    next.emit("session_updated", "a");
    next.emit("data_changed", '{"scope":"messages"}');
    expect(a.map((frame) => frame.type)).toEqual([
      "session_ready",
      "session.timing",
      "session_updated",
    ]);
    expect(b.map((frame) => frame.type)).toEqual(["session_ready"]);
    expect(archive.map((frame) => frame.type)).toEqual(["events_ready", "data_changed"]);
  });

  it("keeps a stream for a duplicate session leaving and shrinks it when the last leaves", () => {
    const hub = new LiveHub();
    const first = { postMessage: vi.fn() };
    const duplicate = { postMessage: vi.fn() };
    const archive = { postMessage: vi.fn() };
    hub.update(first, request("a"));
    hub.update(duplicate, request("a"));
    hub.update(archive, request());
    vi.advanceTimersByTime(250);
    const active = sources[0]!;
    hub.leave(duplicate);
    vi.advanceTimersByTime(250);
    expect(active.closed).toBe(false);
    expect(sources).toHaveLength(1);
    hub.leave(first);
    vi.advanceTimersByTime(250);
    expect(active.closed).toBe(true);
    expect(sources[1]!.url.searchParams.has("session")).toBe(false);
    hub.leave(archive);
    vi.advanceTimersByTime(250);
    expect(sources[1]!.closed).toBe(true);
  });

  it("sends the latest readiness and timing only to a duplicate session joining", () => {
    const hub = new LiveHub();
    const first = { postMessage: vi.fn() };
    const joining = { postMessage: vi.fn() };
    hub.update(first, request("a"));
    sources[0]!.emit("session_ready", "a");
    sources[0]!.emit("session.timing", '{"session_id":"a","turn_count":2}');
    first.postMessage.mockClear();
    hub.update(joining, request("a"));
    vi.advanceTimersByTime(250);
    expect(sources).toHaveLength(1);
    expect(first.postMessage).not.toHaveBeenCalled();
    expect(joining.postMessage.mock.calls).toEqual([
      [{ type: "session_ready", data: "a" }],
      [{ type: "session.timing", data: '{"session_id":"a","turn_count":2}' }],
    ]);
    sources[0]!.emit("session_missing", "a");
    const missing = { postMessage: vi.fn() };
    hub.update(missing, request("a"));
    vi.advanceTimersByTime(250);
    expect(sources).toHaveLength(1);
    expect(missing.postMessage.mock.calls).toEqual([[{ type: "session_missing", data: "a" }]]);
  });

  it("retries when EventSource closes on a temporary HTTP failure", () => {
    const hub = new LiveHub();
    const port = { postMessage: vi.fn() };
    hub.update(port, request());
    sources[0]!.readyState = 2;
    sources[0]!.emit("error");
    expect(port.postMessage).toHaveBeenLastCalledWith({ type: "closed" });
    vi.advanceTimersByTime(1000);
    expect(sources).toHaveLength(2);
    sources[1]!.emit("open");
    sources[1]!.emit("events_ready", "{}");
    expect(port.postMessage).toHaveBeenLastCalledWith({ type: "events_ready", data: "{}" });
    expect(
      port.postMessage.mock.calls.filter(([frame]) => frame.type === "events_ready"),
    ).toHaveLength(1);
    hub.leave(port);
    vi.advanceTimersByTime(250);
    expect(sources[1]!.closed).toBe(true);
  });

  it("closes after repeated errors and signals permanent failure", () => {
    const hub = new LiveHub();
    const port = { postMessage: vi.fn() };
    hub.update(port, request());
    const source = sources[0]!;
    for (let i = 0; i < 5; i++) source.emit("error");
    expect(source.closed).toBe(true);
    expect(port.postMessage).toHaveBeenLastCalledWith({ type: "permanent_failure" });
  });
});
