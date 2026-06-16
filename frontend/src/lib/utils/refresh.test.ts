import {
  afterEach,
  describe,
  expect,
  it,
  vi,
} from "vite-plus/test";
import {
  createRefreshScheduler,
  formatRefreshAge,
} from "./refresh.js";

describe("formatRefreshAge", () => {
  const now = Date.parse("2026-06-16T12:10:00Z");

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
});

describe("createRefreshScheduler", () => {
  afterEach(() => {
    vi.useRealTimers();
  });

  it("runs immediately and then at the configured interval", async () => {
    vi.useFakeTimers();
    const refresh = vi.fn();
    const scheduler = createRefreshScheduler(refresh, 300_000);

    scheduler.start();
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

    scheduler.start();
    await vi.advanceTimersByTimeAsync(290_000);
    scheduler.refreshNow();
    expect(refresh).toHaveBeenCalledTimes(2);

    await vi.advanceTimersByTimeAsync(299_999);
    expect(refresh).toHaveBeenCalledTimes(2);

    await vi.advanceTimersByTimeAsync(1);
    expect(refresh).toHaveBeenCalledTimes(3);

    scheduler.stop();
  });
});
