import { watchEvents, type DataChangedEvent } from "../api/client.js";

type Listener = (e: DataChangedEvent) => void;

class EventsStore {
  private es: EventSource | null = null;
  // Use a Map keyed by a unique per-call token so two subscribes
  // of the same function reference are tracked independently and
  // each unsubscribe only removes its own entry.
  private listeners = new Map<symbol, Listener>();

  /** Subscribe to every event. Returns unsubscribe. */
  subscribe(fn: Listener): () => void {
    const key = Symbol();
    this.listeners.set(key, fn);
    this.ensureOpen();
    return () => {
      this.listeners.delete(key);
      if (this.listeners.size === 0) {
        this.close();
      }
    };
  }

  /** Subscribe with a trailing-edge debounce. The callback fires
   * once, `delayMs` after the last event in a burst, with the
   * most recent event's payload. Returns unsubscribe. */
  subscribeDebounced(
    fn: Listener,
    delayMs = 300,
  ): () => void {
    let timer: ReturnType<typeof setTimeout> | null = null;
    let latest: DataChangedEvent | null = null;

    const wrapped: Listener = (e) => {
      latest = e;
      if (timer !== null) clearTimeout(timer);
      timer = setTimeout(() => {
        timer = null;
        if (latest) fn(latest);
        latest = null;
      }, delayMs);
    };

    const unsub = this.subscribe(wrapped);
    return () => {
      unsub();
      if (timer !== null) {
        clearTimeout(timer);
        timer = null;
      }
    };
  }

  private ensureOpen() {
    // watchEvents trips a circuit breaker and calls es.close() on
    // repeated errors; the store's cached handle then points at a
    // CLOSED EventSource. Treat that as "not open" and build a
    // fresh connection so existing/new subscribers can recover
    // once the endpoint starts serving again.
    //
    // EventSource.CLOSED === 2 per the HTML spec. Using the literal
    // here avoids referencing the EventSource global, which isn't
    // defined in every test environment (e.g. sessions tests mock
    // watchEvents directly without stubbing EventSource).
    const CLOSED = 2;
    if (this.es !== null && this.es.readyState !== CLOSED) {
      return;
    }
    this.es = watchEvents((e) => {
      for (const fn of this.listeners.values()) fn(e);
    });
  }

  private close() {
    if (this.es === null) return;
    this.es.close();
    this.es = null;
  }
}

export const events = new EventsStore();
