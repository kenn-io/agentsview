import { afterEach, describe, expect, it, vi } from "vite-plus/test";
import { daysAgo, localDateStr, today } from "./dates.js";

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
});
