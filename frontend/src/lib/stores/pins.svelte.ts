import type { PinnedMessage } from "../api/types.js";
import * as api from "../api/client.js";

class PinsStore {
  /** All pins across all sessions (loaded for pinned tab). */
  pins: PinnedMessage[] = $state([]);
  loading: boolean = $state(false);

  /** Message IDs that are pinned in the currently viewed session. */
  sessionPinIds: Set<number> = $state(new Set());

  #currentSessionId: string | null = null;
  /** Tracks in-flight mutation message IDs to prevent double-clicks. */
  #inflight: Set<number> = new Set();
  #loadVersion = 0;
  #loadAllVersion = 0;
  /** Incremented on every mutation so loads can detect staleness. */
  #mutationVersion = 0;

  async loadAll() {
    this.loading = true;
    const loadVer = ++this.#loadAllVersion;
    const mutVer = this.#mutationVersion;
    try {
      const res = await api.listPins();
      // Apply only if this is the latest load AND no mutation
      // occurred since the request started (which would make
      // this response stale relative to the optimistic state).
      if (this.#loadAllVersion === loadVer && this.#mutationVersion === mutVer) {
        this.pins = res.pins;
      }
    } catch {
      // Silently ignore — pins are non-critical.
    } finally {
      if (this.#loadAllVersion === loadVer) {
        this.loading = false;
      }
    }
  }

  async loadForSession(sessionId: string) {
    const isNewSession = this.#currentSessionId !== sessionId;
    this.#currentSessionId = sessionId;
    const loadVer = ++this.#loadVersion;
    const mutVer = this.#mutationVersion;
    // Only clear on session change to avoid flickering pins
    // during re-fetches triggered by mutation completion.
    if (isNewSession) {
      this.sessionPinIds = new Set();
    }
    try {
      const res = await api.listSessionPins(sessionId);
      if (this.#loadVersion === loadVer && this.#mutationVersion === mutVer) {
        this.sessionPinIds = new Set(
          res.pins.map((p) => p.message_id),
        );
      }
    } catch {
      // Silently ignore — pins are non-critical.
    }
  }

  clearSession() {
    this.#currentSessionId = null;
    this.sessionPinIds = new Set();
  }

  isPinned(messageId: number): boolean {
    return this.sessionPinIds.has(messageId);
  }

  /** Re-fetch session pins after the last in-flight mutation settles. */
  #refetchAfterMutation() {
    if (this.#inflight.size === 0 && this.#currentSessionId) {
      this.loadForSession(this.#currentSessionId);
    }
  }

  async unpin(sessionId: string, messageId: number) {
    if (this.#inflight.has(messageId)) return;
    this.#inflight.add(messageId);
    this.#mutationVersion++;
    try {
      await api.unpinMessage(sessionId, messageId);
      // Only update sessionPinIds if still viewing the same session.
      if (this.#currentSessionId === sessionId) {
        const next = new Set(this.sessionPinIds);
        next.delete(messageId);
        this.sessionPinIds = next;
      }
      this.pins = this.pins.filter(
        (p) =>
          !(
            p.session_id === sessionId &&
            p.message_id === messageId
          ),
      );
    } catch {
      // Silently ignore — refetch will reconcile state.
    } finally {
      this.#inflight.delete(messageId);
      this.#refetchAfterMutation();
    }
  }

  async togglePin(
    sessionId: string,
    messageId: number,
    ordinal: number,
  ) {
    if (this.#inflight.has(messageId)) return;
    if (this.sessionPinIds.has(messageId)) {
      await this.unpin(sessionId, messageId);
    } else {
      this.#inflight.add(messageId);
      this.#mutationVersion++;
      try {
        const result = await api.pinMessage(sessionId, messageId);
        // Only update sessionPinIds if still viewing the same session.
        if (this.#currentSessionId === sessionId) {
          const next = new Set(this.sessionPinIds);
          next.add(messageId);
          this.sessionPinIds = next;
        }
        this.pins = [
          {
            id: result.id,
            session_id: sessionId,
            message_id: messageId,
            ordinal,
            created_at: new Date().toISOString(),
          },
          ...this.pins.filter(
            (p) =>
              !(
                p.session_id === sessionId &&
                p.message_id === messageId
              ),
          ),
        ];
      } catch {
        // Silently ignore — refetch will reconcile state.
      } finally {
        this.#inflight.delete(messageId);
        this.#refetchAfterMutation();
      }
    }
  }
}

export const pins = new PinsStore();
