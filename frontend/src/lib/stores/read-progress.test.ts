// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from "vitest";
import {
  buildReadProgressToken,
  ReadProgressStore,
} from "./read-progress.svelte.js";

afterEach(() => {
  localStorage.clear();
  vi.restoreAllMocks();
});

describe("read progress", () => {
  it("builds the session change token only from the transcript revision", () => {
    expect(
      buildReadProgressToken({
        transcript_revision: "abc",
        local_modified_at: "2026-07-11T12:00:00Z",
      }),
    ).toBe("abc");
    expect(
      buildReadProgressToken({
        transcript_revision: null,
        local_modified_at: "2026-07-11T12:00:01Z",
      }),
    ).toBeNull();
  });

  it("keeps the initial baseline read and only flips unread off after markRead", () => {
    const store = new ReadProgressStore();
    store.baseline("one", "old", 3);

    expect(store.get("one")).toEqual({
      token: "old",
      ordinal: 3,
      touched_at: expect.any(Number),
    });
    expect(store.hasUnread("one", "old")).toBe(false);
    expect(store.hasUnread("one", "new")).toBe(true);

    store.advanceOrdinal("one", 5);
    expect(store.get("one")).toEqual({
      token: "old",
      ordinal: 5,
      touched_at: expect.any(Number),
    });
    expect(store.hasUnread("one", "new")).toBe(true);

    store.markRead("one", "new", 7);
    expect(store.get("one")).toEqual({
      token: "new",
      ordinal: 7,
      touched_at: expect.any(Number),
    });
    expect(store.hasUnread("one", "new")).toBe(false);
  });

  it("ignores malformed JSON and keeps in-memory state when writes fail", () => {
    localStorage.setItem("agentsview-read-progress", "malformed JSON");
    const store = new ReadProgressStore();
    vi.spyOn(Storage.prototype, "setItem").mockImplementation(() => {
      throw new Error("quota exceeded");
    });

    store.baseline("one", "current", 2);
    store.markRead("one", "current", 4);

    expect(store.get("one")).toEqual({
      token: "current",
      ordinal: 4,
      touched_at: expect.any(Number),
    });
  });

  it("prunes old entries to keep storage bounded", () => {
    const now = vi.spyOn(Date, "now");
    now
      .mockReturnValueOnce(1)
      .mockReturnValueOnce(2)
      .mockReturnValueOnce(3);

    const store = new ReadProgressStore(2);
    store.baseline("first", "a", 1);
    store.baseline("second", "b", 2);
    store.baseline("third", "c", 3);

    expect(store.get("first")).toBeNull();
    expect(store.get("second")?.token).toBe("b");
    expect(store.get("third")?.token).toBe("c");
  });
});
