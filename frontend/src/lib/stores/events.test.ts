import { beforeEach, describe, expect, it, vi } from "vite-plus/test";
import type { DataChangedEvent, LiveSource, WatchEventsOptions } from "../api/client.js";

const api = vi.hoisted(() => ({ watchEvents: vi.fn() }));
vi.mock("../api/client.js", () => api);

class FakeSource extends EventTarget implements LiveSource {
  readyState = 1;
  close = vi.fn(() => {
    this.readyState = 2;
  });
}

describe("events store", () => {
  let source: FakeSource;
  let deliver: (event: DataChangedEvent) => void;
  let options: WatchEventsOptions;
  beforeEach(() => {
    vi.resetModules();
    vi.clearAllMocks();
    vi.spyOn(document, "hidden", "get").mockReturnValue(false);
    source = new FakeSource();
    api.watchEvents.mockImplementation((onEvent, opts) => {
      deliver = onEvent;
      options = opts;
      return source;
    });
  });

  it("shares subscriptions and refreshes them when the stream reconnects", async () => {
    const { events } = await import("./events.svelte.js");
    events.setAvailable(true);
    const first = vi.fn();
    const second = vi.fn();
    const unsubFirst = events.subscribe(first);
    const unsubSecond = events.subscribe(second);
    expect(api.watchEvents).toHaveBeenCalledOnce();
    deliver({ scope: "messages" });
    source.dispatchEvent(new Event("events_ready"));
    expect(first.mock.calls).toEqual([[{ scope: "messages" }], [{ scope: "sync" }]]);
    expect(second.mock.calls).toEqual(first.mock.calls);
    unsubFirst();
    expect(source.close).not.toHaveBeenCalled();
    unsubSecond();
    expect(source.close).toHaveBeenCalledOnce();
  });

  it("opens on return and settles startup when hidden", async () => {
    vi.spyOn(document, "hidden", "get").mockReturnValue(true);
    const { events } = await import("./events.svelte.js");
    const unsub = events.subscribe(vi.fn());
    events.setAvailable(true);
    await expect(events.whenReady(100)).resolves.toBeUndefined();
    vi.spyOn(document, "hidden", "get").mockReturnValue(false);
    document.dispatchEvent(new Event("visibilitychange"));
    expect(api.watchEvents).toHaveBeenCalledOnce();
    unsub();
  });

  it("tracks repeated subscriptions and debounces updates", async () => {
    vi.useFakeTimers();
    const { events } = await import("./events.svelte.js");
    events.setAvailable(true);
    const listener = vi.fn();
    const first = events.subscribe(listener);
    const second = events.subscribe(listener);
    const debounced = vi.fn();
    const stopDebounce = events.subscribeDebounced(debounced, 300);
    deliver({ scope: "messages" });
    expect(listener).toHaveBeenCalledTimes(2);
    first();
    deliver({ scope: "sessions" });
    await vi.advanceTimersByTimeAsync(300);
    expect(listener).toHaveBeenCalledTimes(3);
    expect(debounced).toHaveBeenCalledExactlyOnceWith({ scope: "sessions" });
    second();
    stopDebounce();
    vi.useRealTimers();
  });

  it("reopens a stream after a transient failure", async () => {
    vi.useFakeTimers();
    const active = new Set<(event: DataChangedEvent) => void>();
    api.watchEvents.mockImplementation((onEvent) => {
      const stream = new FakeSource();
      active.add(onEvent);
      stream.close.mockImplementation(() => {
        stream.readyState = 2;
        active.delete(onEvent);
      });
      return stream;
    });
    const { events, EVENTS_STORE_HEAL_INTERVAL_MS } = await import("./events.svelte.js");
    events.setAvailable(true);
    const listener = vi.fn();
    const unsub = events.subscribe(listener);
    const first = api.watchEvents.mock.results[0]!.value as FakeSource;
    first.readyState = 2;
    await vi.advanceTimersByTimeAsync(EVENTS_STORE_HEAL_INTERVAL_MS);
    expect(api.watchEvents).toHaveBeenCalledTimes(2);
    expect(first.close).toHaveBeenCalledOnce();
    for (const send of active) send({ scope: "messages" });
    expect(listener).toHaveBeenCalledExactlyOnceWith({ scope: "messages" });
    unsub();
    expect(active.size).toBe(0);
    vi.useRealTimers();
  });

  it("stops retrying a permanently unavailable stream", async () => {
    vi.useFakeTimers();
    const { events, EVENTS_STORE_HEAL_INTERVAL_MS } = await import("./events.svelte.js");
    events.setAvailable(true);
    const unsub = events.subscribe(vi.fn());
    options.onPermanentFailure?.();
    source.readyState = 2;
    await vi.advanceTimersByTimeAsync(EVENTS_STORE_HEAL_INTERVAL_MS * 2);
    expect(api.watchEvents).toHaveBeenCalledOnce();
    unsub();
    vi.useRealTimers();
  });
});
