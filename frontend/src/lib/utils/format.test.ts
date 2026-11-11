import { afterEach, beforeEach, describe, expect, it, vi } from "vite-plus/test";
import { setLocale } from "../i18n/index.js";
import { testMoney } from "../test/money.js";
import {
  formatRelativeTime,
  sanitizeSnippet,
  highlightTerms,
  _resetNonceCounter,
  formatCost,
  formatCount,
  formatTokenUsage,
} from "./format.js";

describe("formatRelativeTime", () => {
  beforeEach(() => {
    vi.useFakeTimers();
    setLocale("en");
  });

  afterEach(() => {
    vi.useRealTimers();
    setLocale("en");
  });

  it.each([
    ["2026-06-17T12:00:00Z", "just now"],
    ["2026-06-17T11:58:00Z", "2m ago"],
    ["2026-06-17T09:30:00Z", "2h ago"],
    ["2026-06-15T12:00:00Z", "2d ago"],
  ])("formats %s in English", (value, expected) => {
    vi.setSystemTime(new Date("2026-06-17T12:00:20Z"));
    expect(formatRelativeTime(value)).toBe(expected);
  });

  it("localizes relative time labels", () => {
    setLocale("zh-CN");
    vi.setSystemTime(new Date("2026-06-17T12:00:20Z"));

    expect(formatRelativeTime("2026-06-17T12:00:00Z")).toBe("刚刚");
    expect(formatRelativeTime("2026-06-17T11:58:00Z")).toBe("2 分钟前");
    expect(formatRelativeTime("2026-06-15T12:00:00Z")).toBe("2 天前");
  });
});

describe("formatCost", () => {
  it.each([
    [0, "$0.00"],
    [0.004, "<$0.01"],
    [0.01, "$0.01"],
    [0.42, "$0.42"],
    [12.345, "$12.35"],
    [99.994, "$99.99"],
    [100, "$100"],
    [1234.5, "$1,235"],
  ])("formats %d as %s", (value, expected) => {
    expect(formatCost(testMoney(value))).toBe(expected);
  });
});

describe("sanitizeSnippet", () => {
  beforeEach(() => {
    _resetNonceCounter(0);
  });

  it.each([
    ["preserves <mark> tags", "hello <mark>world</mark> end", "hello <mark>world</mark> end"],
    [
      "escapes other HTML tags",
      '<script>alert("xss")</script>',
      '&lt;script&gt;alert("xss")&lt;/script&gt;',
    ],
    ["escapes img tags", "<img src=x onerror=alert(1)>", "&lt;img src=x onerror=alert(1)&gt;"],
    [
      "handles mixed mark and other tags",
      "<b>bold</b> <mark>highlighted</mark> <i>italic</i>",
      "&lt;b&gt;bold&lt;/b&gt; <mark>highlighted</mark> &lt;i&gt;italic&lt;/i&gt;",
    ],
    [
      "handles case-insensitive mark tags",
      "<MARK>upper</MARK> <Mark>mixed</Mark>",
      "<mark>upper</mark> <mark>mixed</mark>",
    ],
    [
      "handles multiple mark spans",
      "<mark>first</mark> gap <mark>second</mark>",
      "<mark>first</mark> gap <mark>second</mark>",
    ],
    ["returns empty string for empty input", "", ""],
    ["handles plain text without tags", "no tags here", "no tags here"],
    ["escapes angle brackets in content", "x < y > z", "x &lt; y &gt; z"],
    [
      "handles nested mark tags gracefully",
      "<mark>outer <mark>inner</mark></mark>",
      "<mark>outer <mark>inner</mark></mark>",
    ],
    [
      "escapes event handler attributes in mark-like tags",
      "<mark onload=alert(1)>text</mark>",
      "&lt;mark onload=alert(1)&gt;text</mark>",
    ],
    [
      "keeps pre-escaped mark entities as text",
      "&lt;mark&gt;not real&lt;/mark&gt;",
      "&lt;mark&gt;not real&lt;/mark&gt;",
    ],
    [
      "keeps pre-escaped entities alongside real mark tags",
      "<mark>real</mark> &lt;mark&gt;fake&lt;/mark&gt;",
      "<mark>real</mark> &lt;mark&gt;fake&lt;/mark&gt;",
    ],
    [
      "does not promote text matching old placeholder tokens",
      "text \x00MARK_O\x00 and \x00MARK_C\x00 here",
      "text \x00MARK_O\x00 and \x00MARK_C\x00 here",
    ],
    [
      "skips nonce when input contains the candidate placeholder",
      "text \x000\x00O\x000\x00 and \x000\x00C\x000\x00 here",
      "text \x000\x00O\x000\x00 and \x000\x00C\x000\x00 here",
    ],
  ])("%s", (_name, input, expected) => {
    expect(sanitizeSnippet(input)).toBe(expected);
  });
});

