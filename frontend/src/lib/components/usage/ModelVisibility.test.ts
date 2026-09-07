import { afterEach, beforeEach, describe, expect, it, vi } from "vite-plus/test";
import { mount, tick, unmount } from "svelte";
import type { UsageSummaryResponse } from "../../api/generated/index";
import { testMoney } from "../../test/money.js";

const api = vi.hoisted(() => {
  const zero = { microdollars: 0 };
  const metrics = {
    totalCost: zero,
    inputTokens: 0,
    outputTokens: 0,
    cacheCreationTokens: 0,
    cacheReadTokens: 0,
    totalTokens: 0,
    sessionCount: 0,
    costPerSession: null,
    tokensPerSession: null,
  };
  return {
    getApiV1UsageSummary: vi.fn(),
    getApiV1UsageComparison: vi.fn().mockResolvedValue({
      priorFrom: "2023-12-01",
      priorTo: "2023-12-31",
      priorTotalCost: zero,
      deltaPct: null,
    }),
    getApiV1UsagePairwiseComparison: vi.fn().mockResolvedValue({
      left: metrics,
      right: metrics,
      deltas: {
        totalCostDelta: zero,
        totalCostDeltaRatio: null,
        inputTokensDelta: 0,
        inputTokensDeltaRatio: null,
        outputTokensDelta: 0,
        outputTokensDeltaRatio: null,
        cacheCreationDelta: 0,
        cacheCreationDeltaRatio: null,
        cacheReadDelta: 0,
        cacheReadDeltaRatio: null,
        totalTokensDelta: 0,
        totalTokensDeltaRatio: null,
        sessionCountDelta: 0,
        sessionCountDeltaRatio: null,
        costPerSessionDelta: null,
        costPerSessionRatio: null,
        tokensPerSessionDelta: null,
        tokensPerSessionRatio: null,
      },
    }),
    getApiV1UsageTopSessions: vi.fn().mockResolvedValue([]),
  };
});
vi.mock("../../api/generated/index", () => ({ UsageService: api }));
vi.mock("../../api/runtime.js", () => ({
  callGenerated: (request: () => Promise<unknown>) => request(),
  isAbortError: () => false,
}));

import UsagePage from "./UsagePage.svelte";
import { usage } from "../../stores/usage.svelte.js";
import { sessions } from "../../stores/sessions.svelte.js";
import { router } from "../../stores/router.svelte.js";

function summary(excluded = ""): UsageSummaryResponse {
  const hidden = excluded.split(",");
  return {
    from: "2024-01-01",
    to: "2024-01-31",
    projects: {},
    totals: {
      inputTokens: 30,
      outputTokens: 15,
      cacheCreationTokens: 0,
      cacheReadTokens: 0,
      totalCost: testMoney(6),
      cacheSavings: testMoney(0),
    },
    daily: [],
    projectTotals: [],
    agentTotals: [],
    modelTotals: ["model-alpha", "model-bravo", "model-charlie"]
      .filter((model) => !hidden.includes(model))
      .map((model, index) => ({
        model,
        inputTokens: 10,
        outputTokens: 5,
        cacheCreationTokens: 0,
        cacheReadTokens: 0,
        cost: testMoney(3 - index),
      })),
    sessionCounts: { total: 3, byProject: {}, byAgent: {} },
    cacheStats: {
      cacheReadTokens: 0,
      cacheCreationTokens: 0,
      uncachedInputTokens: 30,
      outputTokens: 15,
      hitRate: 0,
      savingsVsUncached: testMoney(0),
    },
  };
}

function modelPicker() {
  return document.querySelector<HTMLButtonElement>(
    '.kit-filter-dropdown__btn[aria-label^="Model:"]',
  )!;
}

function modelOption(name: string) {
  return Array.from(
    document.querySelectorAll<HTMLButtonElement>(".kit-filter-dropdown__item"),
  ).find((button) => button.textContent?.trim() === name)!;
}

