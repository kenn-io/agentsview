// Progress-rate estimator for polled counter-style progress (done/total),
// tqdm-style: the instantaneous rate of each positive progress delta is
// folded into an exponentially weighted moving average, so bursty batches
// smooth out instead of whipsawing the ETA. Stalled polls (no new progress)
// deliberately do not advance the sample baseline: when progress resumes,
// the stall time is counted against the resumed delta's rate, keeping the
// estimate honest across temporary stalls.
//
// The estimator is keyed by the identity of the thing being measured (for
// embeddings builds: build id + start time + phase). A key change, a total
// change, or progress moving backwards all reset the estimator — two
// different builds or phases must never be treated as one continuous run.

/** Smoothing factor for the EWMA over per-delta rates (tqdm's default 0.3:
 * higher weighs recent deltas more). */
const SMOOTHING = 0.3;

/** Positive-progress samples required before an ETA is reported; below
 * this the estimator stays in the "estimating" state. */
const MIN_POSITIVE_SAMPLES = 2;

export interface EtaEstimate {
  /** False until enough positive-progress samples exist for a stable rate. */
  ready: boolean;
  /** Smoothed throughput in units/second; null while not ready. */
  ratePerSecond: number | null;
  /** Estimated remaining milliseconds; null while not ready or total <= 0. */
  etaMs: number | null;
}

const ESTIMATING: EtaEstimate = { ready: false, ratePerSecond: null, etaMs: null };

export class EtaEstimator {
  private key: string | null = null;
  private lastDone = 0;
  private lastTotal = 0;
  private lastTimeMs = 0;
  private emaRatePerMs: number | null = null;
  private positiveSamples = 0;

  /**
   * Feed one polled progress observation. `timeMs` must come from a
   * monotonic clock (performance.now()), never wall time. Returns the
   * current estimate after incorporating the sample.
   */
  sample(key: string, done: number, total: number, timeMs: number): EtaEstimate {
    if (this.key !== key || total !== this.lastTotal || done < this.lastDone) {
      this.resetTo(key, done, total, timeMs);
      return this.estimate();
    }
    const elapsedMs = timeMs - this.lastTimeMs;
    const delta = done - this.lastDone;
    if (delta > 0 && elapsedMs > 0) {
      const rate = delta / elapsedMs;
      this.emaRatePerMs =
        this.emaRatePerMs === null
          ? rate
          : SMOOTHING * rate + (1 - SMOOTHING) * this.emaRatePerMs;
      this.positiveSamples += 1;
      this.lastDone = done;
      this.lastTimeMs = timeMs;
    }
    return this.estimate();
  }

  /** Drop all state, e.g. when the build being watched goes away. */
  reset(): void {
    this.key = null;
    this.lastDone = 0;
    this.lastTotal = 0;
    this.lastTimeMs = 0;
    this.emaRatePerMs = null;
    this.positiveSamples = 0;
  }

  private resetTo(key: string, done: number, total: number, timeMs: number): void {
    this.key = key;
    this.lastDone = done;
    this.lastTotal = total;
    this.lastTimeMs = timeMs;
    this.emaRatePerMs = null;
    this.positiveSamples = 0;
  }

  private estimate(): EtaEstimate {
    if (
      this.emaRatePerMs === null ||
      this.emaRatePerMs <= 0 ||
      this.positiveSamples < MIN_POSITIVE_SAMPLES ||
      this.lastTotal <= 0
    ) {
      return ESTIMATING;
    }
    const remaining = Math.max(0, this.lastTotal - this.lastDone);
    return {
      ready: true,
      ratePerSecond: this.emaRatePerMs * 1000,
      etaMs: remaining / this.emaRatePerMs,
    };
  }
}
