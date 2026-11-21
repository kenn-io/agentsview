import { afterEach, beforeEach, describe, expect, it, vi } from "vite-plus/test";
import { EventSource } from "eventsource";
import { LiveHub, type LiveFrame, type LiveRequest } from "./liveHub.js";

vi.mock("eventsource", () => ({ EventSource: vi.fn() }));

class FakeSource extends EventTarget {
  closed = false;
  readyState = 0;
  constructor(
    public url: URL,
    public options: { fetch: typeof fetch },
  ) {
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
    credentialTime: 0,
    session,
  });
  beforeEach(() => {
    vi.useFakeTimers();
    sources = [];
    vi.mocked(EventSource).mockImplementation(function (url, options) {
      const source = new FakeSource(new URL(url), options as { fetch: typeof fetch });
      sources.push(source);
      return source as unknown as EventSource;
    });
  });
  afterEach(() => {
    vi.unstubAllGlobals();
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
    old.emit("events_ready", "{}");
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
    hub.leave(aPort);
    hub.leave(bPort);
    vi.advanceTimersByTime(250);
    sources[2]!.emit("events_ready", "{}");
    expect(archive.map((frame) => frame.type)).toEqual(["events_ready", "data_changed"]);
    hub.close();
  });

  it("sends readiness again to archive subscribers when the same stream reconnects", () => {
    const hub = new LiveHub();
    const archive = { postMessage: vi.fn() };
    hub.update(archive, request());
    sources[0]!.emit("events_ready", "{}");
    sources[0]!.emit("error");
    sources[0]!.emit("events_ready", "{}");
    expect(archive.postMessage.mock.calls.map(([frame]) => frame.type)).toEqual([
      "events_ready",
      "error",
      "events_ready",
    ]);
    hub.close();
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

  it.each(["old subscriber leaves", "subscriptions arrive out of order"])(
    "keeps the newest credentials when %s",
    async (scenario) => {
      const fetch = vi.fn().mockResolvedValue(new Response(null, { status: 200 }));
      vi.stubGlobal("fetch", fetch);
      const hub = new LiveHub();
      const old = { postMessage: vi.fn() };
      const current = { postMessage: vi.fn() };
      const oldRequest = { ...request("a"), token: "old-token", credentialTime: 1 };
      const currentRequest = { ...request("a"), token: "new-token", credentialTime: 2 };
      if (scenario === "old subscriber leaves") {
        hub.update(old, oldRequest);
        hub.update(current, currentRequest);
      } else {
        hub.update(current, currentRequest);
        hub.update(old, oldRequest);
      }
      vi.advanceTimersByTime(250);
      const expectedSources = scenario === "old subscriber leaves" ? 2 : 1;
      expect(sources).toHaveLength(expectedSources);
      if (scenario === "old subscriber leaves") {
        expect(sources[0]!.closed).toBe(true);
        await sources[1]!.options.fetch(sources[1]!.url, {});
        expect((fetch.mock.calls[0]![1].headers as Headers).get("Authorization")).toBe(
          "Bearer new-token",
        );
        hub.leave(old);
        vi.advanceTimersByTime(250);
        expect(sources).toHaveLength(expectedSources);
      }
      const active = sources[expectedSources - 1]!;
      await active.options.fetch(active.url, {});
      expect(fetch.mock.calls.at(-1)![0]).toBe("http://example.test/api/v1/events?session=a");
      expect((fetch.mock.calls.at(-1)![1].headers as Headers).get("Authorization")).toBe(
        "Bearer new-token",
      );
      hub.close();
    },
  );

  it("encodes session IDs and keeps tokens out of the stream URL", () => {
    const hub = new LiveHub();
    const port = { postMessage: vi.fn() };
    hub.update(port, { ...request("child%7E/%25?#"), token: "a b&c?d=e/f+g" });
    expect(sources[0]!.url.href).toBe(
      "http://example.test/api/v1/events?session=child%257E%2F%2525%3F%23",
    );
    hub.close();
  });

  it("gives a replacement source a fresh error budget", () => {
    const hub = new LiveHub();
    const first = { postMessage: vi.fn() };
    const second = { postMessage: vi.fn() };
    hub.update(first, request());
    for (let i = 0; i < 5; i++) sources[0]!.emit("error");
    expect(sources[0]!.closed).toBe(true);
    expect(first.postMessage).toHaveBeenLastCalledWith({ type: "permanent_failure" });
    hub.update(second, request("a"));
    sources[1]!.emit("error");
    expect(sources[1]!.closed).toBe(false);
    expect(second.postMessage).toHaveBeenLastCalledWith({ type: "error" });
    hub.close();
  });

  it.each(["open", "data_changed", "session_updated"])("resets the error budget on %s", (type) => {
    const hub = new LiveHub();
    const port = { postMessage: vi.fn() };
    hub.update(port, request("a"));
    const source = sources[0]!;
    if (type !== "open") source.emit("open");
    for (let i = 0; i < 4; i++) source.emit("error");
    source.emit(type, type === "session_updated" ? "a" : "{}");
    for (let i = 0; i < 4; i++) source.emit("error");
    expect(source.closed).toBe(false);
    source.emit("error");
    expect(source.closed).toBe(true);
    expect(port.postMessage).toHaveBeenLastCalledWith({ type: "closed" });
    hub.close();
  });
});
