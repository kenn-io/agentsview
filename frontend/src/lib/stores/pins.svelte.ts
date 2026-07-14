import type { PinnedMessage } from "../api/types.js";
import { PinsService } from "../api/generated/index";
import {
  callGenerated,
  configureGeneratedClient,
  isAbortError,
} from "../api/runtime.js";
import { LatestRead } from "../utils/latest-read.js";

interface PinsResponse {
  pins: PinnedMessage[];
}

interface PinMessageResponse {
  id: number;
}

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
  /** Project that the current this.pins was fetched for. */
  #loadedProject: string | undefined = undefined;
  #allPinsRead = new LatestRead();
  #sessionPinsRead = new LatestRead();

  async loadAll(project?: string) {
    // Clear immediately when switching projects to prevent stale
    // pins from the previous filter remaining visible while the
    // new request is in-flight or if it fails.
    if (project !== this.#loadedProject) {
      this.pins = [];
    }
    this.loading = true;
    const loadVer = ++this.#loadAllVersion;
    const signal = this.#allPinsRead.begin();
    const mutVer = this.#mutationVersion;
    try {
      configureGeneratedClient();
      const res = await callGenerated(
        () => PinsService.getApiV1Pins({ project }),
        signal,
      ) as unknown as PinsResponse;
      // Apply only if this is the latest load AND no mutation
      // occurred since the request started (which would make
      // this response stale relative to the optimistic state).
      if (
        this.#allPinsRead.isCurrent(signal) &&
        this.#loadAllVersion === loadVer &&
        this.#mutationVersion === mutVer
      ) {
        this.pins = res.pins;
        this.#loadedProject = project;
      }
    } catch (e) {
      if (isAbortError(e) || !this.#allPinsRead.isCurrent(signal)) return;
      // Silently ignore — pins are non-critical.
    } finally {
      if (this.#allPinsRead.finish(signal)) {
        this.loading = false;
      }
    }
  }

  async loadForSession(sessionId: string) {
    const isNewSession = this.#currentSessionId !== sessionId;
    this.#currentSessionId = sessionId;
    const loadVer = ++this.#loadVersion;
    const signal = this.#sessionPinsRead.begin();
    const mutVer = this.#mutationVersion;
    // Only clear on session change to avoid flickering pins
    // during re-fetches triggered by mutation completion.
    if (isNewSession) {
      this.sessionPinIds = new Set();
    }
    try {
      configureGeneratedClient();
      const res = await callGenerated(
        () => PinsService.getApiV1SessionsIdPins({ id: sessionId }),
        signal,
      ) as unknown as PinsResponse;
      if (
        this.#sessionPinsRead.isCurrent(signal) &&
        this.#loadVersion === loadVer &&
        this.#mutationVersion === mutVer
      ) {
        this.sessionPinIds = new Set(
          res.pins.map((p) => p.message_id),
        );
      }
    } catch (e) {
      if (isAbortError(e) || !this.#sessionPinsRead.isCurrent(signal)) return;
      // Silently ignore — pins are non-critical.
    } finally {
      this.#sessionPinsRead.finish(signal);
    }
  }

  clearSession() {
    this.cancelSessionPinsRead();
    this.#currentSessionId = null;
    this.sessionPinIds = new Set();
  }

  cancelAllPinsRead(): void {
    this.#loadAllVersion++;
    this.#allPinsRead.cancel();
    this.loading = false;
  }

  cancelSessionPinsRead(): void {
    this.#loadVersion++;
    this.#sessionPinsRead.cancel();
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
      configureGeneratedClient();
      await PinsService.deleteApiV1SessionsIdMessagesMessageidPin({
        id: sessionId,
        messageId,
      });
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
        configureGeneratedClient();
        const result =
          await PinsService.postApiV1SessionsIdMessagesMessageidPin({
            id: sessionId,
            messageId,
            requestBody: {},
          }) as unknown as PinMessageResponse;
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

export function createPinsStore(): PinsStore {
  return new PinsStore();
}

export const pins = createPinsStore();
