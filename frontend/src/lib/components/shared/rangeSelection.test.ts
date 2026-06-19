import { describe, expect, it, vi } from "vite-plus/test";

import {
  calendarLabel,
  defaultForMode,
  periodBounds,
  rangeLabel,
  resolveRange,
  selectionFromRange,
  selectionFromWindow,
  stepAnchor,
  type RangeSelection,
} from "./rangeSelection.js";

describe("periodBounds", () => {
  it("returns the single day for unit=day", () => {
    expect(periodBounds("day", "2026-06-17")).toEqual({
      from: "2026-06-17",
      to: "2026-06-17",
    });
  });

  it("returns Monday-Sunday for the ISO week containing a midweek anchor", () => {
    expect(periodBounds("week", "2026-06-17")).toEqual({
      from: "2026-06-15",
      to: "2026-06-21",
    });
  });

  it("treats a Sunday anchor as the end of its week", () => {
    expect(periodBounds("week", "2026-03-15")).toEqual({
      from: "2026-03-09",
      to: "2026-03-15",
    });
  });

  it("returns first-to-last day for unit=month", () => {
    expect(periodBounds("month", "2026-02-14")).toEqual({
      from: "2026-02-01",
      to: "2026-02-28",
    });
  });
});

describe("stepAnchor", () => {
  it("steps a day", () => {
    expect(stepAnchor("day", "2026-06-17", 1)).toBe("2026-06-18");
    expect(stepAnchor("day", "2026-06-17", -1)).toBe("2026-06-16");
  });

  it("steps a week by seven days", () => {
    expect(stepAnchor("week", "2026-06-17", 1)).toBe("2026-06-24");
    expect(stepAnchor("week", "2026-06-17", -1)).toBe("2026-06-10");
  });

  it("steps a month and clamps the day to the target month", () => {
    expect(stepAnchor("month", "2026-01-31", 1)).toBe("2026-02-28");
    expect(stepAnchor("month", "2026-03-15", -1)).toBe("2026-02-15");
  });
});

describe("resolveRange", () => {
  it("resolves a relative window ending today", () => {
    vi.setSystemTime(new Date("2026-04-25T12:00:00Z"));
    expect(resolveRange({ mode: "relative", days: 30 })).toEqual({
      from: "2026-03-26",
      to: "2026-04-25",
    });
  });

  it("resolves the all-time window to the earliest session", () => {
    vi.setSystemTime(new Date("2026-04-25T12:00:00Z"));
    expect(
      resolveRange({ mode: "relative", days: 0 }, "2024-02-03T00:00:00Z"),
    ).toEqual({ from: "2024-02-03", to: "2026-04-25" });
  });

  it("resolves a calendar week to its bounds", () => {
    expect(
      resolveRange({ mode: "calendar", unit: "week", anchor: "2026-06-17" }),
    ).toEqual({ from: "2026-06-15", to: "2026-06-21" });
  });

  it("passes a custom range through unchanged", () => {
    expect(
      resolveRange({ mode: "custom", from: "2026-01-01", to: "2026-01-31" }),
    ).toEqual({ from: "2026-01-01", to: "2026-01-31" });
  });
});

describe("calendarLabel", () => {
  it("labels each unit", () => {
    expect(calendarLabel("day", "2026-06-17")).toBe("Jun 17, 2026");
    expect(calendarLabel("week", "2026-06-17")).toBe("Week of Jun 15");
    expect(calendarLabel("month", "2026-02-14")).toBe("February 2026");
  });
});

describe("rangeLabel", () => {
  it("labels relative presets and an unknown day count", () => {
    expect(rangeLabel({ mode: "relative", days: 30 })).toBe("Last 30 days");
    expect(rangeLabel({ mode: "relative", days: 0 })).toBe("All time");
    expect(rangeLabel({ mode: "relative", days: 14 })).toBe("Last 14 days");
  });

  it("labels calendar and custom selections", () => {
    expect(
      rangeLabel({ mode: "calendar", unit: "week", anchor: "2026-06-17" }),
    ).toBe("Week of Jun 15");
    expect(
      rangeLabel({ mode: "custom", from: "2026-05-20", to: "2026-06-19" }),
    ).toBe("May 20 - Jun 19");
    expect(
      rangeLabel({ mode: "custom", from: "2026-06-19", to: "2026-06-19" }),
    ).toBe("Jun 19");
  });
});

describe("defaultForMode", () => {
  const custom: RangeSelection = {
    mode: "custom",
    from: "2026-05-20",
    to: "2026-06-19",
  };

  it("seeds calendar from the current range end", () => {
    expect(defaultForMode("calendar", custom)).toEqual({
      mode: "calendar",
      unit: "week",
      anchor: "2026-06-19",
    });
  });

  it("seeds custom from the current resolved range", () => {
    expect(
      defaultForMode("custom", {
        mode: "calendar",
        unit: "week",
        anchor: "2026-06-17",
      }),
    ).toEqual({ mode: "custom", from: "2026-06-15", to: "2026-06-21" });
  });

  it("defaults relative to 30 days", () => {
    expect(defaultForMode("relative", custom)).toEqual({
      mode: "relative",
      days: 30,
    });
  });
});

describe("selectionFromWindow", () => {
  it("maps a non-pinned window to a relative preset", () => {
    expect(
      selectionFromWindow({
        isPinned: false,
        windowDays: 30,
        from: "2026-05-20",
        to: "2026-06-19",
      }),
    ).toEqual({ mode: "relative", days: 30 });
  });

  it("shows a pinned all-time range as the All preset", () => {
    vi.setSystemTime(new Date("2026-04-25T12:00:00Z"));
    expect(
      selectionFromWindow({
        isPinned: true,
        windowDays: 0,
        from: "2024-02-03",
        to: "2026-04-25",
        earliestSession: "2024-02-03T00:00:00Z",
      }),
    ).toEqual({ mode: "relative", days: 0 });
  });

  it("shows any other pinned range as custom", () => {
    vi.setSystemTime(new Date("2026-04-25T12:00:00Z"));
    expect(
      selectionFromWindow({
        isPinned: true,
        windowDays: 0,
        from: "2026-01-01",
        to: "2026-01-31",
        earliestSession: "2024-02-03T00:00:00Z",
      }),
    ).toEqual({ mode: "custom", from: "2026-01-01", to: "2026-01-31" });
  });
});

describe("selectionFromRange", () => {
  it("recognizes a span matching a relative preset", () => {
    vi.setSystemTime(new Date("2026-04-25T12:00:00Z"));
    expect(selectionFromRange("2025-04-25", "2026-04-25")).toEqual({
      mode: "relative",
      days: 365,
    });
  });

  it("falls back to custom for an arbitrary span", () => {
    vi.setSystemTime(new Date("2026-04-25T12:00:00Z"));
    expect(selectionFromRange("2026-02-01", "2026-02-14")).toEqual({
      mode: "custom",
      from: "2026-02-01",
      to: "2026-02-14",
    });
  });
});
