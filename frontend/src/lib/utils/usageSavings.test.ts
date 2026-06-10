import { describe, it, expect } from "vite-plus/test";
import { savingsState } from "./usageSavings.js";

describe("savingsState", () => {
  it("returns 'saved' for positive values >= half a cent", () => {
    expect(savingsState(0.005)).toBe("saved");
    expect(savingsState(0.01)).toBe("saved");
    expect(savingsState(2.7)).toBe("saved");
    expect(savingsState(1_000_000)).toBe("saved");
  });

  it("returns 'costlier' for negative values <= -half a cent", () => {
    // Write-heavy workloads: creation premium > read discount.
    expect(savingsState(-0.005)).toBe("costlier");
    expect(savingsState(-0.01)).toBe("costlier");
    expect(savingsState(-0.75)).toBe("costlier");
    expect(savingsState(-42)).toBe("costlier");
  });

  it("returns 'none' for exactly zero", () => {
    expect(savingsState(0)).toBe("none");
    expect(savingsState(-0)).toBe("none");
  });

  it(
    "returns 'none' for sub-cent deltas that would render $0.00",
    () => {
      // These would format as "$0.00 more/saved than uncached"
      // and look broken. Suppress the badge entirely instead.
      expect(savingsState(0.001)).toBe("none");
      expect(savingsState(0.004)).toBe("none");
      expect(savingsState(-0.001)).toBe("none");
      expect(savingsState(-0.004999)).toBe("none");
    },
  );
});
