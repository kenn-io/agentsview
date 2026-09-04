import { afterEach, describe, expect, it, vi } from "vite-plus/test";
import { setLocale } from "../i18n/index.js";
import {
  createRefreshScheduler,
  DEFAULT_REFRESH_INTERVAL_MS,
  formatRefreshAge,
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
