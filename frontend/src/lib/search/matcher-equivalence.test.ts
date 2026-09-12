import { describe, expect, it } from "vite-plus/test";
import { createOccurrenceMatcher, findOccurrences, prepareSearchText } from "./dom-text.js";

describe("prepared occurrence matching", () => {
  it("keeps ASCII boundaries, non-overlap and empty-query behavior", () => {
    expect(findOccurrences("aAA aAa", "AA")).toEqual([
      { start: 0, end: 2 },
      { start: 4, end: 6 },
    ]);
    expect(findOccurrences("needle", "  ")).toEqual([]);
    expect(findOccurrences("", "needle")).toEqual([]);
  });
  it("preserves per-code-point sigma instead of contextual lowercase", () => {
    expect(findOccurrences("ΟΣ", "οσ")).toEqual([{ start: 0, end: 2 }]);
    expect(findOccurrences("ΟΣ", "ος")).toEqual([]);
  });
  it("maps length-changing lowercase and astral letters to original offsets", () => {
    expect(findOccurrences("xİ𐐀😀", "i")).toEqual([{ start: 1, end: 2 }]);
    expect(findOccurrences("xİ𐐀😀", "𐐨")).toEqual([{ start: 2, end: 4 }]);
    expect(findOccurrences("xİ𐐀😀", "😀")).toEqual([{ start: 4, end: 6 }]);
    expect(findOccurrences("😀", "\ud83d")).toEqual([]);
    expect(findOccurrences("ßẞ", "ss")).toEqual([]);
  });
  it("reuses prepared text across different queries without changing it", () => {
    const text = prepareSearchText("NEEDLE İ needle");
    expect(createOccurrenceMatcher("needle")(text)).toEqual([
      { start: 0, end: 6 },
      { start: 9, end: 15 },
    ]);
    expect(createOccurrenceMatcher("i")(text)).toEqual([{ start: 7, end: 8 }]);
    expect(createOccurrenceMatcher("missing")(text)).toEqual([]);
    expect(createOccurrenceMatcher("needle")(text)).toEqual([
      { start: 0, end: 6 },
      { start: 9, end: 15 },
    ]);
  });
  it("preserves Unicode scalar lowercasing on deterministic mixed input", () => {
    let seed = 721;
    const next = () => (seed = (Math.imul(seed, 1664525) + 1013904223) >>> 0);
    for (let sample = 0; sample < 2000; sample++) {
      const text = Array.from({ length: 20 }, () => String.fromCodePoint(next() % 0x110000)).join(
        "",
      );
      const expected = Array.from(text, (point) => point.toLowerCase()).join("");
      expect(prepareSearchText(text).value).toBe(expected);
    }
  });
  it("stores only expansion boundaries for mostly ASCII Unicode blocks", () => {
    const text = "A".repeat(100000) + "İ" + "Z".repeat(100000);
    const prepared = prepareSearchText(text);
    expect(prepared.expansions?.length).toBe(4);
    expect(createOccurrenceMatcher("iz")(prepared)).toEqual([]);
    expect(createOccurrenceMatcher("i\u0307z")(prepared)).toEqual([{ start: 100000, end: 100002 }]);
    expect(prepareSearchText("ΟΣ中文𐐀").expansions).toBe(undefined);
  });
});
