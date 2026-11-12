import type { UsageSummaryResponse } from "../api/generated/index";

// Derives the summary for a sub-range of the window a loaded summary covers,
// from its daily entries, so a chart selection shows totals before the
// range's own summary arrives.
export function summaryForDateRange(
  summary: UsageSummaryResponse,
  from: string,
  to: string,
): UsageSummaryResponse {
  const daily = summary.daily.filter((day) => day.date >= from && day.date <= to);
  const projectTotals = new Map<string, UsageSummaryResponse["projectTotals"][number]>();
  const modelTotals = new Map<string, UsageSummaryResponse["modelTotals"][number]>();
  const agentTotals = new Map<string, UsageSummaryResponse["agentTotals"][number]>();
  let inputTokens = 0;
  let outputTokens = 0;
  let cacheCreationTokens = 0;
  let cacheReadTokens = 0;
  let totalMicrodollars = 0;

  for (const day of daily) {
    inputTokens += day.inputTokens;
    outputTokens += day.outputTokens;
    cacheCreationTokens += day.cacheCreationTokens;
    cacheReadTokens += day.cacheReadTokens;
    totalMicrodollars += day.totalCost.microdollars;

    for (const item of day.projectBreakdowns ?? []) {
      const total = projectTotals.get(item.project_key) ?? {
        project_key: item.project_key,
        project: item.project,
        inputTokens: 0,
        outputTokens: 0,
        cacheCreationTokens: 0,
        cacheReadTokens: 0,
        cost: { microdollars: 0 },
      };
      total.inputTokens += item.inputTokens;
      total.outputTokens += item.outputTokens;
      total.cacheCreationTokens += item.cacheCreationTokens;
      total.cacheReadTokens += item.cacheReadTokens;
      total.cost.microdollars += item.cost.microdollars;
      projectTotals.set(item.project_key, total);
    }

    for (const item of day.modelBreakdowns ?? []) {
      const total = modelTotals.get(item.modelName) ?? {
        model: item.modelName,
        inputTokens: 0,
        outputTokens: 0,
        cacheCreationTokens: 0,
        cacheReadTokens: 0,
        cost: { microdollars: 0 },
      };
      total.inputTokens += item.inputTokens;
      total.outputTokens += item.outputTokens;
      total.cacheCreationTokens += item.cacheCreationTokens;
      total.cacheReadTokens += item.cacheReadTokens;
      total.cost.microdollars += item.cost.microdollars;
      modelTotals.set(item.modelName, total);
    }

    for (const item of day.agentBreakdowns ?? []) {
      const total = agentTotals.get(item.agent) ?? {
        agent: item.agent,
        inputTokens: 0,
        outputTokens: 0,
        cacheCreationTokens: 0,
        cacheReadTokens: 0,
        cost: { microdollars: 0 },
      };
      total.inputTokens += item.inputTokens;
      total.outputTokens += item.outputTokens;
      total.cacheCreationTokens += item.cacheCreationTokens;
      total.cacheReadTokens += item.cacheReadTokens;
      total.cost.microdollars += item.cost.microdollars;
      agentTotals.set(item.agent, total);
    }
  }

  // Plan-only sessions have no usage events, so daily entries cannot place
  // them in the range; keep each model's count from the source summary so
  // its row still reads "Plan usage" instead of zero until the range's own
  // summary arrives.
  for (const source of summary.modelTotals) {
    if (!source.planOnlySessions) continue;
    const total = modelTotals.get(source.model) ?? {
      model: source.model,
      inputTokens: 0,
      outputTokens: 0,
      cacheCreationTokens: 0,
      cacheReadTokens: 0,
      cost: { microdollars: 0 },
    };
    total.planOnlySessions = source.planOnlySessions;
    modelTotals.set(source.model, total);
  }

  const byCost = <T extends { cost: { microdollars: number } }>(a: T, b: T) =>
    b.cost.microdollars - a.cost.microdollars;
  const cacheHitDenominator = cacheReadTokens + inputTokens;

  return {
    ...summary,
    from,
    to,
    daily,
    totals: {
      inputTokens,
      outputTokens,
      cacheCreationTokens,
      cacheReadTokens,
      totalCost: { microdollars: totalMicrodollars },
      // Daily entries carry no per-day savings, so a derived range cannot
      // recompute them; the UI does not read this field for derived ranges.
      cacheSavings: { microdollars: 0 },
    },
    projectTotals: [...projectTotals.values()].sort(
      (a, b) => byCost(a, b) || a.project_key.localeCompare(b.project_key),
    ),
    modelTotals: [...modelTotals.values()].sort(
      (a, b) => byCost(a, b) || a.model.localeCompare(b.model),
    ),
    agentTotals: [...agentTotals.values()].sort(
      (a, b) => byCost(a, b) || a.agent.localeCompare(b.agent),
    ),
    cacheStats: {
      cacheReadTokens,
      cacheCreationTokens,
      uncachedInputTokens: inputTokens,
      outputTokens,
      hitRate: cacheHitDenominator > 0 ? cacheReadTokens / cacheHitDenominator : 0,
      savingsVsUncached: { microdollars: 0 },
    },
    unsupportedUsage: undefined,
    comparison: undefined,
  };
}
