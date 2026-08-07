import type { UsageSummaryResponse } from "../api/types/usage.js";
import { branchFilterToken } from "../branchFilters.js";
import {
  chartSeriesColorMap,
  type ChartPalette,
} from "./chartPalette.js";

export interface UsageChartColorMaps {
  project: ReadonlyMap<string, string>;
  model: ReadonlyMap<string, string>;
  agent: ReadonlyMap<string, string>;
  branch: ReadonlyMap<string, string>;
}

export function usageChartColorMaps(
  summary: UsageSummaryResponse | null,
  palette: ChartPalette,
): UsageChartColorMaps {
  const projects = new Set<string>();
  const models = new Set<string>();
  const agents = new Set<string>();
  const branches = new Set<string>();

  for (const item of summary?.projectTotals ?? []) {
    projects.add(item.project_key);
  }
  for (const item of summary?.modelTotals ?? []) {
    models.add(item.model);
  }
  for (const item of summary?.agentTotals ?? []) {
    agents.add(item.agent);
  }
  for (const item of summary?.branchTotals ?? []) {
    branches.add(branchFilterToken(
      item.project_key || item.project,
      item.branch,
    ));
  }
  for (const day of summary?.daily ?? []) {
    for (const item of day.projectBreakdowns ?? []) {
      projects.add(item.project_key);
    }
    for (const item of day.modelBreakdowns ?? []) {
      models.add(item.modelName);
    }
    for (const item of day.agentBreakdowns ?? []) {
      agents.add(item.agent);
    }
    for (const item of day.branchBreakdowns ?? []) {
      branches.add(branchFilterToken(
        item.project_key || item.project,
        item.branch,
      ));
    }
  }

  return {
    project: chartSeriesColorMap([...projects].sort(), palette),
    model: chartSeriesColorMap([...models].sort(), palette),
    agent: chartSeriesColorMap([...agents].sort(), palette),
    branch: chartSeriesColorMap([...branches].sort(), palette),
  };
}
