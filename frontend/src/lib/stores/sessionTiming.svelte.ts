import { fetchSessionTiming } from "../api/timing.js";
import type { SessionTiming } from "../api/types/timing.js";
import { LatestRead } from "../utils/latest-read.js";

/** Per-session timing snapshot fetched from
 *  GET /api/v1/sessions/{id}/timing and refreshed live by the
 *  session.timing SSE event on the watch stream. */
class SessionTimingStore {
  timing: SessionTiming | null = $state(null);
  loading: boolean = $state(false);
  error: string | null = $state(null);

  private currentSessionId: string | null = null;
  private loadVersion = 0;
  private timingRead = new LatestRead();

  /** True when the current snapshot belongs to the given session. */
  isForSession(sessionId: string): boolean {
    return this.currentSessionId === sessionId;
  }

  /** Fetch the timing snapshot for sessionId. Cached per session;
   *  calling load() again for the same session is a no-op once a
   *  snapshot is in memory. SSE events update the cached snapshot
   *  in place. */
  async load(sessionId: string): Promise<void> {
    if (
      this.currentSessionId === sessionId &&
      this.timing !== null
    ) {
      return;
    }
    if (this.currentSessionId !== sessionId) {
      this.timing = null;
      this.error = null;
    }
    this.currentSessionId = sessionId;
    const version = ++this.loadVersion;
    const signal = this.timingRead.begin();
    this.loading = true;
    this.error = null;
    try {
      const t = await fetchSessionTiming(sessionId, signal);
      if (
        version !== this.loadVersion ||
        !this.timingRead.isCurrent(signal)
      ) return;
      this.timing = t;
    } catch (e) {
      if (
        signal.aborted ||
        version !== this.loadVersion ||
        !this.timingRead.isCurrent(signal)
      ) return;
      this.error =
        e instanceof Error ? e.message : String(e);
      this.timing = null;
    } finally {
      if (this.timingRead.finish(signal)) {
        this.loading = false;
      }
    }
  }

  /** Called by the SSE handler when a session.timing event
   *  arrives. Ignored if the payload is for a different session
   *  than the one currently loaded. */
  applyEvent(payload: SessionTiming): void {
    if (payload.session_id !== this.currentSessionId) return;
    this.timing = payload;
    this.error = null;
  }

  /** Drop cached state. Call when leaving session detail view. */
  reset(): void {
    this.timingRead.cancel();
    this.loadVersion++;
    this.currentSessionId = null;
    this.timing = null;
    this.loading = false;
    this.error = null;
  }

  cancelInFlight(): void {
    this.loadVersion++;
    this.timingRead.cancel();
    this.loading = false;
  }
}

export const sessionTiming = new SessionTimingStore();
