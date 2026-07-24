import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { mount, tick, unmount } from "svelte";
import UsagePairwiseComparisonPanel from "./UsagePairwiseComparisonPanel.svelte";
import { usage } from "../../stores/usage.svelte.js";
import type {
  UsagePairwiseComparisonResponse,
  UsageSummaryResponse,
} from "../../api/types/usage.js";

function usageSummary(): UsageSummaryResponse {
  return {
    from: "2024-01-01",
    to: "2024-01-31",
    totals: {
      inputTokens: 400,
      outputTokens: 200,
      cacheCreationTokens: 0,
      cacheReadTokens: 0,
      totalCost: 99.99,
    },
    daily: [],
    projectTotals: [
      {
        project_key: "pl1:sha256:alpha",
        project: "alpha",
        inputTokens: 200,
        outputTokens: 100,
        cacheCreationTokens: 0,
        cacheReadTokens: 0,
        cost: 10,
      },
      {
        project_key: "pl1:sha256:beta",
        project: "beta",
        inputTokens: 100,
        outputTokens: 50,
        cacheCreationTokens: 0,
        cacheReadTokens: 0,
        cost: 8,
      },
    ],
    modelTotals: [
      {
        model: "claude-sonnet-4-20250514",
        inputTokens: 200,
        outputTokens: 100,
        cacheCreationTokens: 0,
        cacheReadTokens: 0,
        cost: 10,
      },
      {
        model: "gpt-4o",
        inputTokens: 100,
        outputTokens: 50,
        cacheCreationTokens: 0,
        cacheReadTokens: 0,
        cost: 8,
      },
    ],
    agentTotals: [],
    branchTotals: [],
    sessionCounts: {
      total: 2,
      byProject: { alpha: 1, beta: 1 },
      byAgent: {},
    },
    cacheStats: {
      cacheReadTokens: 0,
      cacheCreationTokens: 0,
      uncachedInputTokens: 400,
      outputTokens: 200,
      hitRate: 0,
      savingsVsUncached: 0,
    },
  };
}

function pairwiseComparison(): UsagePairwiseComparisonResponse {
  return {
    left: {
      totalCost: 4,
      inputTokens: 200,
      outputTokens: 100,
      cacheCreationTokens: 0,
      cacheReadTokens: 0,
      totalTokens: 300,
      sessionCount: 2,
      costPerSession: 2,
      tokensPerSession: 150,
    },
    right: {
      totalCost: 5.5,
      inputTokens: 100,
      outputTokens: 50,
      cacheCreationTokens: 0,
      cacheReadTokens: 0,
      totalTokens: 150,
      sessionCount: 1,
      costPerSession: 5.5,
      tokensPerSession: 150,
    },
    deltas: {
      totalCostDelta: 1.5,
      totalCostDeltaRatio: 0.375,
      inputTokensDelta: -100,
      inputTokensDeltaRatio: -0.5,
      outputTokensDelta: -50,
      outputTokensDeltaRatio: -0.5,
      cacheCreationDelta: 0,
      cacheCreationDeltaRatio: null,
      cacheReadDelta: 0,
      cacheReadDeltaRatio: null,
      totalTokensDelta: -150,
      totalTokensDeltaRatio: -0.5,
      sessionCountDelta: -1,
      sessionCountDeltaRatio: -0.5,
      costPerSessionDelta: 3.5,
      costPerSessionRatio: 1.75,
      tokensPerSessionDelta: 0,
      tokensPerSessionRatio: 0,
    },
  };
}

describe("UsagePairwiseComparisonPanel", () => {
  beforeEach(() => {
    usage.summary = usageSummary();
    usage.pairwiseSelection = {
      left: { dimension: "model", value: "claude-sonnet-4-20250514" },
      right: { dimension: "project", value: "pl1:sha256:beta" },
    };
    usage.pairwiseComparison = pairwiseComparison();
    usage.loading.pairwise = false;
    usage.errors.pairwise = null;
  });

  afterEach(() => {
    usage.summary = null;
    usage.pairwiseComparison = null;
    usage.pairwiseSelection = {
      left: { dimension: "model", value: "" },
      right: { dimension: "model", value: "" },
    };
    usage.loading.pairwise = false;
    usage.errors.pairwise = null;
    document.body.innerHTML = "";
    vi.restoreAllMocks();
  });

  it("routes selector changes through the usage store", async () => {
    const spy = vi.spyOn(usage, "setPairwiseSide");
    const component = mount(UsagePairwiseComparisonPanel, {
      target: document.body,
    });
    await tick();

    const trigger = document.querySelector<HTMLButtonElement>(
      'button[aria-label="Left comparison dimension"]',
    );
    expect(trigger).toBeTruthy();
    if (!trigger) return;
    trigger.click();
    await tick();

    const option = Array.from(
      document.querySelectorAll<HTMLLIElement>('li[role="option"]'),
    ).find((item) => item.textContent?.includes("Project"));
    expect(option).toBeTruthy();
    option?.dispatchEvent(new MouseEvent("mousedown", { bubbles: true }));
    await tick();

    expect(spy).toHaveBeenCalledWith("left", { dimension: "project" });
    unmount(component);
  });

  it("renders backend-provided delta fields instead of deriving them from summary totals", async () => {
    const component = mount(UsagePairwiseComparisonPanel, {
      target: document.body,
    });
    await tick();

    const text = document.body.textContent ?? "";
    expect(text).toContain("+$1.50");
    expect(text).toContain("+37.5%");
    expect(text).not.toContain("+$94.49");

    unmount(component);
  });

  it("renders null backend ratios as the empty-state marker", async () => {
    usage.pairwiseComparison = {
      ...pairwiseComparison(),
      deltas: {
        ...pairwiseComparison().deltas,
        costPerSessionDelta: null,
        costPerSessionRatio: null,
        tokensPerSessionDelta: null,
        tokensPerSessionRatio: null,
      },
    };
    const component = mount(UsagePairwiseComparisonPanel, {
      target: document.body,
    });
    await tick();

    const text = document.body.textContent ?? "";
    expect(text).toContain("None");
    expect(text).not.toContain("+$0.00");
    expect(text).not.toContain("+0.0%");

    unmount(component);
  });
});
