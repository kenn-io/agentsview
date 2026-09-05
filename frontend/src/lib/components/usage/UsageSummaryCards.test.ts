// @vitest-environment jsdom
import { afterEach, describe, expect, it } from "vite-plus/test";
import { mount, tick, unmount } from "svelte";
import { usage } from "../../stores/usage.svelte.js";
import { testMoney } from "../../test/money.js";
import type { UsageSummaryResponse } from "../../api/generated/index";
import UsageSummaryCards from "./UsageSummaryCards.svelte";

let component: ReturnType<typeof mount> | undefined;

function summary(): UsageSummaryResponse {
  return {
    from: "2026-07-01",
    to: "2026-07-01",
    projects: {},
    totals: {
      inputTokens: 100,
      cacheCreationTokens: 40,
      cacheReadTokens: 800,
      outputTokens: 25,
      totalCost: testMoney(1),
      cacheSavings: testMoney(0),
    },
    daily: [
      {
        date: "2026-07-01",
        inputTokens: 100,
        cacheCreationTokens: 40,
        cacheReadTokens: 800,
        outputTokens: 25,
        totalCost: testMoney(1),
        modelsUsed: ["model"],
        modelBreakdowns: [],
        projectBreakdowns: [],
        agentBreakdowns: [],
        machineBreakdowns: [],
      },
    ],
    projectTotals: [],
    modelTotals: [],
    agentTotals: [],
    sessionCounts: {
      total: 1,
      byProject: { demo: 1 },
      byAgent: { codex: 1 },
    },
    cacheStats: {
      cacheReadTokens: 800,
      cacheCreationTokens: 40,
      uncachedInputTokens: 100,
      outputTokens: 25,
      hitRate: 0.8,
      savingsVsUncached: testMoney(0),
    },
  };
}

afterEach(() => {
  if (component) {
    unmount(component);
    component = undefined;
  }
  if (usage.selectedTimeRange !== null) {
    usage.clearTimeRange();
  }
  usage.cancelInFlightReads();
  usage.summary = null;
  usage.mode = "cost";
  usage.setSelectedTokenTypes(["input", "cache_write", "cache_read", "output"]);
  document.body.innerHTML = "";
});

describe("UsageSummaryCards", () => {
  it("uses the selected token types for aggregate token cards", async () => {
    usage.summary = summary();
    usage.mode = "token";
    usage.setSelectedTokenTypes(["output"]);

    component = mount(UsageSummaryCards, {
      target: document.body,
    });
    await tick();

    expect(document.querySelector(".featured .card-value")?.textContent?.trim()).toBe("25");
    const labels = Array.from(document.querySelectorAll<HTMLElement>(".card-label"));
    const dailyBurn = labels
      .find((label) => label.textContent?.trim() === "Daily Burn")
      ?.previousElementSibling?.textContent?.trim();
    const peakDay = labels
      .find((label) => label.textContent?.trim() === "Peak Day")
      ?.previousElementSibling?.textContent?.trim();
    expect(dailyBurn).toBe("25");
    expect(peakDay).toBe("25");
  });

  it("keeps the Copilot credits card while a brushed range is active", async () => {
    const parent = summary();
    parent.from = "2026-07-01";
    parent.to = "2026-07-03";
    parent.totals.copilotAICredits = 5;
    usage.summary = parent;

    component = mount(UsageSummaryCards, {
      target: document.body,
    });
    await tick();
    const cardCount = document.querySelectorAll(".summary-cards .card").length;

    usage.setTimeRange("2026-07-01", "2026-07-02");
    usage.cancelInFlightReads();
    await tick();

    expect(document.querySelectorAll(".summary-cards .card")).toHaveLength(cardCount);
    expect(document.body.textContent).toContain("Copilot AI Credits");
  });
});
