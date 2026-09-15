import { describe, expect, it } from "vite-plus/test";
import type { Match } from "./session-index.js";
import {
  cursorFor,
  matchesInDisplayOrder,
  resolveSearchMatch,
  sameCursor,
  stepSearchMatch,
} from "./navigation.js";

function match(ordinal: number, block = "text:0", occurrence = 0): Match {
  return {
    ordinal,
    blockKey: `${ordinal}:${block}`,
    occurrence,
    start: occurrence * 7,
    end: occurrence * 7 + 6,
  };
}

const chronological = [
  match(1),
  match(1, "text:0", 1),
  match(1, "thinking:2"),
  match(8),
  match(8, "tool-output:0"),
  match(8, "tool-output:0", 1),
];

describe("session occurrence navigation", () => {
  it("reuses chronological order without copying", () => {
    expect(matchesInDisplayOrder(chronological, false)).toBe(chronological);
  });

  it("reverses messages without reversing blocks or characters", () => {
    const original = [...chronological];
    const ordered = matchesInDisplayOrder(Object.freeze([...chronological]), true);
    expect(ordered).toEqual([...chronological.slice(3), ...chronological.slice(0, 3)]);
    expect(ordered[1]).toBe(chronological[4]);
    expect(chronological).toEqual(original);
  });

  it("keeps a single message in reading order even with many occurrences", () => {
    const items = Array.from({ length: 5000 }, (_, i) => match(4, "text:0", i));
    expect(matchesInDisplayOrder(items, true)).toEqual(items);
  });

  it.each([false, true])(
    "starts at the first displayed occurrence, newestFirst=%s",
    (newestFirst) => {
      const ordered = matchesInDisplayOrder(chronological, newestFirst);
      expect(resolveSearchMatch(ordered, null, null, newestFirst)).toBe(ordered[0]);
    },
  );

  it("anchors forward in the displayed direction", () => {
    expect(resolveSearchMatch(chronological, null, 4, false)?.ordinal).toBe(8);
    expect(
      resolveSearchMatch(matchesInDisplayOrder(chronological, true), null, 4, true)?.ordinal,
    ).toBe(1);
  });

  it("selects the first remaining occurrence when the anchor is past every match", () => {
    // A filtered index can drop the cursor while other matches stay; a valid
    // result set must never resolve to no current occurrence.
    expect(resolveSearchMatch(chronological, null, 99, false)).toBe(chronological[0]);
    const newestFirst = matchesInDisplayOrder(chronological, true);
    expect(resolveSearchMatch(newestFirst, null, 0, true)).toBe(newestFirst[0]);
  });

  it("keeps an explicit tuple across prepending history and reversing display", () => {
    const cursor = cursorFor(chronological[1]!);
    const replaced = [match(0), ...chronological.map((item) => ({ ...item }))];
    const found = resolveSearchMatch(matchesInDisplayOrder(replaced, true), cursor, null, true);
    expect(found).toBe(replaced[2]);
    expect(sameCursor(found!, cursor)).toBe(true);
  });

  it("falls forward when the cursor's message disappears in either direction", () => {
    const cursor = match(4);
    expect(resolveSearchMatch(chronological, cursor, null, false)?.ordinal).toBe(8);
    expect(
      resolveSearchMatch(matchesInDisplayOrder(chronological, true), cursor, null, true)?.ordinal,
    ).toBe(1);
  });

  it.each([false, true])(
    "next and previous are inverse complete cycles, newestFirst=%s",
    (newestFirst) => {
      const ordered = matchesInDisplayOrder(chronological, newestFirst);
      let current = ordered[0]!;
      for (let index = 0; index < ordered.length; index++) {
        expect(current).toBe(ordered[index]);
        const next = stepSearchMatch(ordered, current, 1)!;
        expect(stepSearchMatch(ordered, next, -1)).toBe(current);
        current = next;
      }
      expect(current).toBe(ordered[0]);
    },
  );

  it("reveals a fresh query's anchor before advancing", () => {
    expect(stepSearchMatch(chronological, chronological[0]!, 1, true)).toBe(chronological[0]);
    expect(stepSearchMatch(chronological, chronological[0]!, 1)).toBe(chronological[1]);
  });

  it("wraps in the requested direction when there is no current occurrence", () => {
    expect(stepSearchMatch(chronological, null, 1, true)).toBe(chronological[0]);
    expect(stepSearchMatch(chronological, null, -1, true)).toBe(chronological.at(-1));
  });

  it("returns no target for empty matches and loops on a single match", () => {
    expect(stepSearchMatch([], null, 1)).toBeNull();
    expect(resolveSearchMatch([], null, null, false)).toBeNull();
    const only = match(1);
    expect(stepSearchMatch([only], only, 1)).toBe(only);
    expect(stepSearchMatch([only], only, -1)).toBe(only);
  });

  it("distinguishes message, block and occurrence in a cursor", () => {
    const current = match(1);
    expect(cursorFor(current)).toEqual({ ordinal: 1, blockKey: "1:text:0", occurrence: 0 });
    expect(cursorFor(current)).not.toBe(current);
    expect(sameCursor(current, match(1))).toBe(true);
    expect(sameCursor(current, match(2))).toBe(false);
    expect(sameCursor(current, match(1, "thinking:0"))).toBe(false);
    expect(sameCursor(current, match(1, "text:0", 1))).toBe(false);
  });
});
