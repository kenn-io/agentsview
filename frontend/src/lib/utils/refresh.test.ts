import { afterEach, describe, expect, it, vi } from "vite-plus/test";
import { setLocale } from "../i18n/index.js";
import {
  createRefreshScheduler,
  DEFAULT_REFRESH_INTERVAL_MS,
  formatQueryDuration,
  formatQueryTick,
  formatRefreshAge,
  formatRefreshStatus,
  queryAxisTicks,
  queryDurationWidthSamples,
  querySegmentsFrom,
  queryStepFrom,
  refreshAgeWidthSamples,
  refreshStatusWidthSamples,
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
    { durationMs: 999.6, expected: "1 s" },
    { durationMs: 1000, expected: "1 s" },
    // Whole seconds only: sub-second detail adds nothing at this scale.
    { durationMs: 1499, expected: "1 s" },
    { durationMs: 1500, expected: "2 s" },
    { durationMs: 9950, expected: "10 s" },
    { durationMs: 59_400, expected: "59 s" },
    // Rounds across the minute boundary instead of reading "60 s".
    { durationMs: 59_600, expected: "1m 00s" },
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
    expect(formatQueryDuration(1234)).toBe("1 秒");
    expect(formatQueryDuration(65_000)).toBe("1 分 05 秒");
  });

  it("reserves the widest rendering of every unit", () => {
    expect(queryDurationWidthSamples()).toEqual(["999 ms", "59 s", "99m 59s"]);
  });
});

describe("formatRefreshStatus", () => {
  const now = Date.parse("2026-06-16T12:10:00Z");

  afterEach(() => {
    setLocale("en");
  });

  it("appends the last-query duration to the age as one phrase", () => {
    expect(formatRefreshStatus(Date.parse("2026-06-16T12:09:45Z"), 2400, now)).toBe(
      "Updated just now · 2 s",
    );
    expect(formatRefreshStatus(Date.parse("2026-06-16T12:07:00Z"), 44, now)).toBe(
      "Updated 3m ago · 44 ms",
    );
  });

  it("shows the age alone before a query has completed", () => {
    expect(formatRefreshStatus(null, null, now)).toBe("Not updated");
    expect(formatRefreshStatus(Date.parse("2026-06-16T12:09:45Z"), null, now)).toBe(
      "Updated just now",
    );
  });

  it("localizes the phrase and its separator", () => {
    setLocale("ja");
    expect(formatRefreshStatus(Date.parse("2026-06-16T12:07:00Z"), 2400, now)).toBe(
      "3 分前に更新されました・2 秒",
    );
  });

  it("reserves every age variant paired with every duration unit", () => {
    const samples = refreshStatusWidthSamples();
    expect(samples).toHaveLength(15);
    expect(samples).toContain("Not updated · 999 ms");
    expect(samples).toContain("Updated 999d ago · 99m 59s");
  });
});

describe("query timeline helpers", () => {
  it.each([
    { axisMs: 154, ticks: [0, 50, 100, 150] },
    { axisMs: 2000, ticks: [0, 500, 1000, 1500, 2000] },
    // A running query's one-second minimum axis: "200 ms" labels every
    // fifth of the track would run into each other.
    { axisMs: 1000, ticks: [0, 500, 1000] },
    { axisMs: 9, ticks: [0, 5] },
    // Never finer than a millisecond: labels are whole milliseconds.
    { axisMs: 2, ticks: [0, 1, 2] },
    { axisMs: 0, ticks: [0] },
  ])("spaces axis ticks for $axisMs ms", ({ axisMs, ticks }) => {
    expect(queryAxisTicks(axisMs)).toEqual(ticks);
  });

  it("labels ticks exactly instead of rounding them", () => {
    expect(formatQueryTick(0)).toBe("0");
    expect(formatQueryTick(50)).toBe("50 ms");
    expect(formatQueryTick(1500)).toBe("1.5 s");
  });

  it("splits a request into wait, download, and apply phases from the query start", () => {
    const timing = { sentAt: 1010, headersAt: 1090, bodyAt: 1100 };
    expect(querySegmentsFrom(timing, 1104, 1000)).toEqual([
      { phase: "wait", startMs: 10, durationMs: 80 },
      { phase: "download", startMs: 90, durationMs: 10 },
      { phase: "apply", startMs: 100, durationMs: 4 },
    ]);
    expect(queryStepFrom("summary", timing, 1005, 1104, 1000)).toEqual({
      name: "summary",
      startMs: 10,
      durationMs: 94,
      segments: querySegmentsFrom(timing, 1104, 1000),
    });
  });

  it("starts the apply phase later when the store waited for a sibling request", () => {
    const timing = { sentAt: 1010, headersAt: 1090, bodyAt: 1100 };
    expect(queryStepFrom("summary", timing, 1005, 1160, 1000, 1150).segments).toEqual([
      { phase: "wait", startMs: 10, durationMs: 80 },
      { phase: "download", startMs: 90, durationMs: 10 },
      { phase: "apply", startMs: 150, durationMs: 10 },
    ]);
  });

  it("falls back to the caller's own start when a response carried no timing", () => {
    expect(queryStepFrom("summary", undefined, 1005, 1104, 1000)).toEqual({
      name: "summary",
      startMs: 5,
      durationMs: 99,
    });
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
