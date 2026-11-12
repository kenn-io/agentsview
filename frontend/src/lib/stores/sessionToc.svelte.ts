import { SessionsService } from "../api/generated/index";
import { isAbortError } from "../api/runtime.js";
import { LatestRead } from "../utils/latest-read.js";
import {
  isPromptMessage,
  tocEntry,
  type TocEntry,
  type TocView,
} from "../components/content/session-toc.js";

const PAGE_SIZE = 1000;

/**
 * State for the session prompt picker. Large sessions load their newest
 * messages first, so the picker reads the prompts older than the loaded
 * window from the server, one user-only read per session. Prompts in the
 * loaded window come from the messages store, which stays current as a
 * live session grows.
 */
class SessionTocStore {
  /** The transcript rows on screen, published by the message list. */
  view: TocView | null = $state(null);
  /** The prompt last picked from the picker. */
  picked: number | null = $state(null);
  /** Prompts with ordinals below `olderBefore`, oldest first. */
  older: TocEntry[] = $state.raw([]);
  olderLoading: boolean = $state(false);
  olderFailed: boolean = $state(false);
  /** Bumped when the picker should open, as the `t` shortcut asks. */
  openRequest: number = $state(0);

  private olderSessionId: string | null = $state(null);
  /** Every prompt below this ordinal is in `older`; 0 when nothing is. */
  private olderBefore: number = $state(0);
  private read = new LatestRead();
  private loadingSignal: AbortSignal | null = null;

  requestOpen() {
    this.openRequest += 1;
  }

  /**
   * Called each time the picker opens. A resync can rewrite prompts read
   * earlier, and no signal tells that apart from an append, so the next
   * `ensureOlder` reads them again. The old list stays until the new one
   * lands.
   */
  refreshOlder() {
    // A read still running started before this opening, so it may hold
    // the old prompts; drop it and let the picker start a fresh one.
    this.read.cancel();
    this.loadingSignal = null;
    this.olderLoading = false;
    this.olderBefore = 0;
    this.olderFailed = false;
  }

  /** Reports whether every prompt older than `before` is loaded. */
  covers(sessionId: string, before: number): boolean {
    return this.olderSessionId === sessionId && before <= this.olderBefore;
  }

  /** Prompts older than the loaded window, when they belong to `sessionId`. */
  olderFor(sessionId: string | null): TocEntry[] {
    return sessionId !== null && this.olderSessionId === sessionId ? this.older : [];
  }

  /** Forget the previous session's screen position and pick. */
  resetView() {
    this.view = null;
    this.picked = null;
  }

  private resetOlder() {
    this.read.cancel();
    this.older = [];
    this.olderLoading = false;
    this.loadingSignal = null;
    this.olderFailed = false;
    this.olderSessionId = null;
    this.olderBefore = 0;
  }

  /**
   * Make sure every prompt older than `before` is known for `sessionId`.
   * Earlier reads that already cover that range are kept; a later window
   * that starts past them reads again from the new boundary.
   */
  async ensureOlder(sessionId: string, before: number) {
    if (this.olderSessionId !== sessionId) {
      this.resetOlder();
      this.olderSessionId = sessionId;
    }
    if (before <= this.olderBefore || this.olderLoading || this.olderFailed) return;
    const signal = this.read.begin();
    this.loadingSignal = signal;
    this.olderLoading = true;
    try {
      const entries: TocEntry[] = [];
      let from = before - 1;
      // Later pages are bound to the first page's revision, so a rewrite
      // between pages fails the read instead of mixing two transcripts.
      let revision: string | undefined;
      while (from >= 0) {
        const res = await SessionsService.getApiV1SessionsByIdMessages(
          { id: sessionId },
          {
            roles: "user",
            direction: "desc",
            from,
            limit: PAGE_SIZE,
            ...(revision ? { expected_revision: revision } : {}),
          },
          { signal },
        );
        if (!this.read.isCurrent(signal)) return;
        revision ??= res.transcript_revision || undefined;
        const page = res.messages;
        for (const message of page) {
          if (isPromptMessage(message)) entries.push(tocEntry(message));
        }
        const oldest = page.at(-1)?.ordinal;
        if (page.length < PAGE_SIZE || oldest === undefined) break;
        from = oldest - 1;
      }
      if (!this.read.finish(signal)) return;
      entries.sort((a, b) => a.ordinal - b.ordinal);
      this.older = entries;
      this.olderBefore = before;
    } catch (err) {
      if (isAbortError(err) || !this.read.isCurrent(signal)) return;
      this.read.finish(signal);
      console.warn("Could not load earlier prompts:", err);
      this.olderFailed = true;
    } finally {
      if (this.loadingSignal === signal) {
        this.loadingSignal = null;
        this.olderLoading = false;
      }
    }
  }
}

export const sessionToc = new SessionTocStore();
