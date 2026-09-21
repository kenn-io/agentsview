import { afterEach, describe, expect, it, vi } from "vite-plus/test";
import { setLocale } from "../i18n/index.js";
import {
  createRefreshScheduler,
  DEFAULT_REFRESH_INTERVAL_MS,
  formatQueryDuration,
  formatRefreshAge,
  queryDurationWidthSamples,
  refreshAgeWidthSamples,
} from "./refresh.js";

describe("formatRefreshAge", () => {
  const now = Date.parse("2026-06-16T12:10:00Z");

  afterEach(() => {
    setLocale("en");
  });

  it.each([
    { updatedAt: null, expected: "Not updated" },
    {
      updatedAt: Date.parse("2026-06-16T12:09:45Z"),
      expected: "Updated just now",
    },
    {
      updatedAt: Date.parse("2026-06-16T12:08:00Z"),
      expected: "Updated 2m ago",
    },
    {
      updatedAt: Date.parse("2026-06-16T10:00:00Z"),
      expected: "Updated 2h ago",
    },
  ])("returns $expected", ({ updatedAt, expected }) => {
    expect(formatRefreshAge(updatedAt, now)).toBe(expected);
  });

  it("localizes refresh age labels", () => {
    setLocale("zh-CN");

    expect(formatRefreshAge(null, now)).toBe("未更新");
    expect(formatRefreshAge(Date.parse("2026-06-16T12:09:45Z"), now)).toBe("刚刚更新");
    expect(formatRefreshAge(Date.parse("2026-06-16T12:08:00Z"), now)).toBe("2 分钟前更新");
    expect(formatRefreshAge(Date.parse("2026-06-16T10:00:00Z"), now)).toBe("2 小时前更新");
    expect(formatRefreshAge(Date.parse("2026-06-13T10:00:00Z"), now)).toBe("3 天前更新");
  });
});

describe("formatQueryDuration", () => {
  afterEach(() => {
    setLocale("en");
  });

  it.each([
    { durationMs: null, expected: "" },
    { durationMs: undefined, expected: "" },
    { durationMs: Number.NaN, expected: "" },
    { durationMs: -5, expected: "0 ms" },
    { durationMs: 0, expected: "0 ms" },
    { durationMs: 42.4, expected: "42 ms" },
    { durationMs: 999.4, expected: "999 ms" },
    // Rounds across the unit boundary instead of reading "1000 ms".
    { durationMs: 999.6, expected: "1.0 s" },
    { durationMs: 1000, expected: "1.0 s" },
    { durationMs: 1234, expected: "1.2 s" },
    { durationMs: 9950, expected: "10.0 s" },
    { durationMs: 59_940, expected: "59.9 s" },
    // Rounds across the minute boundary instead of reading "60.0 s".
    { durationMs: 59_960, expected: "1m 00s" },
    { durationMs: 60_000, expected: "1m 00s" },
    { durationMs: 65_000, expected: "1m 05s" },
    { durationMs: 125_400, expected: "2m 05s" },
    { durationMs: 3_599_000, expected: "59m 59s" },
    { durationMs: 6_000_000, expected: "100m 00s" },
  ])("formats $durationMs ms as $expected", ({ durationMs, expected }) => {
    expect(formatQueryDuration(durationMs)).toBe(expected);
  });

  it("localizes the duration units", () => {
    setLocale("zh-CN");

    expect(formatQueryDuration(42)).toBe("42 毫秒");
    expect(formatQueryDuration(1234)).toBe("1.2 秒");
    expect(formatQueryDuration(65_000)).toBe("1 分 05 秒");
  });

  it("reserves the widest rendering of every unit", () => {
    expect(queryDurationWidthSamples()).toEqual(["999 ms", "59.9 s", "99m 59s"]);
  });
});

describe("refreshAgeWidthSamples", () => {
  it("covers every age label branch at its widest digit budget", () => {
    expect(refreshAgeWidthSamples()).toEqual([
      "Not updated",
      "Updated just now",
      "Updated 59m ago",
      "Updated 23h ago",
      "Updated 999d ago",
    ]);
  });
});

describe("createRefreshScheduler", () => {
  afterEach(() => {
    vi.useRealTimers();
  });

  it("runs immediately and then at the configured interval", async () => {
    vi.useFakeTimers();
    const refresh = vi.fn();
    const scheduler = createRefreshScheduler(refresh, 300_000);

    scheduler.refreshNow();
    expect(refresh).toHaveBeenCalledTimes(1);

    await vi.advanceTimersByTimeAsync(299_999);
    expect(refresh).toHaveBeenCalledTimes(1);

    await vi.advanceTimersByTimeAsync(1);
    expect(refresh).toHaveBeenCalledTimes(2);

    scheduler.stop();
  });

  it("resets the next automatic refresh after a manual refresh", async () => {
    vi.useFakeTimers();
    const refresh = vi.fn();
    const scheduler = createRefreshScheduler(refresh, 300_000);

    scheduler.refreshNow();
    await vi.advanceTimersByTimeAsync(290_000);
    scheduler.refreshNow();
    expect(refresh).toHaveBeenCalledTimes(2);

    await vi.advanceTimersByTimeAsync(299_999);
    expect(refresh).toHaveBeenCalledTimes(2);

    await vi.advanceTimersByTimeAsync(1);
    expect(refresh).toHaveBeenCalledTimes(3);

    scheduler.stop();
  });

  it("waits one interval before the first deferred refresh", async () => {
    vi.useFakeTimers();
    const refresh = vi.fn();
    const scheduler = createRefreshScheduler(refresh, 300_000);

    scheduler.scheduleNext();
    expect(refresh).toHaveBeenCalledTimes(0);

    await vi.advanceTimersByTimeAsync(299_999);
    expect(refresh).toHaveBeenCalledTimes(0);

    await vi.advanceTimersByTimeAsync(1);
    expect(refresh).toHaveBeenCalledTimes(1);

    await vi.advanceTimersByTimeAsync(300_000);
    expect(refresh).toHaveBeenCalledTimes(2);

    scheduler.stop();
  });

  it("shares a five-minute default cadence", () => {
    expect(DEFAULT_REFRESH_INTERVAL_MS).toBe(5 * 60 * 1000);
  });
});
