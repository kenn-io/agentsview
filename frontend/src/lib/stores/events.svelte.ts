import { watchEvents, type DataChangedEvent } from "../api/client.js";

type Listener = (e: DataChangedEvent) => void;

// How often the store checks whether a closed EventSource can be
// rebuilt. Long-lived subscribers (e.g. the sessions store) never
// resubscribe, so without this self-heal a circuit-breaker close
// would leave them permanently detached from live events.
export const EVENTS_STORE_HEAL_INTERVAL_MS = 60_000;

class EventsStore {
  private es: EventSource | null = null;
  // Use a Map keyed by a unique per-call token so two subscribes
  // of the same function reference are tracked independently and
  // each unsubscribe only removes its own entry.
  private listeners = new Map<symbol, Listener>();
  private healTimer: ReturnType<typeof setInterval> | null = null;
  // Sticky flag. When watchEvents reports a permanent failure
  // (circuit breaker tripped without the EventSource ever reaching
  // OPEN — typical of PG serve mode's 503 but also possible for
  // false positives: slow startup, tunnel handshake, transient
  // network issue at mount time), stop rebuilding the connection.
  // The flag clears when the subscriber set empties OR when the
  // tab becomes visible again (giving a user-initiated retry
  // window so false positives recover without a page reload).
  private permanentlyFailed = false;
  private visibilityHandlerInstalled = false;

  /** Subscribe to every event. Returns unsubscribe. */
  subscribe(fn: Listener): () => void {
    const key = Symbol();
    this.listeners.set(key, fn);
    this.installVisibilityHandler();
    this.ensureOpen();
    this.ensureHealTimer();
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
    // Don't retry once watchEvents has told us the endpoint is
    // permanently unavailable. The safety-net polls on each view
    // still keep data fresh in that mode.
    if (this.permanentlyFailed) return;
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
    this.es = watchEvents(
      (e) => {
        for (const fn of this.listeners.values()) fn(e);
      },
      {
        onPermanentFailure: () => {
          this.permanentlyFailed = true;
          if (this.healTimer !== null) {
            clearInterval(this.healTimer);
            this.healTimer = null;
          }
        },
      },
    );
  }

  private close() {
    if (this.es === null) return;
    this.es.close();
    this.es = null;
    if (this.healTimer !== null) {
      clearInterval(this.healTimer);
      this.healTimer = null;
    }
    // Reset permanent-failure state when the subscriber set empties.
    // A fresh round of subscribers may succeed if the backend state
    // has changed (e.g., the user navigated pages, a sidebar opened
    // fresh, or the server was restarted between subscriptions).
    this.permanentlyFailed = false;
  }

  // ensureHealTimer starts a periodic check that rebuilds the
  // shared EventSource when it has been closed by the circuit
  // breaker but listeners are still registered. Without it, a
  // transient outage that trips the breaker would leave long-lived
  // subscribers stuck until the page reloads. Permanent failures
  // (endpoint never reached OPEN) disable the timer via
  // permanentlyFailed so this never becomes a retry storm against
  // a known-dead endpoint like PG serve's 503.
  private ensureHealTimer() {
    if (this.healTimer !== null) return;
    if (this.permanentlyFailed) return;
    this.healTimer = setInterval(() => {
      if (this.listeners.size === 0 || this.permanentlyFailed) {
        if (this.healTimer !== null) {
          clearInterval(this.healTimer);
          this.healTimer = null;
        }
        return;
      }
      const CLOSED = 2;
      if (this.es !== null && this.es.readyState === CLOSED) {
        this.es = null;
        this.ensureOpen();
      }
    }, EVENTS_STORE_HEAL_INTERVAL_MS);
  }

  // installVisibilityHandler wires a one-time document listener
  // that gives permanently-failed SSE one more retry when the user
  // refocuses the tab. This absorbs false-positive "permanent"
  // classifications (slow startup, tunnel handshake, transient
  // network hiccup at mount time) without running a periodic
  // retry storm in the background. Installed lazily on first
  // subscribe so module import has no global side effect.
  private installVisibilityHandler() {
    if (this.visibilityHandlerInstalled) return;
    if (
      typeof document === "undefined" ||
      typeof document.addEventListener !== "function"
    ) {
      return;
    }
    this.visibilityHandlerInstalled = true;
    document.addEventListener("visibilitychange", () => {
      if (document.hidden) return;
      if (!this.permanentlyFailed) return;
      if (this.listeners.size === 0) return;
      this.permanentlyFailed = false;
      this.es = null;
      this.ensureOpen();
      this.ensureHealTimer();
    });
  }
}

export const events = new EventsStore();
