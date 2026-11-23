import { describe, expect, it } from "vite-plus/test";
import { treemapAreas, treemapLabelSize } from "./treemapScale.js";

const TALL = 260;

describe("treemapAreas", () => {
  it("keeps linear areas proportional to the values", () => {
    expect(treemapAreas([100, 1, 0], "linear")).toEqual([100, 1, 0]);
  });

  it("takes the square root on the sqrt scale", () => {
    expect(treemapAreas([100, 1, 0], "sqrt")).toEqual([10, 1, 0]);
  });

  it("gives the smallest value area 1 and adds ln of each ratio on the log scale", () => {
    const [big, small, zero] = treemapAreas([1000, 10, 0], "log");
    // 1 + ln(100)
    expect(big).toBeCloseTo(5.605, 3);
    expect(small).toBe(1);
    expect(zero).toBe(0);
  });

  it("gives the same log areas whatever unit the values use", () => {
    expect(treemapAreas([14_867_000_000, 140_000], "log")).toEqual(
      treemapAreas([14_867, 0.14], "log"),
    );
  });

  it("gives no area when no value is positive", () => {
    expect(treemapAreas([0, -5], "log")).toEqual([0, 0]);
    expect(treemapAreas([0, -5], "sqrt")).toEqual([0, 0]);
    expect(treemapAreas([0, -5], "linear")).toEqual([0, 0]);
  });
});

describe("treemapLabelSize", () => {
  it("grows the label with the square root of the area share", () => {
    expect(treemapLabelSize(100, 100, TALL)).toBe(18);
    // sqrt(0.25) = 0.5, so 11 + 3.5 rounds to 15.
    expect(treemapLabelSize(25, 100, TALL)).toBe(15);
    expect(treemapLabelSize(0, 100, TALL)).toBe(11);
  });

  it("returns the floor when there is no area", () => {
    expect(treemapLabelSize(0, 0, TALL)).toBe(11);
  });

  it("shrinks the label so the lines under it stay inside a short tile", () => {
    expect(treemapLabelSize(100, 100, 45)).toBe(14);
    expect(treemapLabelSize(100, 100, 41)).toBe(11);
  });
});
