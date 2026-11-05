import type { UsageSummaryResponse } from "../api/generated/index";
import type { SeriesStyle } from "@kenn-io/kit-ui/utils/palette";
import { orderedChartSeriesStyleMap, type ChartPalette } from "./chartPalette.js";
import { calmSeriesColor } from "./calmSeriesColor.js";

export interface UsageChartStyleMaps {
  project: ReadonlyMap<string, SeriesStyle>;
  model: ReadonlyMap<string, SeriesStyle>;
  agent: ReadonlyMap<string, SeriesStyle>;
}

function rankedIds(costs: ReadonlyMap<string, number>): string[] {
  return [...costs.entries()]
    .sort(
      ([leftId, leftCost], [rightId, rightCost]) =>
        rightCost - leftCost || leftId.localeCompare(rightId),
    )
    .map(([id]) => id);
}

export interface UsageChartColorOptions {
  /** Turns a palette color such as `var(--accent-blue)` into the current
   * theme's hex so it can be calmed. Without it only hex colors are calmed. */
  resolveColor?: (color: string) => string;
  /** False keeps the palette's exact colors, as high contrast mode needs. */
  calm?: boolean;
}

const MUTED = "var(--text-muted)";

// Calmed light teal and green land at the same lightness only about 20
// degrees of hue apart, so Usage paints teal from its own token
// (--usage-series-teal in app.css), set apart in hue and lightness. A
// resolver that does not know the token keeps the shared teal.
const USAGE_SERIES_TOKENS: Readonly<Record<string, string>> = {
  "var(--accent-teal)": "var(--usage-series-teal)",
};

function resolveUsageColor(color: string, resolveColor?: (color: string) => string): string {
  const token = USAGE_SERIES_TOKENS[color];
  if (token && resolveColor) {
    const resolved = resolveColor(token);
    if (resolved && resolved !== token) return resolved;
  }
  return resolveColor?.(color) || color;
}

function calmColor(color: string, resolveColor?: (color: string) => string): string {
  if (color === MUTED) return color;
  const resolved = resolveUsageColor(color, resolveColor);
  const calmed = calmSeriesColor(resolved);
  return calmed === resolved ? color : calmed;
}

// Usage charts fill large areas, so their series colors are calmed one step.
// The color-blind palette keeps its exact colors, which were chosen as a set
// for color-vision separation.
function usageStyleMap(
  ids: readonly string[],
  palette: ChartPalette,
  options: UsageChartColorOptions,
): ReadonlyMap<string, SeriesStyle> {
  const styles = orderedChartSeriesStyleMap(ids, palette);
  if (palette === "colorblind" || options.calm === false) return styles;
  return new Map(
    [...styles].map(([id, style]) => [
      id,
      { ...style, color: calmColor(style.color, options.resolveColor) },
    ]),
  );
}

function addCost(costs: Map<string, number>, id: string, microdollars: number) {
  costs.set(id, (costs.get(id) ?? 0) + microdollars);
}

export function usageChartStyleMaps(
  summary: UsageSummaryResponse | null,
  palette: ChartPalette,
  options: UsageChartColorOptions = {},
): UsageChartStyleMaps {
  const projects = new Map<string, number>();
  const models = new Map<string, number>();
  const agents = new Map<string, number>();

  for (const item of summary?.projectTotals ?? []) {
    projects.set(item.project_key, item.cost.microdollars);
  }
  for (const item of summary?.modelTotals ?? []) {
    models.set(item.model, item.cost.microdollars);
  }
  for (const item of summary?.agentTotals ?? []) {
    agents.set(item.agent, item.cost.microdollars);
  }

  const dailyProjects = new Map<string, number>();
  const dailyModels = new Map<string, number>();
  const dailyAgents = new Map<string, number>();
  for (const day of summary?.daily ?? []) {
    for (const item of day.projectBreakdowns ?? []) {
      addCost(dailyProjects, item.project_key, item.cost.microdollars);
    }
    for (const item of day.modelBreakdowns ?? []) {
      addCost(dailyModels, item.modelName, item.cost.microdollars);
    }
    for (const item of day.agentBreakdowns ?? []) {
      addCost(dailyAgents, item.agent, item.cost.microdollars);
    }
  }

  for (const [id, cost] of dailyProjects) projects.set(id, cost);
  for (const [id, cost] of dailyModels) models.set(id, cost);
  for (const [id, cost] of dailyAgents) agents.set(id, cost);

  return {
    project: usageStyleMap(rankedIds(projects), palette, options),
    model: usageStyleMap(rankedIds(models), palette, options),
    agent: usageStyleMap(rankedIds(agents), palette, options),
  };
}
