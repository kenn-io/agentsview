// @vitest-environment jsdom
import { afterEach, describe, expect, it } from "vite-plus/test";
import { GettingStartedStore } from "./gettingStarted.svelte.js";
import { ReadProgressStore } from "./read-progress.svelte.js";

const key = "agentsview-getting-started";
afterEach(() => localStorage.clear());

describe("getting started", () => {
  it("persists completed items and hides the checklist when all are done or dismissed", () => {
    const store = new GettingStartedStore(new ReadProgressStore());
    expect(store.visible).toBe(true);
    store.complete("session");
    store.complete("search");
    store.complete("usage");
    store.complete("insights");
    expect(new GettingStartedStore(new ReadProgressStore()).visible).toBe(false);
    localStorage.clear();
    new GettingStartedStore(new ReadProgressStore()).dismiss();
    expect(new GettingStartedStore(new ReadProgressStore()).visible).toBe(false);
  });

  it("keeps the checklist hidden for existing readers", () => {
    const progress = new ReadProgressStore();
    progress.markRead("one", "revision", 1);
    const store = new GettingStartedStore(progress);
    expect(store.visible).toBe(false);
  });

  it("preserves another tab's progress and unknown members on write", () => {
    const stale = new GettingStartedStore(new ReadProgressStore());
    localStorage.setItem(key, '["session","future"]');
    stale.complete("usage");
    expect(JSON.parse(localStorage.getItem(key)!)).toEqual(["session", "future", "usage"]);
  });

  it("starts fresh for corrupt storage", () => {
    localStorage.setItem(key, "broken");
    const store = new GettingStartedStore(new ReadProgressStore());
    expect(store.visible).toBe(true);
    store.complete("search");
    expect(JSON.parse(localStorage.getItem(key)!)).toEqual(["search"]);
  });
});
