import { afterEach, beforeEach, describe, expect, it } from "vite-plus/test";
import { setLocale } from "./i18n/index.js";
import {
  formatMoney,
  formatSignedMoney,
  type Money,
} from "./money.js";
import { costDisplay } from "./stores/costDisplay.svelte.js";
import { testMoney } from "./test/money.js";

beforeEach(() => {
  setLocale("en");
  costDisplay.setPreference("USD", null);
});

afterEach(() => {
  setLocale("en");
  costDisplay.setPreference("USD", null);
});

describe("formatMoney", () => {
  it.each([
    [0, "$0.00"],
    [0.004, "<$0.01"],
    [0.01, "$0.01"],
    [12.345, "$12.35"],
    [99.994, "$99.99"],
    [100, "$100"],
  ])("keeps USD output for %d", (value, expected) => {
    expect(formatMoney(testMoney(value))).toBe(expected);
  });

  it("converts before applying display thresholds", () => {
    costDisplay.setPreference("EUR", 0.5);

    expect(formatMoney(testMoney(10))).toBe("€5.00");
    expect(formatMoney(testMoney(0.012))).toBe("<€0.01");
    expect(formatMoney(testMoney(-0.012))).toBe(">-€0.01");
    expect(formatMoney(testMoney(200))).toBe("€100");
  });

  it("preserves signed output and the source money object", () => {
    const value: Money = testMoney(10);
    costDisplay.setPreference("EUR", 0.9);

    expect(formatSignedMoney(value)).toBe("+€9.00");
    expect(formatSignedMoney(testMoney(-10))).toBe("-€9.00");
    expect(value).toEqual({ microdollars: 10_000_000 });
  });

  it("keeps currency independent from the interface locale", () => {
    setLocale("fr");
    expect(formatMoney(testMoney(1.23))).toContain("$US");

    setLocale("en");
    costDisplay.setPreference("EUR", 0.9);
    expect(formatMoney(testMoney(1.23))).toBe("€1.11");
  });
});
