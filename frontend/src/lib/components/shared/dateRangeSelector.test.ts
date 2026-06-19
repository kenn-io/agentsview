import { describe, expect, it, vi } from "vite-plus/test";

import {
  allFromDate,
  isPresetActive,
  presetRange,
} from "./dateRangeSelector.js";

describe("date range selector presets", () => {
  it("builds last-n-days ranges ending today", () => {
    vi.setSystemTime(new Date("2026-04-25T12:00:00Z"));

    expect(presetRange(90, "2020-01-01")).toEqual({
      from: "2026-01-25",
      to: "2026-04-25",
    });
  });

  it("uses earliest session date for all-time preset", () => {
    vi.setSystemTime(new Date("2026-04-25T12:00:00Z"));

    expect(presetRange(0, "2024-02-03T04:05:06Z")).toEqual({
      from: "2024-02-03",
      to: "2026-04-25",
    });
  });

  it("falls back to one year ago when all-time date is missing", () => {
    vi.setSystemTime(new Date("2026-04-25T12:00:00Z"));

    expect(allFromDate(null)).toBe("2025-04-25");
  });

  it("marks matching presets active", () => {
    vi.setSystemTime(new Date("2026-04-25T12:00:00Z"));

    expect(isPresetActive("2026-01-25", "2026-04-25", 90, null)).toBe(true);
    expect(isPresetActive("2026-01-26", "2026-04-25", 90, null)).toBe(false);
  });
});
