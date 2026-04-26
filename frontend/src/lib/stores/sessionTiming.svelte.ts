import { fetchSessionTiming } from "../api/timing.js";
import type { SessionTiming } from "../api/types/timing.js";

/** Per-session timing snapshot fetched from
 *  GET /api/v1/sessions/{id}/timing and refreshed live by the
 *  session.timing SSE event on the watch stream. */
class SessionTimingStore {
  timing: SessionTiming | null = $state(null);
  loading: boolean = $state(false);
  error: string | null = $state(null);

  private currentSessionId: string | null = null;
  private loadVersion = 0;

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
    this.loading = true;
    this.error = null;
    try {
      const t = await fetchSessionTiming(sessionId);
      if (version !== this.loadVersion) return;
      this.timing = t;
    } catch (e) {
      if (version !== this.loadVersion) return;
      this.error =
        e instanceof Error ? e.message : String(e);
      this.timing = null;
    } finally {
      if (version === this.loadVersion) {
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
    this.loadVersion++;
    this.currentSessionId = null;
    this.timing = null;
    this.loading = false;
    this.error = null;
  }
}

export const sessionTiming = new SessionTimingStore();
