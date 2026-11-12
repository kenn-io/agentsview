import { SessionsService } from "../api/generated/index";
import { isAbortError } from "../api/runtime.js";
import { LatestRead } from "../utils/latest-read.js";
import {
  isPromptMessage,
  tocEntry,
  type TocEntry,
  type TocView,
} from "../components/content/session-toc.js";

const OPEN_KEY = "agentsview-session-toc";
const PAGE_SIZE = 1000;

function readStoredOpen(): boolean {
  try {
    return localStorage?.getItem(OPEN_KEY) === "true";
  } catch {
    return false;
  }
}

/**
 * State for the session prompt outline. Large sessions load their newest
 * messages first, so the outline reads the prompts older than the loaded
 * window from the server, one user-only read per session. Prompts in the
 * loaded window come from the messages store, which stays current as a
 * live session grows.
 */
class SessionTocStore {
  open: boolean = $state(readStoredOpen());
  /** The transcript rows on screen, published by the message list. */
  view: TocView | null = $state(null);
  /** The prompt last picked from the outline. */
  picked: number | null = $state(null);
  /** Prompts with ordinals below `olderBefore`, oldest first. */
  older: TocEntry[] = $state.raw([]);
  olderLoading: boolean = $state(false);
  olderFailed: boolean = $state(false);
  /** Bumped when the outline should take keyboard focus. */
  focusRequest: number = $state(0);

  private olderSessionId: string | null = $state(null);
  /** Every prompt below this ordinal is in `older`; 0 when nothing is. */
  private olderBefore: number = $state(0);
  private read = new LatestRead();
  private loadingSignal: AbortSignal | null = null;

  /** Open or close the outline; `focus` moves focus into it on open. */
  toggle(options: { focus?: boolean } = {}) {
    this.setOpen(!this.open);
    if (this.open && options.focus) this.focusRequest += 1;
  }

  setOpen(open: boolean) {
    // A resync can rewrite prompts the outline read earlier, and no signal
    // tells that apart from an append. Read them again on each opening,
    // keeping the old list on screen until the new one lands.
    if (open && !this.open) {
      // A read still running started before the reopening, so it may hold
      // the old prompts; drop it and let the outline start a fresh one.
      this.read.cancel();
      this.loadingSignal = null;
      this.olderLoading = false;
      this.olderBefore = 0;
      this.olderFailed = false;
    }
    this.open = open;
    try {
      localStorage?.setItem(OPEN_KEY, String(open));
    } catch {
      // ignore
    }
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

  /** Clear a failed read so the next `ensureOlder` tries again. */
  retryOlder() {
    this.olderFailed = false;
  }
}

export const sessionToc = new SessionTocStore();
