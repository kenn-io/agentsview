import { describe, expect, it } from "vite-plus/test";
import { chartSeriesColorMap, DEFAULT_CHART_PALETTE, isChartPalette } from "./chartPalette.js";

const ids = (count: number) =>
  Array.from({ length: count }, (_, i) => `series-${String(i).padStart(2, "0")}`);

const EXPECTED_TAB20 = [
  "#1f77b4",
  "#aec7e8",
  "#ff7f0e",
  "#ffbb78",
  "#2ca02c",
  "#98df8a",
  "#d62728",
  "#ff9896",
  "#9467bd",
  "#c5b0d5",
  "#8c564b",
  "#c49c94",
  "#e377c2",
  "#f7b6d2",
  "#bcbd22",
  "#dbdb8d",
  "#17becf",
  "#9edae5",
] as const;

const EXPECTED_TAB20B_AND_TAB20C = [
  "#393b79",
  "#5254a3",
  "#6b6ecf",
  "#9c9ede",
  "#637939",
  "#8ca252",
  "#b5cf6b",
  "#cedb9c",
  "#8c6d31",
  "#bd9e39",
  "#e7ba52",
  "#e7cb94",
  "#843c39",
  "#ad494a",
  "#d6616b",
  "#e7969c",
  "#7b4173",
  "#a55194",
  "#ce6dbd",
  "#de9ed6",
  "#3182bd",
  "#6baed6",
  "#9ecae1",
  "#c6dbef",
  "#e6550d",
  "#fd8d3c",
  "#fdae6b",
  "#fdd0a2",
  "#31a354",
  "#74c476",
  "#a1d99b",
  "#c7e9c0",
  "#756bb1",
  "#9e9ac8",
  "#bcbddc",
  "#dadaeb",
] as const;

describe("chartSeriesColorMap", () => {
  it("uses gray-free tab10 in Matplotlib order", () => {
    const colors = chartSeriesColorMap(ids(9), "matplotlib");
    expect([...colors.values()]).toEqual([
      "#1f77b4",
      "#ff7f0e",
      "#2ca02c",
      "#d62728",
      "#9467bd",
      "#8c564b",
      "#e377c2",
      "#bcbd22",
      "#17becf",
    ]);
  });

  it("uses exact families through their advertised capacities", () => {
    expect([...chartSeriesColorMap(ids(10), "matplotlib").values()]).toEqual(
      EXPECTED_TAB20.slice(0, 10),
    );
    expect([...chartSeriesColorMap(ids(18), "matplotlib").values()]).toEqual(EXPECTED_TAB20);
    expect([...chartSeriesColorMap(ids(19), "matplotlib").values()]).toEqual(
      EXPECTED_TAB20B_AND_TAB20C.slice(0, 19),
    );
    expect([...chartSeriesColorMap(ids(36), "matplotlib").values()]).toEqual(
      EXPECTED_TAB20B_AND_TAB20C,
    );
  });

  it("keeps every advertised Matplotlib family gray-free", () => {
    const isAchromatic = (hex: string) => {
      const red = hex.slice(1, 3);
      const green = hex.slice(3, 5);
      const blue = hex.slice(5, 7);
      return red === green && green === blue;
    };
    for (const count of [9, 18, 36]) {
      expect([...chartSeriesColorMap(ids(count), "matplotlib").values()].some(isAchromatic)).toBe(
        false,
      );
    }
  });

  it("uses all 36 colors before cycling", () => {
    const atCapacity = chartSeriesColorMap(ids(36), "matplotlib");
    expect(new Set(atCapacity.values()).size).toBe(36);
    const overflow = chartSeriesColorMap(ids(37), "matplotlib");
    expect(overflow.get("series-36")).toBe(overflow.get("series-00"));
  });

  it("is stable across permutations and keeps Other muted", () => {
    const input = ["zeta", "__other__", "alpha", "zeta"];
    expect([...chartSeriesColorMap(input, "matplotlib")]).toEqual([
      ...chartSeriesColorMap([...input].reverse(), "matplotlib"),
    ]);
    expect(chartSeriesColorMap(input, "matplotlib").get("__other__")).toBe("var(--text-muted)");
  });

  it("preserves the supplied agentsview colors", () => {
    const colors = chartSeriesColorMap(
      ["beta", "alpha"],
      DEFAULT_CHART_PALETTE,
      (_id, index) => ["legacy-a", "legacy-b"][index]!,
    );
    expect([...colors.values()]).toEqual(["legacy-a", "legacy-b"]);
    expect(isChartPalette("agentsview")).toBe(true);
    expect(isChartPalette("matplotlib")).toBe(true);
    expect(isChartPalette("neon")).toBe(false);
  });

  it("preserves a surface-specific agentsview Other token", () => {
    const colors = chartSeriesColorMap(
      ["commit", "__other__"],
      "agentsview",
      () => "legacy",
      "var(--chart-series-other)",
    );
    expect(colors.get("__other__")).toBe("var(--chart-series-other)");
  });
});
