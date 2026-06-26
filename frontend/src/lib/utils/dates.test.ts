import { afterEach, describe, expect, it, vi } from "vite-plus/test";
import {
  addDays,
  daysAgo,
  endOfMonth,
  localDateStr,
  parseLocalDate,
  rollingRange,
  startOfIsoWeek,
  startOfMonth,
  today,
} from "./dates.js";

describe("date helpers", () => {
  afterEach(() => vi.useRealTimers());

  it("formats local dates as YYYY-MM-DD", () => {
    expect(localDateStr(new Date(2024, 5, 7))).toBe("2024-06-07");
  });

  it("computes today and daysAgo from local time", () => {
    vi.useFakeTimers();
    vi.setSystemTime(new Date(2024, 5, 10, 12, 0, 0));
    expect(today()).toBe("2024-06-10");
    expect(daysAgo(7)).toBe("2024-06-03");
  });

  it("computes rolling ranges from an explicit clock", () => {
    expect(rollingRange(30, new Date(2024, 5, 10, 12, 0, 0))).toEqual({
      from: "2024-05-11",
      to: "2024-06-10",
    });
  });

  it("parses and offsets local date strings", () => {
    expect(parseLocalDate("2024-02-03")?.getDate()).toBe(3);
    expect(parseLocalDate("not-a-date")).toBeNull();
    expect(addDays("2024-02-27", 2)).toBe("2024-02-29");
    expect(endOfMonth("2024-02-03")).toBe("2024-02-29");
  });

  it("expands anchored activity presets to backend-equivalent local bounds", () => {
    expect(startOfIsoWeek("2026-06-17")).toBe("2026-06-15");
    expect(startOfIsoWeek("2026-06-21")).toBe("2026-06-15");
    expect(startOfMonth("2026-02-14")).toBe("2026-02-01");
  });
});
