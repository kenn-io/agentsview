import {
  createPalette,
  type Palette,
  type PaletteOrder,
  type SeriesStyle,
} from "@kenn-io/kit-ui/utils/palette";
import { PROJECT_PALETTE } from "./projectColor.js";

export type ChartPalette = "agentsview" | "matplotlib" | "colorblind";

export const DEFAULT_CHART_PALETTE: ChartPalette = "agentsview";

const MUTED = "var(--text-muted)";

// "Other" and unnamed series are muted gray, so matplotlib's grays are left
// out to keep real series from looking like them.
const MATPLOTLIB_GRAYS = ["#7f7f7f", "#c7c7c7", "#636363", "#969696", "#bdbdbd", "#d9d9d9"];

export function isChartPalette(value: unknown): value is ChartPalette {
  return value === "agentsview" || value === "matplotlib" || value === "colorblind";
}

function colorsOf(styles: ReadonlyMap<string, SeriesStyle>): ReadonlyMap<string, string> {
  return new Map([...styles].map(([key, style]) => [key, style.color]));
}

// The color-blind palette keeps series apart past its last color with
// patterns, dashes, and markers; the others repeat colors.
function colorblindPalette(fixed: Record<string, string>): Palette {
  return createPalette("petroff", { fixed, overflow: "pattern" });
}

/**
 * Styles for chart series under a chart-color setting. Under the
 * agentsview setting a chart may pass its own colors and "Other" color;
 * without them, series keep their project identity colors. Matplotlib and
 * color-blind safe colors go in sorted order, so a series keeps its style
 * across charts that show the same set.
 */
export function chartSeriesStyleMap(
  ids: readonly string[],
  palette: ChartPalette,
  agentsviewColors?: readonly string[],
  agentsviewOtherColor?: string,
): ReadonlyMap<string, SeriesStyle> {
  const fixed = { "": MUTED, __other__: MUTED };
  let source: Palette;
  let order: PaletteOrder;
  if (palette === "matplotlib") {
    source = createPalette("matplotlib", { omit: MATPLOTLIB_GRAYS, fixed });
    order = "sorted";
  } else if (palette === "colorblind") {
    source = colorblindPalette(fixed);
    order = "sorted";
  } else if (agentsviewColors === undefined) {
    source = createPalette(PROJECT_PALETTE, { fixed });
    order = "hash";
  } else {
    source = createPalette(agentsviewColors, {
      fixed: { ...fixed, __other__: agentsviewOtherColor ?? MUTED },
    });
    order = "given";
  }
  return source.assignStyles(ids, { order });
}

/** Colors for chart series; see chartSeriesStyleMap. */
export function chartSeriesColorMap(
  ids: readonly string[],
  palette: ChartPalette,
  agentsviewColors?: readonly string[],
  agentsviewOtherColor?: string,
): ReadonlyMap<string, string> {
  return colorsOf(chartSeriesStyleMap(ids, palette, agentsviewColors, agentsviewOtherColor));
}

/** Styles for series already ranked by the caller, such as cost order. */
export function orderedChartSeriesStyleMap(
  ids: readonly string[],
  palette: ChartPalette,
): ReadonlyMap<string, SeriesStyle> {
  const fixed = { "": MUTED, __other__: MUTED };
  const source =
    palette === "colorblind"
      ? colorblindPalette(fixed)
      : palette === "matplotlib"
        ? createPalette("matplotlib", { omit: MATPLOTLIB_GRAYS, fixed })
        : createPalette(PROJECT_PALETTE, { fixed });
  return source.assignStyles(ids);
}

/** Colors for series already ranked by the caller; see orderedChartSeriesStyleMap. */
export function orderedChartSeriesColorMap(
  ids: readonly string[],
  palette: ChartPalette,
): ReadonlyMap<string, string> {
  return colorsOf(orderedChartSeriesStyleMap(ids, palette));
}
