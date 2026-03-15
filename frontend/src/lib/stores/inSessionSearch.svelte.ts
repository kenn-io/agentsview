import { messages } from "./messages.svelte.js";
import { ui } from "./ui.svelte.js";
import * as api from "../api/client.js";

export interface SessionMatch {
  ordinal: number;
  sessionId: string;
}

class InSessionSearchStore {
  isOpen: boolean = $state(false);
  query: string = $state("");
  matches: SessionMatch[] = $state([]);
  currentMatchIndex: number = $state(-1);
  loading: boolean = $state(false);
  private prevQuery: string = "";
  private prevSessionId: string = "";
  private prevMessageCount: number = 0;
  private abortController: AbortController | null = null;
  private debounceTimer: ReturnType<typeof setTimeout> | null = null;

  constructor() {
    $effect.root(() => {
      $effect(() => {
        const q = this.query;
        const sessionId = messages.sessionId;
        const msgCount = messages.messageCount;

        if (!q.trim() || !sessionId) {
          this.cancelPending();
          this.matches = [];
          this.currentMatchIndex = -1;
          this.prevQuery = q;
          this.prevMessageCount = msgCount;
          return;
        }

        const queryChanged = q !== this.prevQuery;
        const sessionChanged = sessionId !== this.prevSessionId;
        const contentChanged = msgCount !== this.prevMessageCount;
        this.prevQuery = q;
        this.prevSessionId = sessionId;
        this.prevMessageCount = msgCount;

        if (queryChanged || sessionChanged || contentChanged) {
          const preservePosition =
            contentChanged && !queryChanged && !sessionChanged;
          this.cancelPending();
          if (!preservePosition) {
            this.matches = [];
            this.currentMatchIndex = -1;
          }
          this.loading = true;
          this.debounceTimer = setTimeout(() => {
            this.fetchMatches(q, sessionId, preservePosition);
          }, 150);
        }
      });

      // Auto-close when no session is open
      $effect(() => {
        if (!messages.sessionId && this.isOpen) {
          this.close();
        }
      });
    });
  }

  private cancelPending() {
    if (this.debounceTimer !== null) {
      clearTimeout(this.debounceTimer);
      this.debounceTimer = null;
    }
    if (this.abortController) {
      this.abortController.abort();
      this.abortController = null;
    }
    this.loading = false;
  }

  private async fetchMatches(
    q: string,
    sessionId: string,
    preservePosition = false,
  ) {
    const ac = new AbortController();
    this.abortController = ac;
    this.loading = true;

    const prevOrdinal = this.currentOrdinal;

    try {
      const res = await api.searchSession(sessionId, q, {
        signal: ac.signal,
      });
      if (ac.signal.aborted) return;

      const found: SessionMatch[] = res.ordinals.map((ord) => ({
        ordinal: ord,
        sessionId,
      }));

      this.matches = found;

      if (preservePosition && prevOrdinal !== null) {
        const idx = found.findIndex(
          (m) => m.ordinal === prevOrdinal,
        );
        if (idx >= 0) {
          this.currentMatchIndex = idx;
        } else {
          this.currentMatchIndex = found.length > 0 ? 0 : -1;
          if (found.length > 0) {
            await this.scrollToMatch(found[0]!);
          }
        }
      } else {
        this.currentMatchIndex = found.length > 0 ? 0 : -1;
        if (found.length > 0) {
          await this.scrollToMatch(found[0]!);
        }
      }
    } catch (err: unknown) {
      if (err instanceof DOMException && err.name === "AbortError") return;
      console.warn("Session search failed:", err);
    } finally {
      if (this.abortController === ac) {
        this.abortController = null;
        this.loading = false;
      }
    }
  }

  private async scrollToMatch(match: SessionMatch) {
    await messages.ensureOrdinalLoaded(match.ordinal);
    ui.scrollToOrdinal(match.ordinal, match.sessionId);
  }

  open() {
    this.isOpen = true;
  }

  close() {
    this.cancelPending();
    this.isOpen = false;
    this.query = "";
    this.matches = [];
    this.currentMatchIndex = -1;
    this.prevQuery = "";
    this.prevSessionId = "";
    this.prevMessageCount = 0;
  }

  toggle() {
    if (this.isOpen) {
      this.close();
    } else {
      this.open();
    }
  }

  async next() {
    if (this.matches.length === 0) return;
    this.currentMatchIndex =
      (this.currentMatchIndex + 1) % this.matches.length;
    await this.scrollToMatch(this.matches[this.currentMatchIndex]!);
  }

  async prev() {
    if (this.matches.length === 0) return;
    this.currentMatchIndex =
      (this.currentMatchIndex - 1 + this.matches.length) %
      this.matches.length;
    await this.scrollToMatch(this.matches[this.currentMatchIndex]!);
  }

  get currentOrdinal(): number | null {
    const match = this.matches[this.currentMatchIndex];
    return match?.ordinal ?? null;
  }
}

export const inSessionSearch = new InSessionSearchStore();
