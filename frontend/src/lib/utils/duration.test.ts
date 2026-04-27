import { describe, expect, it } from "vitest";
import { formatDuration } from "./duration.js";

describe("formatDuration", () => {
  it("formats sub-second values as ms", () => {
    expect(formatDuration(0)).toBe("0ms");
    expect(formatDuration(8)).toBe("8ms");
    expect(formatDuration(312)).toBe("312ms");
    expect(formatDuration(999)).toBe("999ms");
  });

  it("formats sub-minute values as one-decimal seconds", () => {
    expect(formatDuration(1000)).toBe("1.0s");
    expect(formatDuration(2400)).toBe("2.4s");
    expect(formatDuration(28400)).toBe("28.4s");
    expect(formatDuration(59999)).toBe("59.9s");
  });

  it("formats sub-hour values as `Nm Ss`", () => {
    expect(formatDuration(60_000)).toBe("1m 0s");
    expect(formatDuration(138_000)).toBe("2m 18s");
    expect(formatDuration(3_599_000)).toBe("59m 59s");
  });

  it("formats hour-plus values as `Nh Mm`", () => {
    expect(formatDuration(3_600_000)).toBe("1h 0m");
    expect(formatDuration(4_320_000)).toBe("1h 12m");
    expect(formatDuration(86_400_000)).toBe("24h 0m");
  });

  it("treats negative or NaN as the dash sentinel", () => {
    expect(formatDuration(-1)).toBe("—");
    expect(formatDuration(Number.NaN)).toBe("—");
  });
});
