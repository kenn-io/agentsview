import {
  afterEach,
  beforeEach,
  describe,
  expect,
  it,
  vi,
} from "vite-plus/test";
import { setLocale } from "../i18n/index.js";
import { testMoney } from "../test/money.js";
import {
  formatRelativeTime,
  sanitizeSnippet,
  _resetNonceCounter,
  formatCost,
  formatTokenCount,
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
    [
      "preserves <mark> tags",
      "hello <mark>world</mark> end",
      "hello <mark>world</mark> end",
    ],
    [
      "escapes other HTML tags",
      '<script>alert("xss")</script>',
      '&lt;script&gt;alert("xss")&lt;/script&gt;',
    ],
    [
      "escapes img tags",
      "<img src=x onerror=alert(1)>",
      "&lt;img src=x onerror=alert(1)&gt;",
    ],
    [
      "handles mixed mark and other tags",
      '<b>bold</b> <mark>highlighted</mark> <i>italic</i>',
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
    [
      "returns empty string for empty input",
      "",
      "",
    ],
    [
      "handles plain text without tags",
      "no tags here",
      "no tags here",
    ],
    [
      "escapes angle brackets in content",
      "x < y > z",
      "x &lt; y &gt; z",
    ],
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

describe("formatTokenCount", () => {
  it("returns '0' for zero", () => {
    expect(formatTokenCount(0)).toBe("0");
  });

  it("returns raw number for values under 1000", () => {
    expect(formatTokenCount(1)).toBe("1");
    expect(formatTokenCount(500)).toBe("500");
    expect(formatTokenCount(999)).toBe("999");
  });

  it("formats thousands with 'k' suffix", () => {
    expect(formatTokenCount(1000)).toBe("1k");
    expect(formatTokenCount(1200)).toBe("1.2k");
    expect(formatTokenCount(1250)).toBe("1.2k");
    expect(formatTokenCount(45000)).toBe("45k");
    expect(formatTokenCount(45600)).toBe("45.6k");
    expect(formatTokenCount(999999)).toBe("999.9k");
  });

  it("formats millions with 'M' suffix", () => {
    expect(formatTokenCount(1000000)).toBe("1M");
    expect(formatTokenCount(1200000)).toBe("1.2M");
    expect(formatTokenCount(1250000)).toBe("1.2M");
    expect(formatTokenCount(25000000)).toBe("25M");
    expect(formatTokenCount(25600000)).toBe("25.6M");
  });

  it("drops decimal when it would be .0", () => {
    expect(formatTokenCount(2000)).toBe("2k");
    expect(formatTokenCount(10000)).toBe("10k");
    expect(formatTokenCount(3000000)).toBe("3M");
  });
});

describe("formatTokenUsage", () => {
  it("returns null when no token metrics are reported", () => {
    expect(formatTokenUsage(0, false, 0, false)).toBeNull();
  });

  it("formats both reported token metrics", () => {
    expect(formatTokenUsage(2400, true, 180, true)).toBe(
      "2.4k ctx / 180 out",
    );
  });

  it("renders an explicit placeholder for missing token metrics", () => {
    expect(formatTokenUsage(0, false, 180, true)).toBe(
      "— ctx / 180 out",
    );
    expect(formatTokenUsage(2400, true, 0, false)).toBe(
      "2.4k ctx / — out",
    );
  });
});
