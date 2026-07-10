// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from "vitest";
import { ReadProgressStore } from "./read-progress.svelte.js";

afterEach(() => {
  localStorage.clear();
  vi.restoreAllMocks();
});

describe("ReadProgressStore", () => {
  it("keeps absent state distinct and advances one session monotonically", () => {
    const store = new ReadProgressStore();

    expect(store.get("one")).toBeNull();
    store.baseline("one", 3, 4);
    store.baseline("two", 8, 9);
    store.recordVisible("one", 2, 3);
    store.recordVisible("one", 5, 6);

    expect(store.get("one")).toEqual({ ordinal: 5, messageCount: 6 });
    expect(store.get("two")).toEqual({ ordinal: 8, messageCount: 9 });
    expect(store.hasUnread("one", 6)).toBe(false);
    expect(store.hasUnread("one", 7)).toBe(true);
  });

  it("restores valid records and ignores malformed JSON and wrong records", () => {
    localStorage.setItem(
      "agentsview-read-progress",
      JSON.stringify({
        version: 1,
        sessions: {
          valid: { ordinal: 2, messageCount: 3 },
          invalid: { ordinal: "2", messageCount: 3 },
        },
      }),
    );
    expect(new ReadProgressStore().get("valid")).toEqual({
      ordinal: 2,
      messageCount: 3,
    });
    expect(new ReadProgressStore().get("invalid")).toBeNull();

    localStorage.setItem("agentsview-read-progress", "malformed JSON");
    expect(new ReadProgressStore().get("valid")).toBeNull();
    localStorage.setItem(
      "agentsview-read-progress",
      JSON.stringify({
        version: 1,
        sessions: [{ valid: { ordinal: 2, messageCount: 3 } }],
      }),
    );
    expect(new ReadProgressStore().get("valid")).toBeNull();
  });

  it("keeps rendering state when storage access or writes fail", () => {
    const getItem = vi.spyOn(Storage.prototype, "getItem").mockImplementation(() => {
      throw new Error("storage unavailable");
    });
    const store = new ReadProgressStore();
    getItem.mockRestore();
    const setItem = vi.spyOn(Storage.prototype, "setItem").mockImplementation(() => {
      throw new Error("quota exceeded");
    });

    store.baseline("one", -1, 0);
    store.recordVisible("one", 0, 1);

    expect(store.get("one")).toEqual({ ordinal: 0, messageCount: 1 });
  });

  it("uses in-memory state when localStorage is not Storage-like", () => {
    const descriptor = Object.getOwnPropertyDescriptor(
      globalThis,
      "localStorage",
    );
    Object.defineProperty(globalThis, "localStorage", {
      configurable: true,
      value: { getItem() {} },
    });
    try {
      const store = new ReadProgressStore();
      store.baseline("one", -1, 0);
      store.recordVisible("one", 0, 1);
      expect(store.get("one")).toEqual({ ordinal: 0, messageCount: 1 });
    } finally {
      Object.defineProperty(globalThis, "localStorage", descriptor!);
    }
  });
});
