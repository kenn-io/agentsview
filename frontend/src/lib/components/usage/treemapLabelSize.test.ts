import { describe, expect, it } from "vite-plus/test";
import { treemapLabelSize, type TreemapLabelScale } from "./treemapLabelSize.js";

const TALL = 260;

describe("treemapLabelSize", () => {
  it.each<[TreemapLabelScale, number, number]>([
    // share 0.25 of 100
    ["linear", 25, 13],
    ["sqrt", 25, 15],
    // ln(27) / ln(102)
    ["log", 25, 16],
  ])("maps a quarter share on the %s scale", (scale, value, size) => {
    expect(treemapLabelSize(value, 100, TALL, scale)).toBe(size);
  });

  it.each<TreemapLabelScale>(["linear", "sqrt", "log"])(
    "gives the whole total 18px on the %s scale",
    (scale) => {
      expect(treemapLabelSize(100, 100, TALL, scale)).toBe(18);
    },
  );

  it.each<[TreemapLabelScale, number]>([
    ["linear", 11],
    ["sqrt", 11],
    // ln(2) / ln(102): the +2 offset lifts a zero value off the floor.
    ["log", 12],
  ])("sizes a zero value on the %s scale", (scale, size) => {
    expect(treemapLabelSize(0, 100, TALL, scale)).toBe(size);
  });

  it("keeps log sizes finite for zero and sub-unit totals", () => {
    expect(treemapLabelSize(0, 0.5, TALL, "log")).toBe(16);
    expect(treemapLabelSize(0.5, 0.5, TALL, "log")).toBe(18);
  });

  it("returns the floor when there is no total", () => {
    expect(treemapLabelSize(0, 0, TALL, "sqrt")).toBe(11);
  });

  it("shrinks the label so the lines under it stay inside a short tile", () => {
    expect(treemapLabelSize(100, 100, 45, "sqrt")).toBe(14);
    expect(treemapLabelSize(100, 100, 41, "sqrt")).toBe(11);
  });
});
