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
    this.readyState = 2;
  }
  emit(type: string, data = "", lastEventId = "") {
    this.readyState = type === "error" ? 0 : 1;
    this.dispatchEvent(new MessageEvent(type, { data, lastEventId }));
  }
}

const marker1 = `v1:${"1".repeat(64)}`;
const marker2 = `v1:${"2".repeat(64)}`;

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

  it("retries readiness when the latest child refresh failed", () => {
    const hub = new LiveHub();
    const a = { postMessage: vi.fn() };
    hub.update(a, request("a"));
    const old = sources[0]!;
    old.emit("session_ready", "a", `v1:${"a".repeat(64)}`);
    hub.acknowledge(
      a,
      a.postMessage.mock.calls.findLast(([frame]) => frame.receipt !== undefined)![0].receipt,
    );
    old.emit("events_ready", "{}");
    old.emit("session_updated", "a", `v1:${"a".repeat(64)}`);
    // The child read failed, so the update is not acknowledged.
    a.postMessage.mockClear();
    hub.update({ postMessage: vi.fn() }, request("b"));
    vi.advanceTimersByTime(250);
    sources[1]!.emit("session_ready", "a", `v1:${"a".repeat(64)}`);
    expect(a.postMessage).toHaveBeenCalledWith(expect.objectContaining({ type: "session_ready" }));
    hub.close();
  });

  it("ignores an acknowledgment for an overtaken refresh", () => {
    const hub = new LiveHub();
    const a = { postMessage: vi.fn() };
    hub.update(a, request("a"));
    const old = sources[0]!;
    old.emit("session_ready", "a", marker1);
    const receipt = a.postMessage.mock.lastCall![0].receipt;
    old.emit("events_ready", "{}");
    old.emit("session_updated", "a", marker2);
    hub.acknowledge(a, receipt);
    a.postMessage.mockClear();
    hub.update({ postMessage: vi.fn() }, request("b"));
    vi.advanceTimersByTime(250);
    sources[1]!.emit("session_ready", "a", marker1);
    expect(a.postMessage).toHaveBeenCalledWith(expect.objectContaining({ type: "session_ready" }));
    hub.close();
  });

  it("suppresses startup frames for three applied sessions during a handoff", () => {
    const hub = new LiveHub();
    const ports = ["a", "b", "c"].map((id) => ({ id, port: { postMessage: vi.fn() } }));
    for (const { id, port } of ports) hub.update(port, request(id));
    vi.advanceTimersByTime(250);
    const old = sources.at(-1)!;
    for (const { id, port } of ports) {
      old.emit("session_ready", id, marker1);
      hub.acknowledge(port, port.postMessage.mock.lastCall![0].receipt);
      old.emit("session.timing", JSON.stringify({ session_id: id }));
    }
    old.emit("events_ready", "{}");
    for (const { port } of ports) port.postMessage.mockClear();
    const changed = { postMessage: vi.fn() };
    hub.update(changed, request("d"));
    vi.advanceTimersByTime(250);
    const next = sources.at(-1)!;
    for (const { id } of ports) {
      next.emit("session_ready", id, marker1);
      next.emit("session.timing", JSON.stringify({ session_id: id }));
    }
    next.emit("session_ready", "d", marker2);
    next.emit("session.timing", '{"session_id":"d"}');
    next.emit("events_ready", "{}");
    for (const { port } of ports) expect(port.postMessage).not.toHaveBeenCalled();
    expect(changed.postMessage.mock.calls.map(([frame]) => frame.type)).toEqual([
      "session_ready",
      "session.timing",
    ]);
    hub.close();
  });

  it("delivers changed timing during an otherwise quiet handoff", () => {
    const hub = new LiveHub();
    const a = { postMessage: vi.fn() };
    hub.update(a, request("a"));
    const old = sources[0]!;
    old.emit("session_ready", "a", marker1);
    hub.acknowledge(a, a.postMessage.mock.lastCall![0].receipt);
    old.emit("session.timing", '{"session_id":"a","total_duration_ms":1000,"running":true}');
    old.emit("events_ready", "{}");
    a.postMessage.mockClear();
    hub.update({ postMessage: vi.fn() }, request("b"));
    vi.advanceTimersByTime(250);
    sources[1]!.emit("session_ready", "a", marker1);
    sources[1]!.emit(
      "session.timing",
      '{"session_id":"a","total_duration_ms":61000,"running":true}',
    );
    expect(a.postMessage.mock.calls.map(([frame]) => frame.type)).toEqual(["session.timing"]);
    expect(a.postMessage.mock.lastCall![0].data).toContain("61000");
    hub.close();
  });

  it("keeps archive updates flowing while changing watched sessions", () => {
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
    const duplicateArchive = { postMessage: vi.fn() };
    hub.update(duplicateArchive, request());
    expect(duplicateArchive.postMessage).toHaveBeenCalledOnce();
    expect(archive.map((frame) => frame.type)).toEqual(["events_ready"]);
    hub.leave(duplicateArchive);
    hub.update(aPort, request("a"));
    hub.update(bPort, request("b"));
    expect(sources).toHaveLength(1);
    vi.advanceTimersByTime(250);
    expect(old.closed).toBe(false);
    const next = sources[1]!;
    expect(next.url.searchParams.getAll("session")).toEqual(["a", "b"]);
    expect(a).toEqual([]);
    old.emit("data_changed", '{"scope":"sessions"}');
    next.emit("session_ready", "a");
    expect(old.closed).toBe(true);
    old.emit("data_changed", '{"scope":"sessions"}');
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
    expect(archive.map((frame) => frame.type)).toEqual([
      "events_ready",
      "data_changed",
      "data_changed",
    ]);
    hub.leave(aPort);
    hub.leave(bPort);
    vi.advanceTimersByTime(250);
    expect(next.closed).toBe(false);
    next.emit("data_changed", '{"scope":"sessions"}');
    sources[2]!.emit("events_ready", "{}");
    expect(next.closed).toBe(true);
    expect(archive.map((frame) => frame.type)).toEqual([
      "events_ready",
      "data_changed",
      "data_changed",
      "data_changed",
    ]);
    hub.close();
  });

  it.each([
    { name: "unchanged", old: marker1, replacement: marker1, ready: false },
    { name: "changed", old: marker1, replacement: marker2, ready: true },
    { name: "unknown", old: "", replacement: "", ready: true },
    {
      name: "updated before handoff",
      old: marker2,
      replacement: marker2,
      ready: false,
      update: true,
    },
    {
      name: "missing before handoff",
      old: marker1,
      replacement: marker1,
      ready: true,
      missing: true,
    },
    {
      name: "disconnected",
      old: marker1,
      replacement: marker1,
      ready: true,
      disconnected: true,
    },
    {
      name: "replacement errors",
      old: marker1,
      replacement: marker1,
      ready: true,
      replacementError: true,
    },
  ])("compares readiness at handoff: $name", (scenario) => {
    const hub = new LiveHub();
    const archive = { postMessage: vi.fn() };
    const a = { postMessage: vi.fn() };
    const b = { postMessage: vi.fn() };
    hub.update(a, request("a"));
    hub.update(archive, request());
    const old = sources[0]!;
    old.emit("session_ready", "a", scenario.update ? marker1 : scenario.old);
    old.emit("session.timing", '{"session_id":"a","turn_count":1}');
    old.emit("events_ready", "{}");
    if (scenario.update) old.emit("session_updated", "a", scenario.old);
    hub.acknowledge(
      a,
      a.postMessage.mock.calls.findLast(([frame]) => frame.receipt !== undefined)![0].receipt,
    );
    if (scenario.missing) old.emit("session_missing", "a");
    if (scenario.disconnected) old.emit("error");
    a.postMessage.mockClear();
    archive.postMessage.mockClear();
    hub.update(b, request("b"));
    vi.advanceTimersByTime(250);
    const next = sources[1]!;
    if (scenario.replacementError) next.emit("error");
    next.emit("session_ready", "a", scenario.replacement);
    next.emit("session.timing", '{"session_id":"a","turn_count":1}');
    next.emit("session_ready", "b", "1:b");
    next.emit("session.timing", '{"session_id":"b"}');
    next.emit("events_ready", "{}");
    expect(a.postMessage.mock.calls.map(([frame]) => frame.type)).toEqual([
      ...(scenario.ready ? ["session_ready", "session.timing"] : []),
    ]);
    expect(b.postMessage.mock.calls.map(([frame]) => frame.type)).toEqual([
      "session_ready",
      "session.timing",
    ]);
    if (!scenario.disconnected) expect(archive.postMessage).not.toHaveBeenCalled();
    next.emit("session_updated", "a", "3:z");
    expect(a.postMessage).toHaveBeenLastCalledWith(
      expect.objectContaining({
        type: "session_updated",
        data: "a",
        id: "3:z",
      }),
    );
    hub.close();
  });

  it("gives a joining tab the latest readiness marker after an update", () => {
    const hub = new LiveHub();
    const first = { postMessage: vi.fn() };
    const joining = { postMessage: vi.fn() };
    hub.update(first, request("a"));
    const old = sources[0]!;
    old.emit("session_ready", "a", marker1);
    old.emit("events_ready", "{}");
    old.emit("session_updated", "a", marker2);
    old.emit("session.timing", '{"session_id":"a","turn_count":1}');
    hub.update(joining, request("a"));
    expect(joining.postMessage).toHaveBeenCalledWith(
      expect.objectContaining({
        type: "session_ready",
        data: "a",
        id: marker2,
      }),
    );
    hub.acknowledge(
      joining,
      joining.postMessage.mock.calls.find(([frame]) => frame.receipt)![0].receipt,
    );
    joining.postMessage.mockClear();
    hub.update({ postMessage: vi.fn() }, request("b"));
    vi.advanceTimersByTime(250);
    sources[1]!.emit("session_ready", "a", marker2);
    sources[1]!.emit("session.timing", '{"session_id":"a","turn_count":1}');
    expect(joining.postMessage).not.toHaveBeenCalled();
    hub.close();
  });

  it.each(["before readiness", "between readiness and timing", "after leaving and rejoining"])(
    "delivers session state to a tab joining mid-handoff: %s",
    (when) => {
      const hub = new LiveHub();
      const a = { postMessage: vi.fn() };
      const joining = { postMessage: vi.fn() };
      hub.update(a, request("a"));
      hub.update({ postMessage: vi.fn() }, request("b"));
      vi.advanceTimersByTime(250);
      const old = sources[1]!;
      old.emit("session_ready", "a", marker1);
      old.emit("session.timing", '{"session_id":"a"}');
      old.emit("session_ready", "b", "1:b");
      old.emit("events_ready", "{}");
      if (when === "after leaving and rejoining") {
        hub.update(joining, request("a"));
        joining.postMessage.mockClear();
      }
      a.postMessage.mockClear();
      hub.update({ postMessage: vi.fn() }, request("c"));
      vi.advanceTimersByTime(250);
      const next = sources[2]!;
      next.emit("session_ready", "c", "1:c");
      if (when === "before readiness") hub.update(joining, request("a"));
      if (when === "after leaving and rejoining") {
        // Both leave and a new subscription clear the joining port's marker.
        hub.leave(joining);
        hub.update(joining, request("a"));
      }
      next.emit("session_ready", "a", marker1);
      if (when === "between readiness and timing") hub.update(joining, request("a"));
      next.emit("session.timing", '{"session_id":"a"}');
      next.emit("events_ready", "{}");
      expect(joining.postMessage.mock.calls).toEqual([
        [expect.objectContaining({ type: "session_ready", data: "a", id: marker1 })],
        [{ type: "session.timing", data: '{"session_id":"a"}' }],
      ]);
      hub.close();
    },
  );

  it("closes the old stream immediately when credentials change and refreshes on readiness", () => {
    const hub = new LiveHub();
    const archive = { postMessage: vi.fn() };
    hub.update(archive, { ...request(), token: "old-token", credentialTime: 1 });
    sources[0]!.emit("events_ready", "{}");
    hub.update(archive, { ...request(), token: "new-token", credentialTime: 2 });
    expect(sources[0]!.closed).toBe(true);
    sources[1]!.emit("events_ready", "{}");
    expect(archive.postMessage.mock.calls.map(([frame]) => frame.type)).toEqual([
      "events_ready",
      "events_ready",
    ]);
    hub.close();
  });

  it.each(["reconnect", "failure during handoff", "disconnected"])(
    "refreshes archives when continuity breaks: %s",
    (reason) => {
      const hub = new LiveHub();
      const archive = { postMessage: vi.fn() };
      hub.update(archive, request());
      sources[0]!.emit("events_ready", "{}");
      if (reason !== "failure during handoff") sources[0]!.emit("error");
      if (reason !== "reconnect") {
        hub.update({ postMessage: vi.fn() }, request("a"));
        vi.advanceTimersByTime(250);
        if (reason === "failure during handoff") sources[0]!.emit("error");
        expect(sources[0]!.closed).toBe(true);
        sources[1]!.emit("session_ready", "a");
      }
      sources[reason === "reconnect" ? 0 : 1]!.emit("events_ready", "{}");
      expect(archive.postMessage.mock.calls.map(([frame]) => frame.type)).toEqual([
        "events_ready",
        "error",
        "events_ready",
      ]);
      hub.close();
    },
  );

  it("keeps the old stream while a pending replacement is superseded", () => {
    const hub = new LiveHub();
    const archive = { postMessage: vi.fn() };
    const a = { postMessage: vi.fn() };
    const b = { postMessage: vi.fn() };
    hub.update(archive, request());
    sources[0]!.emit("events_ready", "{}");
    hub.update(a, request("a"));
    vi.advanceTimersByTime(250);
    hub.update(b, request("b"));
    vi.advanceTimersByTime(250);
    expect(sources[0]!.closed).toBe(false);
    expect(sources[1]!.closed).toBe(true);
    sources[0]!.emit("data_changed", '{"scope":"sessions"}');
    sources[2]!.emit("session_ready", "a");
    sources[2]!.emit("events_ready", "{}");
    expect(sources[0]!.closed).toBe(true);
    expect(archive.postMessage.mock.calls.map(([frame]) => frame.type)).toEqual([
      "events_ready",
      "data_changed",
    ]);
    hub.close();
  });

  it("recovers from a failed replacement after keeping the old stream during retries", () => {
    const hub = new LiveHub();
    const archive = { postMessage: vi.fn() };
    hub.update(archive, request());
    sources[0]!.emit("events_ready", "{}");
    hub.update({ postMessage: vi.fn() }, request("a"));
    vi.advanceTimersByTime(250);
    for (let i = 0; i < 4; i++) sources[1]!.emit("error");
    expect(sources[0]!.closed).toBe(false);
    sources[1]!.emit("error");
    expect(sources[0]!.closed).toBe(true);
    expect(archive.postMessage).toHaveBeenLastCalledWith({ type: "closed" });
    hub.update(archive, request());
    sources[2]!.emit("events_ready", "{}");
    expect(archive.postMessage).toHaveBeenLastCalledWith({ type: "events_ready", data: "{}" });
    hub.close();
  });

  it.each(["same credentials", "new credentials"])(
    "classifies replacement failure during reconnect with %s",
    (credentials) => {
      const hub = new LiveHub();
      const archive = { postMessage: vi.fn() };
      hub.update(archive, request());
      sources[0]!.emit("open");
      sources[0]!.emit("error");
      if (credentials === "same credentials") {
        hub.update({ postMessage: vi.fn() }, request("a"));
        vi.advanceTimersByTime(250);
      } else {
        hub.update(archive, { ...request("a"), token: "new-token", credentialTime: 1 });
      }
      expect(sources[0]!.closed).toBe(true);
      for (let i = 0; i < 5; i++) sources[1]!.emit("error");
      expect(archive.postMessage).toHaveBeenLastCalledWith({
        type: credentials === "same credentials" ? "closed" : "permanent_failure",
      });
      hub.close();
    },
  );

  it("reports permanent failure when a fresh stream never connects after recovery is exhausted", () => {
    const hub = new LiveHub();
    const archive = { postMessage: vi.fn() };
    const session = { postMessage: vi.fn() };
    hub.update(archive, request());
    sources[0]!.emit("open");
    sources[0]!.emit("error");
    hub.update(session, request("a"));
    vi.advanceTimersByTime(250);
    for (let i = 0; i < 5; i++) sources[1]!.emit("error");
    expect(archive.postMessage).toHaveBeenLastCalledWith({ type: "closed" });

    hub.leave(archive);
    const fresh = { postMessage: vi.fn() };
    hub.update(fresh, request());
    sources[2]!.emit("error");
    expect(sources[2]!.closed).toBe(false);
    expect(fresh.postMessage).toHaveBeenLastCalledWith({ type: "error" });
    for (let i = 0; i < 4; i++) sources[2]!.emit("error");
    expect(fresh.postMessage).toHaveBeenLastCalledWith({ type: "permanent_failure" });
    hub.close();
  });

  it("keeps a stream for a duplicate session leaving and shrinks it when the last leaves", () => {
    const hub = new LiveHub();
    const first = { postMessage: vi.fn() };
    const duplicate = { postMessage: vi.fn() };
    const archive = { postMessage: vi.fn() };
    hub.update(first, request("a"));
    sources[0]!.emit("session_ready", "a");
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
    expect(active.closed).toBe(false);
    sources[1]!.emit("events_ready", "{}");
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
      [expect.objectContaining({ type: "session_ready", data: "a", id: "" })],
      [{ type: "session.timing", data: '{"session_id":"a","turn_count":2}' }],
    ]);
  });

  it("rechecks a missing session when another subscriber joins", () => {
    const hub = new LiveHub();
    const first = { postMessage: vi.fn() };
    const joining = { postMessage: vi.fn() };
    hub.update(first, request("a"));
    sources[0]!.emit("session_missing", "a");
    hub.update(joining, request("a"));
    expect(joining.postMessage).not.toHaveBeenCalled();
    vi.advanceTimersByTime(250);
    expect(sources).toHaveLength(2);
    expect(sources[1]!.url.searchParams.getAll("session")).toEqual(["a"]);
    sources[1]!.emit("session_ready", "a");
    expect(first.postMessage).toHaveBeenLastCalledWith(
      expect.objectContaining({
        type: "session_ready",
        data: "a",
        id: "",
      }),
    );
    expect(joining.postMessage).toHaveBeenLastCalledWith(
      expect.objectContaining({
        type: "session_ready",
        data: "a",
        id: "",
      }),
    );
    hub.close();
  });

  it.each(["old first", "new first"])(
    "uses the newest credentials and strips reconnect headers with %s",
    async (order) => {
      const fetch = vi.fn().mockResolvedValue(new Response(null, { status: 200 }));
      vi.stubGlobal("fetch", fetch);
      const hub = new LiveHub();
      const old = { postMessage: vi.fn() };
      const current = { postMessage: vi.fn() };
      const oldRequest = { ...request("a"), token: "old-token", credentialTime: 1 };
      const newRequest = { ...request("a"), token: "new-token", credentialTime: 2 };
      if (order === "old first") {
        hub.update(old, oldRequest);
        hub.update(current, newRequest);
        expect(sources[0]!.closed).toBe(true);
        hub.leave(old);
      } else {
        hub.update(current, newRequest);
        hub.update(old, oldRequest);
      }
      vi.advanceTimersByTime(250);
      expect(sources).toHaveLength(order === "old first" ? 2 : 1);
      const source = sources.at(-1)!;
      await source.options.fetch(source.url, { headers: { "Last-Event-ID": marker1 } });
      expect((fetch.mock.calls[0]![1].headers as Headers).get("Authorization")).toBe(
        "Bearer new-token",
      );
      expect((fetch.mock.calls[0]![1].headers as Headers).has("Last-Event-ID")).toBe(false);
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

  it.each(["open", "session_updated"])("resets the error budget on %s", (type) => {
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
