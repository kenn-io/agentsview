// Label/stepping helpers (rangeLabel, calendarLabel, stepAnchor,
// defaultForMode) moved into kit-ui's RangePicker with the shared-component
// migration; only the app-side selection/resolution semantics live here now.
import { describe, expect, it, vi } from "vite-plus/test";

import {
  periodBounds,
  resolveRange,
  selectionFromRange,
  selectionFromWindow,
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
