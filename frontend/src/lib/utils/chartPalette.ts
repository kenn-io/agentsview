import { createPalette } from "@kenn-io/kit-ui/utils/palette";
import { PROJECT_PALETTE, seriesColorMap } from "./projectColor.js";

export type ChartPalette = "agentsview" | "matplotlib" | "colorblind";

export const DEFAULT_CHART_PALETTE: ChartPalette = "agentsview";

const MUTED = "var(--text-muted)";

// "Other" and unnamed series are muted gray, so matplotlib's grays are left
// out to keep real series from looking like them.
const MATPLOTLIB_GRAYS = ["#7f7f7f", "#c7c7c7", "#636363", "#969696", "#bdbdbd", "#d9d9d9"];

export function isChartPalette(value: unknown): value is ChartPalette {
  return value === "agentsview" || value === "matplotlib" || value === "colorblind";
}

/**
 * Colors for chart series under a chart-color setting. Under the
 * agentsview setting a chart may pass its own colors and "Other" color;
 * without them, series keep their project identity colors. Matplotlib and
 * color-blind safe colors go in sorted order, so a series keeps its color
 * across charts that show the same set.
 */
export function chartSeriesColorMap(
  ids: readonly string[],
  palette: ChartPalette,
  agentsviewColors?: readonly string[],
  agentsviewOtherColor?: string,
): ReadonlyMap<string, string> {
  const fixed = { "": MUTED, __other__: MUTED };
  if (palette === "matplotlib") {
    return createPalette("matplotlib", { omit: MATPLOTLIB_GRAYS, fixed }).assign(ids, {
      order: "sorted",
    });
  }
  if (palette === "colorblind") {
    return createPalette("petroff", { fixed }).assign(ids, { order: "sorted" });
  }
  if (agentsviewColors === undefined) return seriesColorMap(ids);
  return createPalette(agentsviewColors, {
    fixed: { ...fixed, __other__: agentsviewOtherColor ?? MUTED },
  }).assign(ids);
}

/** Colors for series already ranked by the caller, such as cost order. */
export function orderedChartSeriesColorMap(
  ids: readonly string[],
  palette: ChartPalette,
): ReadonlyMap<string, string> {
  const fixed = { "": MUTED, __other__: MUTED };
  const source =
    palette === "agentsview"
      ? PROJECT_PALETTE
      : palette === "colorblind"
        ? "petroff"
        : "matplotlib";
  return createPalette(source, {
    omit: palette === "matplotlib" ? MATPLOTLIB_GRAYS : undefined,
    fixed,
  }).assign(ids);
}
