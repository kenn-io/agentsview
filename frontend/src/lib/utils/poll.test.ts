import { describe, it, expect, vi, beforeEach, afterEach } from "vite-plus/test";
import { sameValueZero, waitForStableValue } from "./poll.js";

describe("sameValueZero", () => {
  it.each([
    ["equal numbers", 1, 1, true],
    ["different numbers", 1, 2, false],
    ["equal strings", "a", "a", true],
    ["different strings", "a", "b", false],
    ["NaN equals NaN", NaN, NaN, true],
    ["0 equals -0", 0, -0, true],
    ["-0 equals 0", -0, 0, true],
    ["null equals null", null, null, true],
    ["undefined equals undefined", undefined, undefined, true],
    ["null vs undefined", null, undefined, false],
    ["same object reference", Object, Object, true],
  ] as [string, unknown, unknown, boolean][])(
    "%s",
    (_name, a, b, expected) => {
      expect(sameValueZero(a, b)).toBe(expected);
    },
  );

  it("returns false for distinct object instances", () => {
    expect(sameValueZero({}, {})).toBe(false);
  });

  it("returns false for distinct array instances", () => {
    expect(sameValueZero([1], [1])).toBe(false);
  });
});

describe("waitForStableValue", () => {
  beforeEach(() => {
    vi.useFakeTimers();
  });

  afterEach(() => {
    vi.useRealTimers();
  });

  it("resolves when value is immediately stable", async () => {
    const fn = vi.fn().mockResolvedValue(42);

    const promise = waitForStableValue(fn, 50, 10, 500);
    // Advance past a few poll intervals + stable duration
    await vi.advanceTimersByTimeAsync(200);

    await expect(promise).resolves.toBe(42);
  });

  it("waits for value to stop changing", async () => {
    let value = 0;
    const fn = vi.fn(async () => value);

    const promise = waitForStableValue(fn, 50, 10, 2000);

    // Value changes a few times
    await vi.advanceTimersByTimeAsync(10);
    value = 1;
    await vi.advanceTimersByTimeAsync(10);
    value = 2;
    await vi.advanceTimersByTimeAsync(10);
    value = 3;
    // Now let it stabilize
    await vi.advanceTimersByTimeAsync(200);

    await expect(promise).resolves.toBe(3);
  });

  it("throws when value never stabilizes", async () => {
    let counter = 0;
    const fn = vi.fn(async () => counter++);

    const promise = waitForStableValue(fn, 50, 10, 200);
    // Attach rejection handler before advancing to avoid unhandled rejection
    const rejection = expect(promise).rejects.toThrow(
      /did not stabilize within 200ms/,
    );
    await vi.advanceTimersByTimeAsync(300);
    await rejection;
  });

  it("uses custom isEqual comparator", async () => {
    let value = { x: 1 };
    const fn = vi.fn(async () => ({ ...value }));
    const isEqual = (
      a: { x: number },
      b: { x: number },
    ): boolean => a.x === b.x;

    const promise = waitForStableValue(
      fn,
      50,
      10,
      500,
      isEqual,
    );
    await vi.advanceTimersByTimeAsync(200);

    const result = await promise;
    expect(result).toEqual({ x: 1 });
  });

  it("custom comparator detects changes", async () => {
    let value = { x: 1 };
    const fn = vi.fn(async () => ({ ...value }));
    const isEqual = (
      a: { x: number },
      b: { x: number },
    ): boolean => a.x === b.x;

    const promise = waitForStableValue(
      fn,
      50,
      10,
      2000,
      isEqual,
    );

    await vi.advanceTimersByTimeAsync(10);
    value = { x: 2 };
    await vi.advanceTimersByTimeAsync(200);

    await expect(promise).resolves.toEqual({ x: 2 });
  });

  it("defaults pollIntervalMs to 100", async () => {
    const fn = vi.fn().mockResolvedValue(5);

    const promise = waitForStableValue(fn, 50);
    // With default 100ms interval, need enough for a few polls
    await vi.advanceTimersByTimeAsync(500);

    await expect(promise).resolves.toBe(5);
    // First call + polls at ~100ms intervals
    expect(fn.mock.calls.length).toBeGreaterThanOrEqual(2);
    expect(fn.mock.calls.length).toBeLessThanOrEqual(7);
  });

  it("defaults maxWaitMs to stableDurationMs * 3", async () => {
    let counter = 0;
    const fn = vi.fn(async () => counter++);

    const stableDurationMs = 100;
    const promise = waitForStableValue(fn, stableDurationMs, 10);
    const rejection = expect(promise).rejects.toThrow(
      /did not stabilize within 300ms/,
    );
    await vi.advanceTimersByTimeAsync(400);
    await rejection;
  });

  it("handles synchronous fn", async () => {
    const fn = vi.fn(() => "sync");

    const promise = waitForStableValue(fn, 30, 10, 500);
    await vi.advanceTimersByTimeAsync(200);

    await expect(promise).resolves.toBe("sync");
  });

  it("treats NaN as stable via default comparator", async () => {
    const fn = vi.fn().mockResolvedValue(NaN);

    const promise = waitForStableValue(fn, 50, 10, 500);
    await vi.advanceTimersByTimeAsync(200);

    const result = await promise;
    expect(Number.isNaN(result)).toBe(true);
  });

  it("treats 0 and -0 as equal via default comparator", async () => {
    // Alternate between 0 and -0 on every poll. Object.is(0, -0)
    // returns false, so under Object.is the value would never
    // stabilize. SameValueZero treats them as equal, so it should.
    let callCount = 0;
    const fn = vi.fn(() => (callCount++ % 2 === 0 ? 0 : -0));

    const stableMs = 50;
    const pollMs = 10;
    const maxMs = 300;
    const promise = waitForStableValue(fn, stableMs, pollMs, maxMs);
    await vi.advanceTimersByTimeAsync(200);

    await expect(promise).resolves.toBeDefined();
  });

  it("0/-0 alternation times out with Object.is comparator", async () => {
    // Prove the alternating 0/-0 pattern does NOT stabilize when
    // compared with Object.is, confirming the test above is not
    // vacuously passing.
    let callCount = 0;
    const fn = vi.fn(() => (callCount++ % 2 === 0 ? 0 : -0));

    const stableMs = 50;
    const pollMs = 10;
    const maxMs = 300;
    const promise = waitForStableValue(
      fn,
      stableMs,
      pollMs,
      maxMs,
      Object.is,
    );
    const rejection = expect(promise).rejects.toThrow(
      /did not stabilize within 300ms/,
    );
    await vi.advanceTimersByTimeAsync(400);
    await rejection;
  });
});
