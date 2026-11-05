import { describe, expect, it } from "vite-plus/test";
import { calmSeriesColor } from "./calmSeriesColor.js";

// Reference OKLab conversion, independent of the module's inverse path, so
// the test checks the output color rather than replaying the same math.
function oklch(hex: string): { l: number; c: number; h: number } {
  const value = Number.parseInt(hex.slice(1), 16);
  const lin = (v: number) => {
    const c = v / 255;
    return c <= 0.04045 ? c / 12.92 : ((c + 0.055) / 1.055) ** 2.4;
  };
  const r = lin((value >> 16) & 0xff);
  const g = lin((value >> 8) & 0xff);
  const b = lin(value & 0xff);
  const l = Math.cbrt(0.4122214708 * r + 0.5363325363 * g + 0.0514459929 * b);
  const m = Math.cbrt(0.2119034982 * r + 0.6806995451 * g + 0.1073969566 * b);
  const s = Math.cbrt(0.0883024619 * r + 0.2817188376 * g + 0.6299787005 * b);
  const L = 0.2104542553 * l + 0.793617785 * m - 0.0040720468 * s;
  const A = 1.9779984951 * l - 2.428592205 * m + 0.4505937099 * s;
  const B = 0.0259040371 * l + 0.7827717662 * m - 0.808675766 * s;
  return { l: L, c: Math.hypot(A, B), h: (Math.atan2(B, A) * 180) / Math.PI };
}

describe("calmSeriesColor", () => {
  it.each(["#2563eb", "#60a5fa", "#e11d48", "#a3e635", "#ff7f0e"])(
    "lowers the chroma of %s and keeps its lightness and hue",
    (hex) => {
      const before = oklch(hex);
      const after = oklch(calmSeriesColor(hex));

      expect(after.c).toBeLessThan(before.c * 0.85);
      expect(after.c).toBeGreaterThan(before.c * 0.7);
      expect(Math.abs(after.l - before.l)).toBeLessThan(0.01);
      expect(Math.abs(after.h - before.h)).toBeLessThan(2);
    },
  );

  it("passes through colors it cannot read", () => {
    expect(calmSeriesColor("var(--accent-blue)")).toBe("var(--accent-blue)");
    expect(calmSeriesColor("rgb(1, 2, 3)")).toBe("rgb(1, 2, 3)");
  });
});
