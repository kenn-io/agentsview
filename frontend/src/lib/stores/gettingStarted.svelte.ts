import { readProgress, type ReadProgressStore } from "./read-progress.svelte.js";

export type Item = "session" | "search" | "usage" | "insights";
export const items: Item[] = ["session", "search", "usage", "insights"];
const key = "agentsview-getting-started";

function read(): string[] {
  try {
    const value = JSON.parse(localStorage.getItem(key) ?? "[]");
    return Array.isArray(value) ? value : [];
  } catch {
    return [];
  }
}

export class GettingStartedStore {
  done = $state<Set<string>>(new Set(read()));

  constructor(progress: ReadProgressStore = readProgress) {
    if (this.done.size === 0 && progress.hasMarkers) this.done.add("dismissed");
  }

  get visible(): boolean {
    return !this.done.has("dismissed") && !items.every((item) => this.done.has(item));
  }

  complete(item: Item | "dismissed") {
    const done = new Set([...read(), ...this.done]);
    if (done.has(item)) {
      this.done = done;
      return;
    }
    done.add(item);
    this.done = done;
    try {
      localStorage.setItem(key, JSON.stringify([...this.done]));
    } catch {
      // Storage is optional.
    }
  }

  dismiss() {
    this.complete("dismissed");
  }
}

export const gettingStarted = new GettingStartedStore();
