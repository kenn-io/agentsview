import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

// Minimal EventSource stub. Tests control when events fire and
// assert on the number of instances created.
class FakeEventSource {
  static instances: FakeEventSource[] = [];
  public url: string;
  public readyState = 1;
  private listeners: Record<string, ((ev: MessageEvent) => void)[]> = {};
  public onerror: ((ev: Event) => void) | null = null;
  public closed = false;

  constructor(url: string) {
    this.url = url;
    FakeEventSource.instances.push(this);
  }

  addEventListener(name: string, cb: (ev: MessageEvent) => void) {
    (this.listeners[name] ||= []).push(cb);
  }

  close() {
    this.closed = true;
  }

  fire(name: string, data: unknown) {
    const payload = { data: JSON.stringify(data) } as MessageEvent;
    (this.listeners[name] || []).forEach((cb) => cb(payload));
  }

  static reset() {
    FakeEventSource.instances = [];
  }
}

beforeEach(() => {
  FakeEventSource.reset();
  vi.stubGlobal("EventSource", FakeEventSource);
});

afterEach(() => {
  vi.unstubAllGlobals();
});

describe("events store", () => {
  it("opens a single EventSource on first subscribe", async () => {
    const { events } = await import("./events.svelte.js");
    const unsub1 = events.subscribe(() => {});
    const unsub2 = events.subscribe(() => {});
    expect(FakeEventSource.instances).toHaveLength(1);
    unsub1();
    unsub2();
  });

  it("closes the EventSource when the last subscriber leaves", async () => {
    const { events } = await import("./events.svelte.js");
    const unsub = events.subscribe(() => {});
    const es = FakeEventSource.instances[0]!;
    expect(es.closed).toBe(false);
    unsub();
    expect(es.closed).toBe(true);
  });

  it("delivers events to every subscriber", async () => {
    const { events } = await import("./events.svelte.js");
    const received: string[] = [];
    const unsub1 = events.subscribe((e) => received.push(`a:${e.scope}`));
    const unsub2 = events.subscribe((e) => received.push(`b:${e.scope}`));
    FakeEventSource.instances[0]!.fire("data_changed", { scope: "messages" });
    expect(received).toEqual(["a:messages", "b:messages"]);
    unsub1();
    unsub2();
  });

  it("tracks duplicate subscriptions independently", async () => {
    const { events } = await import("./events.svelte.js");
    const fn = vi.fn();
    const unsub1 = events.subscribe(fn);
    const unsub2 = events.subscribe(fn);

    expect(FakeEventSource.instances).toHaveLength(1);

    FakeEventSource.instances[0]!.fire("data_changed", { scope: "messages" });
    expect(fn).toHaveBeenCalledTimes(2);

    // First unsubscribe removes only one entry; connection stays open.
    unsub1();
    expect(FakeEventSource.instances[0]!.closed).toBe(false);

    fn.mockClear();
    FakeEventSource.instances[0]!.fire("data_changed", { scope: "sessions" });
    expect(fn).toHaveBeenCalledTimes(1);

    // Second unsubscribe closes the connection.
    unsub2();
    expect(FakeEventSource.instances[0]!.closed).toBe(true);
  });

  it("debounces rapid events into one callback per debounce window", async () => {
    vi.useFakeTimers();
    const { events } = await import("./events.svelte.js");
    const received: string[] = [];
    const unsub = events.subscribeDebounced(
      (e) => received.push(e.scope),
      100,
    );
    const es = FakeEventSource.instances[0]!;
    es.fire("data_changed", { scope: "messages" });
    es.fire("data_changed", { scope: "messages" });
    es.fire("data_changed", { scope: "sessions" });
    expect(received).toEqual([]);
    vi.advanceTimersByTime(100);
    expect(received).toEqual(["sessions"]); // last-write-wins
    unsub();
    vi.useRealTimers();
  });
});
