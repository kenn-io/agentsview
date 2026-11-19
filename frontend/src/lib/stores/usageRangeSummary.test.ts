import { describe, expect, it } from "vite-plus/test";
import type { DbDailyUsageEntry, UsageSummaryResponse } from "../api/generated/index";
import { summaryForDateRange } from "./usageRangeSummary.js";

const tokens = (input: number, output: number, cacheCreation = 0, cacheRead = 0) => ({
  inputTokens: input,
  outputTokens: output,
  cacheCreationTokens: cacheCreation,
  cacheReadTokens: cacheRead,
});

function day(entry: Partial<DbDailyUsageEntry> & { date: string }): DbDailyUsageEntry {
  return {
    ...tokens(0, 0),
    totalCost: { microdollars: 0 },
    agentBreakdowns: [],
    machineBreakdowns: [],
    modelBreakdowns: [],
    modelsUsed: [],
    projectBreakdowns: [],
    ...entry,
  };
}

function window(): UsageSummaryResponse {
  return {
    from: "2024-01-01",
    to: "2024-01-03",
    projects: {},
    sessionCounts: { total: 4, byAgent: {}, byProject: {} },
    totals: {
      ...tokens(9_999, 999),
      totalCost: { microdollars: 9_000_000 },
      cacheSavings: { microdollars: 123 },
    },
    daily: [
      day({
        date: "2024-01-01",
        ...tokens(9_999, 999),
        totalCost: { microdollars: 9_000_000 },
        projectBreakdowns: [
          {
            project_key: "pg",
            project: "gamma",
            ...tokens(9_999, 999),
            cost: { microdollars: 9_000_000 },
          },
        ],
      }),
      day({
        date: "2024-01-02",
        ...tokens(100, 10, 5, 300),
        totalCost: { microdollars: 2_000_000 },
        projectBreakdowns: [
          {
            project_key: "pb",
            project: "beta",
            ...tokens(60, 6, 3, 200),
            cost: { microdollars: 1_000_000 },
          },
          {
            project_key: "pa",
            project: "alpha",
            ...tokens(40, 4, 2, 100),
            cost: { microdollars: 1_000_000 },
          },
        ],
        modelBreakdowns: [
          { modelName: "m1", ...tokens(100, 10, 5, 300), cost: { microdollars: 2_000_000 } },
        ],
        agentBreakdowns: [
          { agent: "claude", ...tokens(100, 10, 5, 300), cost: { microdollars: 2_000_000 } },
        ],
      }),
      day({
        date: "2024-01-03",
        ...tokens(50, 5),
        projectBreakdowns: [
          { project_key: "pa", project: "alpha", ...tokens(50, 5), cost: { microdollars: 0 } },
        ],
        modelBreakdowns: [{ modelName: "m2", ...tokens(50, 5), cost: { microdollars: 0 } }],
        agentBreakdowns: [{ agent: "codex", ...tokens(50, 5), cost: { microdollars: 0 } }],
      }),
    ],
    projectTotals: [],
    modelTotals: [
      { model: "m1", ...tokens(0, 0), cost: { microdollars: 0 } },
      { model: "m3", ...tokens(0, 0), cost: { microdollars: 0 }, planOnlySessions: 2 },
    ],
    agentTotals: [],
    cacheStats: {
      ...tokens(0, 0),
      uncachedInputTokens: 0,
      hitRate: 0.5,
      savingsVsUncached: { microdollars: 77 },
    },
    comparison: {} as UsageSummaryResponse["comparison"],
    unsupportedUsage: {} as UsageSummaryResponse["unsupportedUsage"],
  };
}

describe("summaryForDateRange", () => {
  it("totals only the days inside the range and re-ranks each breakdown by cost", () => {
    const derived = summaryForDateRange(window(), "2024-01-02", "2024-01-03");

    expect(derived.from).toBe("2024-01-02");
    expect(derived.to).toBe("2024-01-03");
    expect(derived.daily.map((entry) => entry.date)).toEqual(["2024-01-02", "2024-01-03"]);
    expect(derived.totals).toEqual({
      ...tokens(150, 15, 5, 300),
      totalCost: { microdollars: 2_000_000 },
      cacheSavings: { microdollars: 0 },
    });
    // alpha and beta tie on cost, so the key breaks the tie.
    expect(derived.projectTotals).toEqual([
      {
        project_key: "pa",
        project: "alpha",
        ...tokens(90, 9, 2, 100),
        cost: { microdollars: 1_000_000 },
      },
      {
        project_key: "pb",
        project: "beta",
        ...tokens(60, 6, 3, 200),
        cost: { microdollars: 1_000_000 },
      },
    ]);
    expect(derived.agentTotals).toEqual([
      { agent: "claude", ...tokens(100, 10, 5, 300), cost: { microdollars: 2_000_000 } },
      { agent: "codex", ...tokens(50, 5), cost: { microdollars: 0 } },
    ]);
    expect(derived.cacheStats).toEqual({
      cacheReadTokens: 300,
      cacheCreationTokens: 5,
      uncachedInputTokens: 150,
      outputTokens: 15,
      hitRate: 2 / 3,
      savingsVsUncached: { microdollars: 0 },
    });
    expect(derived.comparison).toBeUndefined();
    expect(derived.unsupportedUsage).toBeUndefined();
    expect(derived.sessionCounts).toEqual({ total: 4, byAgent: {}, byProject: {} });
  });

  it("keeps plan-only model counts from the window, which daily entries cannot place", () => {
    const derived = summaryForDateRange(window(), "2024-01-02", "2024-01-03");

    expect(derived.modelTotals).toEqual([
      { model: "m1", ...tokens(100, 10, 5, 300), cost: { microdollars: 2_000_000 } },
      { model: "m2", ...tokens(50, 5), cost: { microdollars: 0 } },
      { model: "m3", ...tokens(0, 0), cost: { microdollars: 0 }, planOnlySessions: 2 },
    ]);
  });

  it("reports a zero hit rate for a range with no input or cache reads", () => {
    const derived = summaryForDateRange(window(), "2024-02-01", "2024-02-02");

    expect(derived.daily).toEqual([]);
    expect(derived.totals.totalCost).toEqual({ microdollars: 0 });
    expect(derived.cacheStats.hitRate).toBe(0);
    expect(derived.projectTotals).toEqual([]);
  });
});
