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
  private abortController: AbortController | null = null;
  private debounceTimer: ReturnType<typeof setTimeout> | null = null;

  constructor() {
    $effect.root(() => {
      $effect(() => {
        const q = this.query;
        const sessionId = messages.sessionId;

        if (!q.trim() || !sessionId) {
          this.cancelPending();
          this.matches = [];
          this.currentMatchIndex = -1;
          this.prevQuery = q;
          return;
        }

        const queryChanged = q !== this.prevQuery;
        const sessionChanged = sessionId !== this.prevSessionId;
        this.prevQuery = q;
        this.prevSessionId = sessionId;

        if (queryChanged || sessionChanged) {
          // Debounce API calls — wait for user to pause typing
          this.cancelPending();
          this.debounceTimer = setTimeout(() => {
            this.fetchMatches(q, sessionId);
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

  private async fetchMatches(q: string, sessionId: string) {
    const ac = new AbortController();
    this.abortController = ac;
    this.loading = true;

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
      this.currentMatchIndex = found.length > 0 ? 0 : -1;
      if (found.length > 0) {
        await this.scrollToMatch(found[0]!);
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