describe("formatCount", () => {
  afterEach(() => {
    setLocale("en");
  });

  it("formats English counts with compact units", () => {
    setLocale("en");
    expect(formatCount(1_405)).toBe("1.4K");
  });

  it("follows the active locale for decimals and compact units", () => {
    setLocale("fr");
    expect(formatCount(2.5)).toBe("2,5");
    expect(formatCount(999)).toBe("999");
    expect(formatCount(1_405)).toBe("1,4\u00a0k");
    expect(formatCount(18_033)).toBe("18,0\u00a0k");
    expect(formatCount(13_144_700_000)).toBe("13,1\u00a0Md");
  });
});

describe("formatTokenUsage", () => {
  it("returns null when no token metrics are reported", () => {
    expect(formatTokenUsage(0, false, 0, false)).toBeNull();
  });

  it("formats both reported token metrics", () => {
    expect(formatTokenUsage(2400, true, 180, true)).toBe("2.4K ctx / 180 out");
    expect(formatTokenUsage(145_600, true, 12_000, true)).toBe("145.6K ctx / 12.0K out");
  });

  it("renders an explicit placeholder for missing token metrics", () => {
    expect(formatTokenUsage(0, false, 180, true)).toBe("— ctx / 180 out");
    expect(formatTokenUsage(2400, true, 0, false)).toBe("2.4K ctx / — out");
  });
});

describe("highlightTerms", () => {
  it("flags case-insensitive matches of each query word", () => {
    expect(highlightTerms("Goldmark v2 uses goldmark parser", "goldmark parser")).toEqual([
      { text: "Goldmark", match: true },
      { text: " v2 uses ", match: false },
      { text: "goldmark", match: true },
      { text: " ", match: false },
      { text: "parser", match: true },
    ]);
  });

  it("matches query punctuation literally and in full", () => {
    expect(highlightTerms("call a.b(c) now", "a.b(c)")).toEqual([
      { text: "call ", match: false },
      { text: "a.b(c)", match: true },
      { text: " now", match: false },
    ]);
    expect(highlightTerms("it's done", "it's")).toEqual([
      { text: "it's", match: true },
      { text: " done", match: false },
    ]);
    expect(highlightTerms("axb(c) here", "a.b(c)")).toEqual([
      { text: "axb(c) here", match: false },
    ]);
  });

  it("strips phrase quotes but keeps the words", () => {
    expect(highlightTerms("the goldmark parser", '"goldmark"')).toEqual([
      { text: "the ", match: false },
      { text: "goldmark", match: true },
      { text: " parser", match: false },
    ]);
  });

  it("keeps quotes inside ordinary words and treats quoted phrases as one term", () => {
    expect(highlightTerms('a parser" b', 'parser"')).toEqual([
      { text: "a ", match: false },
      { text: 'parser"', match: true },
      { text: " b", match: false },
    ]);
    expect(highlightTerms("the goldmark v2 api", '"goldmark v2"')).toEqual([
      { text: "the ", match: false },
      { text: "goldmark v2", match: true },
      { text: " api", match: false },
    ]);
  });

  it("uses phrase syntax only when the query starts with a quote, like the backend", () => {
    // Not leading: every word is literal, quotes included.
    expect(highlightTerms('fix "bug" here; fix bug', 'fix "bug"')).toEqual([
      { text: "fix", match: true },
      { text: " ", match: false },
      { text: '"bug"', match: true },
      { text: " here; ", match: false },
      { text: "fix", match: true },
      { text: " bug", match: false },
    ]);
    // Leading: a doubled quote inside a phrase is one literal quote.
    expect(highlightTerms('she said "hi" twice', '"said ""hi"""')).toEqual([
      { text: "she ", match: false },
      { text: 'said "hi"', match: true },
      { text: " twice", match: false },
    ]);
  });

  it("keeps non-ASCII whitespace inside a quote-led term, like the backend", () => {
    // U+00A0 does not split terms in quote-led FTS syntax.
    expect(highlightTerms("fix\u00a0bug, then fix the bug", '"fix"\u00a0"bug"')).toEqual([
      { text: "fix\u00a0bug", match: true },
      { text: ", then fix the bug", match: false },
    ]);
  });

  it("folds Unicode case and keeps distinct spellings of one word", () => {
    // U+212A KELVIN SIGN folds to K only under Unicode case folding.
    expect(highlightTerms("Kelvin scale", "kelvin")).toEqual([
      { text: "Kelvin", match: true },
      { text: " scale", match: false },
    ]);
    // Dotted capital I lowercases to i plus a combining dot; both spellings
    // in the query must stay and both must highlight.
    const lower = "İstanbul".toLowerCase();
    expect(highlightTerms(`İstanbul and ${lower}`, `İstanbul ${lower}`)).toEqual([
      { text: "İstanbul", match: true },
      { text: " and ", match: false },
      { text: lower, match: true },
    ]);
  });

  it("returns the whole text unflagged when no term qualifies", () => {
    expect(highlightTerms("a short note", "a")).toEqual([{ text: "a short note", match: false }]);
    expect(highlightTerms("nothing here", "goldmark")).toEqual([
      { text: "nothing here", match: false },
    ]);
  });
});
