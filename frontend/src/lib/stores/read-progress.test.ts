// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from "vitest";
import { ReadProgressStore } from "./read-progress.svelte.js";

afterEach(() => {
  localStorage.clear();
  vi.restoreAllMocks();
});

describe("ReadProgressStore", () => {
  it("keeps absent markers distinct and advances sessions monotonically", () => {
    const store = new ReadProgressStore();

    expect(store.get("one")).toBeNull();
    store.baseline("one", 3);
    store.baseline("two", 8);
    store.recordVisible("one", 2);
    store.recordVisible("one", 5);

    expect(store.get("one")).toEqual({ seenOrdinal: 5 });
    expect(store.get("two")).toEqual({ seenOrdinal: 8 });
    expect(store.hasUnread("one", 5)).toBe(false);
    expect(store.hasUnread("one", 6)).toBe(true);
    expect(store.hasUnread("missing", 6)).toBe(false);
  });

  it("tracks same-ordinal content growth when both cursors have content lengths", () => {
    const store = new ReadProgressStore();
    store.baseline("one", 5, 12);

    expect(store.hasUnread("one", 5, 12)).toBe(false);
    expect(store.hasUnread("one", 5, 18)).toBe(true);

    store.recordVisible("one", 5, 18);

    expect(store.get("one")).toEqual({
      seenOrdinal: 5,
      seenContentLength: 18,
    });
    expect(store.hasUnread("one", 5, 18)).toBe(false);
  });

  it("enriches same-ordinal legacy markers with a content-length cursor", () => {
    const store = new ReadProgressStore();
    store.baseline("one", 5);

    store.baseline("one", 5, 12);
    expect(store.get("one")).toEqual({
      seenOrdinal: 5,
      seenContentLength: 12,
    });

    store.recordVisible("one", 5, 18);
    expect(store.get("one")).toEqual({
      seenOrdinal: 5,
      seenContentLength: 18,
    });
  });

  it("treats null followed by ordinal zero as unread until observed", () => {
    const store = new ReadProgressStore();
    store.baseline("one", null);

    expect(store.hasUnread("one", 0)).toBe(true);
    store.recordVisible("one", 0);
    expect(store.get("one")).toEqual({ seenOrdinal: 0 });
    expect(store.hasUnread("one", 0)).toBe(false);
  });

  it("repairs regressions without creating inactive markers", () => {
    const store = new ReadProgressStore();
    store.baseline("tracked", 99);

    store.reconcile("tracked", 9);
    store.reconcile("absent", 9);

    expect(store.get("tracked")).toEqual({ seenOrdinal: 9 });
    expect(store.get("absent")).toBeNull();
  });

  it("keeps existing lower markers unread on a successful baseline", () => {
    const store = new ReadProgressStore();
    store.baseline("one", 3);
    store.baseline("one", 5);

    expect(store.get("one")).toEqual({ seenOrdinal: 3 });
    expect(store.hasUnread("one", 5)).toBe(true);
  });

  it("evicts the least recently visited session at capacity", () => {
    const store = new ReadProgressStore(3);
    store.baseline("oldest", 1);
    store.baseline("middle", 2);
    store.baseline("newest", 3);

    // Revisiting a session refreshes its recency without acknowledging
    // output beyond the existing marker.
    store.baseline("oldest", 9);
    store.baseline("incoming", 4);

    expect(store.get("middle")).toBeNull();
    expect(store.get("oldest")).toEqual({ seenOrdinal: 1 });
    expect(store.hasUnread("oldest", 9)).toBe(true);
    expect(store.get("newest")).toEqual({ seenOrdinal: 3 });
    expect(store.get("incoming")).toEqual({ seenOrdinal: 4 });
    expect(localStorage.getItem("agentsview-read-progress:session:middle"))
      .toBeNull();
    expect(JSON.parse(
      localStorage.getItem("agentsview-read-progress:index")!,
    )).toEqual(["newest", "oldest", "incoming"]);
  });

  it("prunes oversized persisted progress when loading", () => {
    localStorage.setItem(
      "agentsview-read-progress:index",
      JSON.stringify(["one", "two", "three", "four"]),
    );
    for (const [id, seenOrdinal] of Object.entries({
      one: 1,
      two: 2,
      three: 3,
      four: 4,
    })) {
      localStorage.setItem(
        `agentsview-read-progress:session:${id}`,
        JSON.stringify({ seenOrdinal }),
      );
    }

    const store = new ReadProgressStore(2);

    expect(store.get("one")).toBeNull();
    expect(store.get("two")).toBeNull();
    expect(store.get("three")).toEqual({ seenOrdinal: 3 });
    expect(store.get("four")).toEqual({ seenOrdinal: 4 });
    expect(localStorage.getItem("agentsview-read-progress:session:one"))
      .toBeNull();
    expect(localStorage.getItem("agentsview-read-progress:session:two"))
      .toBeNull();
    expect(JSON.parse(
      localStorage.getItem("agentsview-read-progress:index")!,
    )).toEqual(["three", "four"]);
  });

  it("writes only the active marker when visible progress advances", () => {
    const store = new ReadProgressStore();
    store.baseline("one", 1);
    const write = vi.spyOn(localStorage, "setItem");

    store.recordVisible("one", 2);

    expect(write).toHaveBeenCalledTimes(1);
    expect(write).toHaveBeenCalledWith(
      "agentsview-read-progress:session:one",
      JSON.stringify({ seenOrdinal: 2 }),
    );
  });

  it("ignores malformed storage and keeps in-memory state when writes fail", () => {
    localStorage.setItem("agentsview-read-progress:index", "malformed JSON");
    const store = new ReadProgressStore();
    vi.spyOn(Storage.prototype, "setItem").mockImplementation(() => {
      throw new Error("quota exceeded");
    });

    store.baseline("one", null);
    store.recordVisible("one", 0);

    expect(store.get("one")).toEqual({ seenOrdinal: 0 });
  });

  it("persists a marker separately from the recency index", () => {
    const store = new ReadProgressStore();
    store.baseline("one", 7, 12);

    expect(JSON.parse(
      localStorage.getItem("agentsview-read-progress:session:one")!,
    )).toEqual({ seenOrdinal: 7, seenContentLength: 12 });
    expect(JSON.parse(
      localStorage.getItem("agentsview-read-progress:index")!,
    )).toEqual(["one"]);
  });
});
