import { describe, expect, it } from "vite-plus/test";
import type { UsageSummaryResponse } from "../api/generated/index";
import { testMoney } from "../test/money.js";
import { usageChartStyleMaps } from "./usageChartStyles.js";

function tenModelSummary(
  models = [
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
  ],
): UsageSummaryResponse {
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
    projects: {},
    totals: {
      inputTokens: 100,
      outputTokens: 50,
      cacheCreationTokens: 0,
      cacheReadTokens: 0,
      totalCost: testMoney(55),
      cacheSavings: testMoney(0),
    },
    daily: [
      {
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
        projectBreakdowns: [],
        agentBreakdowns: [],
        machineBreakdowns: [],
      },
    ],
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

describe("usageChartStyleMaps", () => {
  it("assigns Matplotlib colors by aggregate cost descending", () => {
    const styles = usageChartStyleMaps(tenModelSummary(), "matplotlib").model;

    expect(styles.size).toBe(10);
    expect(styles.get("model-zulu")?.color).toBe("#1f77b4");
    expect(styles.get("model-india")?.color).toBe("#aec7e8");
    expect(styles.get("model-alpha")?.color).toBe("#c5b0d5");
  });

  it("keeps Matplotlib colors distinct when a filter exposes more models", () => {
    const models = Array.from({ length: 11 }, (_, index) => `model-${index}`);
    const styles = usageChartStyleMaps(tenModelSummary(models), "matplotlib").model;

    expect(new Set([...styles.values()].map((style) => style.color)).size).toBe(11);
  });

  it("hatches color-blind safe series past the tenth color instead of repeating it", () => {
    const models = Array.from({ length: 11 }, (_, index) => `model-${index}`);
    const styles = [...usageChartStyleMaps(tenModelSummary(models), "colorblind").model.values()];

    expect(styles.slice(0, 10).every((style) => style.fill === "solid")).toBe(true);
    expect(styles[10]).toMatchObject({ color: styles[0]!.color, fill: "hatch" });
  });
});
