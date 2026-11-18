import { readProgress, type ReadProgressStore } from "./read-progress.svelte.js";

export type Item = "session" | "search" | "usage" | "insights";
export const items: Item[] = ["session", "search", "usage", "insights"];
const key = "agentsview-getting-started";

function read(): string[] {
  try {
    const value = JSON.parse(localStorage.getItem(key) ?? "[]");
    return Array.isArray(value) ? value.filter((item): item is string => typeof item === "string") : [];
  } catch {
    return [];
  }
}

export class GettingStartedStore {
  done = $state<Set<string>>(new Set(read()));

  constructor(progress: ReadProgressStore = readProgress) {
    try {
      if (localStorage.getItem(key) === null) {
        if (progress.hasMarkers) this.done.add("dismissed");
        localStorage.setItem(key, JSON.stringify([...this.done]));
      }
    } catch {
      // Storage is optional.
    }
    if (typeof window !== "undefined") {
      window.addEventListener("storage", (event) => {
        if (event.key === key || event.key === null) this.done = new Set(read());
      });
    }
  }

  get visible(): boolean {
    return !this.done.has("dismissed") && !items.every((item) => this.done.has(item));
  }

  complete(item: Item | "dismissed") {
    this.done = new Set([...read(), ...this.done, item]);
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
