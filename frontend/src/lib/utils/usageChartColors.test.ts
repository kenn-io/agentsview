import { describe, expect, it } from "vite-plus/test";
import type { UsageSummaryResponse } from "../api/types/usage.js";
import { testMoney } from "../test/money.js";
import { usageChartColorMaps } from "./usageChartColors.js";

function tenModelSummary(): UsageSummaryResponse {
  const models = [
    "model-alpha",
    "model-bravo",
    "model-charlie",
    "model-delta",
    "model-echo",
    "model-foxtrot",
    "model-golf",
    "model-hotel",
    "model-india",
    "model-zulu",
  ];
  const modelTotals = models.map((model, index) => ({
    model,
    inputTokens: 10,
    outputTokens: 5,
    cacheCreationTokens: 0,
    cacheReadTokens: 0,
    cost: testMoney(index + 1),
  }));
  return {
    from: "2026-07-01",
    to: "2026-07-01",
    totals: {
      inputTokens: 100,
      outputTokens: 50,
      cacheCreationTokens: 0,
      cacheReadTokens: 0,
      totalCost: testMoney(55),
    },
    daily: [{
      date: "2026-07-01",
      inputTokens: 100,
      outputTokens: 50,
      cacheCreationTokens: 0,
      cacheReadTokens: 0,
      totalCost: testMoney(55),
      modelsUsed: models,
      modelBreakdowns: models.map((modelName, index) => ({
        modelName,
        inputTokens: 10,
        outputTokens: 5,
        cacheCreationTokens: 0,
        cacheReadTokens: 0,
        cost: testMoney(index + 1),
      })),
    }],
    projectTotals: [],
    modelTotals,
    agentTotals: [],
    sessionCounts: { total: 10, byProject: {}, byAgent: {} },
    cacheStats: {
      cacheReadTokens: 0,
      cacheCreationTokens: 0,
      uncachedInputTokens: 100,
      outputTokens: 50,
      hitRate: 0,
      savingsVsUncached: testMoney(0),
    },
  };
}

describe("usageChartColorMaps", () => {
  it("assigns Matplotlib colors by aggregate cost descending", () => {
    const colors = usageChartColorMaps(tenModelSummary(), "matplotlib").model;

    expect(colors.size).toBe(10);
    expect(colors.get("model-zulu")).toBe("#1f77b4");
    expect(colors.get("model-india")).toBe("#aec7e8");
    expect(colors.get("model-alpha")).toBe("#c5b0d5");
  });
});
