import { messages } from "./messages.svelte.js";
import { ui } from "./ui.svelte.js";

export interface SessionMatch {
  ordinal: number;
  sessionId: string;
}

class InSessionSearchStore {
  isOpen: boolean = $state(false);
  query: string = $state("");
  matches: SessionMatch[] = $state([]);
  currentMatchIndex: number = $state(-1);

  constructor() {
    $effect.root(() => {
      $effect(() => {
        const q = this.query;
        const msgs = messages.messages;
        const sessionId = messages.sessionId;

        if (!q.trim() || !sessionId) {
          this.matches = [];
          this.currentMatchIndex = -1;
          return;
        }

        const lower = q.toLowerCase();
        const found: SessionMatch[] = [];
        for (const msg of msgs) {
          if (msg.content.toLowerCase().includes(lower)) {
            found.push({ ordinal: msg.ordinal, sessionId });
          }
        }

        this.matches = found;
        this.currentMatchIndex = found.length > 0 ? 0 : -1;

        if (found.length > 0) {
          ui.scrollToOrdinal(found[0]!.ordinal, sessionId);
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

  open() {
    this.isOpen = true;
  }

  close() {
    this.isOpen = false;
    this.query = "";
    this.matches = [];
    this.currentMatchIndex = -1;
  }

  toggle() {
    if (this.isOpen) {
      this.close();
    } else {
      this.open();
    }
  }

  next() {
    if (this.matches.length === 0) return;
    this.currentMatchIndex =
      (this.currentMatchIndex + 1) % this.matches.length;
    this.scrollToCurrent();
  }

  prev() {
    if (this.matches.length === 0) return;
    this.currentMatchIndex =
      (this.currentMatchIndex - 1 + this.matches.length) %
      this.matches.length;
    this.scrollToCurrent();
  }

  private scrollToCurrent() {
    const match = this.matches[this.currentMatchIndex];
    if (match) {
      ui.scrollToOrdinal(match.ordinal, match.sessionId);
    }
  }

  get currentOrdinal(): number | null {
    const match = this.matches[this.currentMatchIndex];
    return match?.ordinal ?? null;
  }
}

export const inSessionSearch = new InSessionSearchStore();
