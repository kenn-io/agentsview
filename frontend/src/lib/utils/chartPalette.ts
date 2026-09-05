import { PROJECT_PALETTE, seriesColorMap } from "./projectColor.js";

export type ChartPalette = "agentsview" | "matplotlib";

export const DEFAULT_CHART_PALETTE: ChartPalette = "agentsview";

const MUTED = "var(--text-muted)";

const TAB10 = [
  "#1f77b4",
  "#ff7f0e",
  "#2ca02c",
  "#d62728",
  "#9467bd",
  "#8c564b",
  "#e377c2",
  "#bcbd22",
  "#17becf",
] as const;

const TAB20 = [
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

const TAB20B_AND_TAB20C = [
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

function matplotlibFamily(count: number): readonly string[] {
  return count <= 9 ? TAB10 : count <= 18 ? TAB20 : TAB20B_AND_TAB20C;
}

export function isChartPalette(value: unknown): value is ChartPalette {
  return value === "agentsview" || value === "matplotlib";
}

export function chartSeriesColorMap(
  ids: readonly string[],
  palette: ChartPalette,
  agentsviewColor?: (id: string, index: number) => string,
  agentsviewOtherColor?: string,
): ReadonlyMap<string, string> {
  if (palette === "agentsview" && agentsviewColor === undefined) {
    return seriesColorMap(ids);
  }

  const activeIds = [...new Set(ids)].filter((id) => id !== "" && id !== "__other__");
  const colors = new Map<string, string>();

  if (palette === "agentsview") {
    for (const [index, id] of activeIds.entries()) {
      colors.set(id, agentsviewColor!(id, index));
    }
  } else {
    activeIds.sort();
    const family = matplotlibFamily(activeIds.length);
    for (const [index, id] of activeIds.entries()) {
      colors.set(id, family[index % family.length]!);
    }
  }

  if (ids.includes("")) colors.set("", MUTED);
  if (ids.includes("__other__")) {
    colors.set("__other__", palette === "agentsview" ? (agentsviewOtherColor ?? MUTED) : MUTED);
  }
  return colors;
}

export function orderedChartSeriesColorMap(
  ids: readonly string[],
  palette: ChartPalette,
): ReadonlyMap<string, string> {
  const activeIds = [...new Set(ids)].filter((id) => id !== "" && id !== "__other__");
  const family = palette === "agentsview" ? PROJECT_PALETTE : matplotlibFamily(activeIds.length);
  const colors = new Map<string, string>();

  for (const [index, id] of activeIds.entries()) {
    colors.set(id, family[index % family.length]!);
  }
  if (ids.includes("")) colors.set("", MUTED);
  if (ids.includes("__other__")) colors.set("__other__", MUTED);
  return colors;
}
