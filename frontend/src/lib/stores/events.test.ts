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
    this.readyState = 2; // EventSource.CLOSED per HTML spec
  }

  fireError() {
    if (this.onerror) this.onerror(new Event("error"));
  }

  fire(name: string, data: unknown) {
    const payload = { data: JSON.stringify(data) } as MessageEvent;
    (this.listeners[name] || []).forEach((cb) => cb(payload));
  }

  fireOpen() {
    (this.listeners["open"] || []).forEach((cb) =>
      cb(new Event("open") as MessageEvent),
    );
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

  it("self-heals a closed EventSource after a transient failure", async () => {
    vi.useFakeTimers();
    const { events, EVENTS_STORE_HEAL_INTERVAL_MS } = await import(
      "./events.svelte.js"
    );
    const received: string[] = [];
    const unsub = events.subscribe((e) => received.push(e.scope));
    const first = FakeEventSource.instances[0]!;

    // Simulate a transient failure: the stream opens successfully
    // at least once, then later errors trip the circuit breaker.
    // This is the "worked once, then failed" case that should heal.
    first.fireOpen();
    for (let i = 0; i < 5; i++) first.fireError();
    expect(first.closed).toBe(true);

    // Advance to the heal-timer tick. The store should notice the
    // closed ES and rebuild while the listener is still active.
    await vi.advanceTimersByTimeAsync(EVENTS_STORE_HEAL_INTERVAL_MS);
    expect(FakeEventSource.instances.length).toBe(2);
    const second = FakeEventSource.instances[1]!;
    expect(second.closed).toBe(false);

    second.fire("data_changed", { scope: "messages" });
    expect(received).toEqual(["messages"]);

    unsub();
    vi.useRealTimers();
  });

  it("does not heal after a permanent failure (never opened)", async () => {
    vi.useFakeTimers();
    const { events, EVENTS_STORE_HEAL_INTERVAL_MS } = await import(
      "./events.svelte.js"
    );
    const unsub = events.subscribe(() => {});
    const first = FakeEventSource.instances[0]!;

    // Permanent failure pattern: errors fire without any open ever
    // succeeding (e.g. PG serve 503 on /api/v1/events). The circuit
    // breaker trips AND marks the store permanentlyFailed, so no
    // heal timer rebuilds.
    for (let i = 0; i < 5; i++) first.fireError();
    expect(first.closed).toBe(true);

    // Advance far past the heal interval. The store must NOT
    // rebuild — doing so would reintroduce retry churn on an
    // endpoint that has already been classified as unreachable.
    await vi.advanceTimersByTimeAsync(EVENTS_STORE_HEAL_INTERVAL_MS * 3);
    expect(FakeEventSource.instances.length).toBe(1);

    unsub();
    vi.useRealTimers();
  });

  it("reopens the EventSource after a transient close on new subscribe", async () => {
    const { events } = await import("./events.svelte.js");
    const received: string[] = [];
    const unsub = events.subscribe((e) => received.push(e.scope));
    const first = FakeEventSource.instances[0]!;

    // Transient-close pattern: open fires once so the failure is
    // classified as recoverable, then the breaker trips.
    first.fireOpen();
    for (let i = 0; i < 5; i++) first.fireError();
    expect(first.closed).toBe(true);
    expect(first.readyState).toBe(2);

    // A new subscriber should trigger a fresh EventSource rather
    // than silently returning with a stale CLOSED handle.
    const unsub2 = events.subscribe((e) => received.push(`b:${e.scope}`));
    expect(FakeEventSource.instances.length).toBe(2);
    const second = FakeEventSource.instances[1]!;
    expect(second.closed).toBe(false);

    second.fire("data_changed", { scope: "messages" });
    expect(received).toContain("messages");
    expect(received).toContain("b:messages");

    unsub();
    unsub2();
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