let component: ReturnType<typeof mount>;
beforeEach(() => {
  vi.clearAllMocks();
  vi.spyOn(sessions, "loadAgents").mockResolvedValue();
  api.getApiV1UsageSummary.mockImplementation(async (params) => summary(params.exclude_model));
  usage.excludedModels = "";
  usage.toggles.attribution.groupBy = "model";
  usage.toggles.attribution.view = "treemap";
  usage.summary = summary();
  router.route = "usage";
  router.params = {};
});
afterEach(async () => {
  await unmount(component);
  usage.cancelInFlightReads();
  usage.summary = null;
  usage.excludedModels = "";
  usage.toggles.attribution.groupBy = "project";
  router.route = "sessions";
  router.params = {};
  document.body.innerHTML = "";
  localStorage.clear();
  vi.restoreAllMocks();
});

describe("Usage model visibility", () => {
  it.each(["URL", "saved filters"])(
    "restores one hidden model from %s without isolating it",
    async (source) => {
      const excluded = "model-alpha,model-bravo";
      if (source === "URL") router.params = { exclude_model: excluded };
      else usage.excludedModels = excluded;
      usage.summary = summary(excluded);
      component = mount(UsagePage, { target: document.body });
      await tick();

      expect(modelPicker().textContent).not.toContain("All");
      modelPicker().click();
      await tick();
      expect(modelOption("model-alpha")).toBeDefined();
      expect(modelOption("model-alpha").classList.contains("active")).toBe(false);
      expect(modelOption("model-charlie").classList.contains("active")).toBe(true);
      modelOption("model-alpha").click();

      await vi.waitFor(() => {
        const params = api.getApiV1UsageSummary.mock.lastCall?.[0];
        expect(params.exclude_model).toBe("model-bravo");
        expect(params.model).toBeUndefined();
        expect(
          Array.from(document.querySelectorAll(".tile title"), (tile) => tile.textContent),
        ).toEqual(["Click to hide model-alpha", "Click to hide model-charlie"]);
      });
      expect(router.params.exclude_model).toBe("model-bravo");
    },
  );

  it("keeps chart hiding, picker restoring, and bulk visibility in sync", async () => {
    component = mount(UsagePage, { target: document.body });
    await tick();
    document.querySelector(".tile")!.dispatchEvent(new MouseEvent("click", { bubbles: true }));
    await vi.waitFor(() => expect(document.querySelectorAll(".tile")).toHaveLength(2));
    expect(modelPicker().textContent).not.toContain("All");

    modelPicker().click();
    await tick();
    expect(modelOption("model-alpha").classList.contains("active")).toBe(false);
    modelOption("model-alpha").click();
    await vi.waitFor(() => expect(document.querySelectorAll(".tile")).toHaveLength(3));

    modelOption("model-bravo").click();
    await vi.waitFor(() => {
      expect(api.getApiV1UsageSummary.mock.lastCall?.[0].exclude_model).toBe("model-bravo");
      expect(document.querySelectorAll(".tile")).toHaveLength(2);
    });

    const bulk = Array.from(
      document.querySelectorAll<HTMLButtonElement>(".kit-filter-dropdown__bulk-btn"),
    );
    const deselectAll = bulk.find((button) => button.textContent === "Deselect all")!;
    const selectAll = bulk.find((button) => button.textContent === "Select all")!;
    deselectAll.click();
    await vi.waitFor(() => expect(document.querySelectorAll(".tile")).toHaveLength(0));
    expect(modelPicker().textContent).toContain("None");
    selectAll.click();
    await vi.waitFor(() => expect(document.querySelectorAll(".tile")).toHaveLength(3));
    expect(modelPicker().textContent).toContain("All");
    expect(api.getApiV1UsageSummary.mock.lastCall?.[0].exclude_model).toBeUndefined();
  });
});
