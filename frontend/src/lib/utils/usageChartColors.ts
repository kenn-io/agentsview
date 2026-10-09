import type { UsageSummaryResponse } from "../api/generated/index";
import { orderedChartSeriesColorMap, type ChartPalette } from "./chartPalette.js";

export type UsageColorSummary = UsageSummaryResponse & {
  colors?: Record<ChartPalette, UsageChartColorMaps>;
};

export interface UsageChartColorMaps {
  project: ReadonlyMap<string, string>;
  model: ReadonlyMap<string, string>;
  agent: ReadonlyMap<string, string>;
}

function rankedIds(costs: ReadonlyMap<string, number>): string[] {
  return [...costs.entries()]
    .sort(
      ([leftId, leftCost], [rightId, rightCost]) =>
        rightCost - leftCost || leftId.localeCompare(rightId),
    )
    .map(([id]) => id);
}

function addCost(costs: Map<string, number>, id: string, microdollars: number) {
  costs.set(id, (costs.get(id) ?? 0) + microdollars);
}

export function usageChartColorMaps(
  summary: UsageColorSummary | null,
  palette: ChartPalette,
): UsageChartColorMaps {
  if (summary?.colors) return summary.colors[palette];
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
    project: orderedChartSeriesColorMap(rankedIds(projects), palette),
    model: orderedChartSeriesColorMap(rankedIds(models), palette),
    agent: orderedChartSeriesColorMap(rankedIds(agents), palette),
  };
}

export function mergeUsageColorSummary(
  previous: UsageColorSummary | null,
  summaries: UsageSummaryResponse[],
): UsageColorSummary {
  const colors = {} as Record<ChartPalette, UsageChartColorMaps>;
  for (const palette of ["agentsview", "matplotlib"] as const) {
    const maps = { ...usageChartColorMaps(previous, palette) };
    for (const summary of summaries) {
      const incoming = usageChartColorMaps(summary, palette);
      for (const by of ["project", "model", "agent"] as const) {
        const ids = [...new Set([...maps[by].keys(), ...incoming[by].keys()])];
        const assigned = orderedChartSeriesColorMap(ids, palette);
        maps[by] = new Map(ids.map((id) => [id, maps[by].get(id) ?? assigned.get(id)!]));
      }
    }
    colors[palette] = maps;
  }
  return { ...summaries[0]!, colors };
}
