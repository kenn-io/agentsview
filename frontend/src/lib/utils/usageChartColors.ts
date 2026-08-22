import type { UsageSummaryResponse } from "../api/types/usage.js";
import {
  orderedChartSeriesColorMap,
  type ChartPalette,
} from "./chartPalette.js";

export interface UsageChartColorMaps {
  project: ReadonlyMap<string, string>;
  model: ReadonlyMap<string, string>;
  agent: ReadonlyMap<string, string>;
}

function rankedIds(costs: ReadonlyMap<string, number>): string[] {
  return [...costs.entries()]
    .sort(([leftId, leftCost], [rightId, rightCost]) =>
      rightCost - leftCost || leftId.localeCompare(rightId)
    )
    .map(([id]) => id);
}

function addCost(costs: Map<string, number>, id: string, microdollars: number) {
  costs.set(id, (costs.get(id) ?? 0) + microdollars);
}

export function usageChartColorMaps(
  summary: UsageSummaryResponse | null,
  palette: ChartPalette,
): UsageChartColorMaps {
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
