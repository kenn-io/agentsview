import { describe, expect, it } from "vite-plus/test";
import { EtaEstimator } from "./etaEstimator.js";

const KEY = "build-1|2026-07-11T10:00:00Z|embedding";

describe("EtaEstimator", () => {
  it("stays estimating until two positive-progress samples exist", () => {
    const est = new EtaEstimator();

    const first = est.sample(KEY, 0, 1000, 0);
    expect(first.ready).toBe(false);
    expect(first.etaMs).toBeNull();
    expect(first.ratePerSecond).toBeNull();

    const second = est.sample(KEY, 100, 1000, 1000);
    expect(second.ready).toBe(false);

    const third = est.sample(KEY, 200, 1000, 2000);
    expect(third.ready).toBe(true);
  });

  it("reports rate and ETA from a steady 100 units/second run", () => {
    const est = new EtaEstimator();
    est.sample(KEY, 0, 1000, 0);
    est.sample(KEY, 100, 1000, 1000);
    const got = est.sample(KEY, 200, 1000, 2000);

    // 100 units per 1000ms on both deltas: the EWMA of a constant is that
    // constant, so 800 remaining at 0.1 units/ms is 8000ms.
    expect(got.ready).toBe(true);
    expect(got.ratePerSecond).toBeCloseTo(100, 6);
    expect(got.etaMs).toBeCloseTo(8000, 6);
  });

  it("smooths a burst instead of adopting its instantaneous rate", () => {
    const est = new EtaEstimator();
    est.sample(KEY, 0, 1000, 0);
    est.sample(KEY, 100, 1000, 1000);
    est.sample(KEY, 200, 1000, 2000);
    // Burst: 300 units in one second (0.3/ms) against an EWMA of 0.1/ms.
    // With smoothing 0.3: 0.3*0.3 + 0.7*0.1 = 0.16/ms, not 0.3/ms.
    const got = est.sample(KEY, 500, 1000, 3000);

    expect(got.ratePerSecond).toBeCloseTo(160, 6);
    expect(got.etaMs).toBeCloseTo(500 / 0.16, 3);
  });

  it("keeps the last estimate through a stalled poll, then counts the stall against the resumed rate", () => {
    const est = new EtaEstimator();
    est.sample(KEY, 0, 1000, 0);
    est.sample(KEY, 100, 1000, 1000);
    est.sample(KEY, 200, 1000, 2000);

    const stalled = est.sample(KEY, 200, 1000, 4000);
    expect(stalled.ready).toBe(true);
    expect(stalled.ratePerSecond).toBeCloseTo(100, 6);

    // Progress resumes: 100 units spread over the full 3000ms since the
    // last progress, so the delta's rate is 100/3000 ≈ 0.0333/ms and the
    // EWMA becomes 0.3*0.0333 + 0.7*0.1 = 0.08/ms.
    const resumed = est.sample(KEY, 300, 1000, 5000);
    expect(resumed.ratePerSecond).toBeCloseTo(80, 3);
    expect(resumed.etaMs).toBeCloseTo(700 / 0.08, 2);
  });

  it("resets when progress moves backwards", () => {
    const est = new EtaEstimator();
    est.sample(KEY, 0, 1000, 0);
    est.sample(KEY, 100, 1000, 1000);
    est.sample(KEY, 200, 1000, 2000);

    const regressed = est.sample(KEY, 50, 1000, 3000);
    expect(regressed.ready).toBe(false);
    expect(regressed.etaMs).toBeNull();

    // The regressed sample is the new baseline; two positive samples from
    // there re-arm the estimator with only post-reset rates.
    est.sample(KEY, 150, 1000, 4000);
    const rearmed = est.sample(KEY, 250, 1000, 5000);
    expect(rearmed.ready).toBe(true);
    expect(rearmed.ratePerSecond).toBeCloseTo(100, 6);
  });

  it("resets when the total changes", () => {
    const est = new EtaEstimator();
    est.sample(KEY, 0, 1000, 0);
    est.sample(KEY, 100, 1000, 1000);
    est.sample(KEY, 200, 1000, 2000);

    const got = est.sample(KEY, 300, 2000, 3000);
    expect(got.ready).toBe(false);
  });

  it("resets when the key (build/phase identity) changes", () => {
    const est = new EtaEstimator();
    est.sample(KEY, 0, 1000, 0);
    est.sample(KEY, 100, 1000, 1000);
    est.sample(KEY, 200, 1000, 2000);

    const other = est.sample("build-2|2026-07-11T11:00:00Z|embedding", 300, 1000, 3000);
    expect(other.ready).toBe(false);
  });

  it("never reports an ETA while the total is unknown (scanning)", () => {
    const est = new EtaEstimator();
    est.sample("b|s|scanning", 0, 0, 0);
    est.sample("b|s|scanning", 5, 0, 1000);
    const got = est.sample("b|s|scanning", 10, 0, 2000);
    expect(got.ready).toBe(false);
    expect(got.etaMs).toBeNull();
  });

  it("ignores zero-elapsed duplicate samples instead of dividing by zero", () => {
    const est = new EtaEstimator();
    est.sample(KEY, 0, 1000, 0);
    est.sample(KEY, 100, 1000, 1000);
    est.sample(KEY, 200, 1000, 2000);

    const dup = est.sample(KEY, 250, 1000, 2000);
    expect(dup.ready).toBe(true);
    expect(dup.ratePerSecond).toBeCloseTo(100, 6);
    expect(Number.isFinite(dup.etaMs ?? NaN)).toBe(true);
  });

  it("clamps the ETA at zero when done overshoots total", () => {
    const est = new EtaEstimator();
    est.sample(KEY, 0, 1000, 0);
    est.sample(KEY, 600, 1000, 1000);
    const got = est.sample(KEY, 1100, 1000, 2000);
    expect(got.ready).toBe(true);
    expect(got.etaMs).toBe(0);
  });

  it("reset() drops all accumulated state", () => {
    const est = new EtaEstimator();
    est.sample(KEY, 0, 1000, 0);
    est.sample(KEY, 100, 1000, 1000);
    est.sample(KEY, 200, 1000, 2000);
    est.reset();

    const got = est.sample(KEY, 300, 1000, 3000);
    expect(got.ready).toBe(false);
  });
});
