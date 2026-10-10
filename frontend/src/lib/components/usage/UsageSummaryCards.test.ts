// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from "vite-plus/test";
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

function issueSummary(): UsageSummaryResponse {
  const s = summary();
  const totals = {
    inputTokens: 248_600_000,
    cacheCreationTokens: 1_400_000,
    cacheReadTokens: 7_650_000_000,
    outputTokens: 20_000_000,
  };
  s.totals = { ...s.totals, ...totals };
  s.daily = [{ ...s.daily[0]!, ...totals }];
  return s;
}

function cardLabels(): string[] {
  return Array.from(document.querySelectorAll<HTMLElement>(".card-label")).map(
    (label) => label.textContent?.trim() ?? "",
  );
}

function cardFor(label: string): HTMLElement | undefined {
  return Array.from(document.querySelectorAll<HTMLElement>(".card-label")).find(
    (el) => el.textContent?.trim() === label,
  )?.parentElement ?? undefined;
}

function cardValue(label: string): string | undefined {
  return cardFor(label)?.querySelector(".card-value")?.textContent?.trim();
}

function cardSub(label: string): string | undefined {
  return cardFor(label)?.querySelector(".card-sub")?.textContent?.trim();
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
  usage.referenceSummary = null;
  usage.selectedProjectKey = "";
  usage.errors.summary = null;
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

  it.each(["a brushed range is active", "a selected project has no credits"])("keeps the Copilot credits card while %s", async (situation) => {
    const parent = summary();
    parent.from = "2026-07-01";
    parent.to = "2026-07-03";
    parent.totals.copilotAICredits = 5;
    usage.summary = parent;
    if (situation !== "a brushed range is active") {
      usage.selectedProjectKey = "pl1:sha256:selected";
      usage.referenceSummary = parent;
    }
    component = mount(UsageSummaryCards, { target: document.body });
    await tick();
    const cardCount = document.querySelectorAll(".summary-cards .card").length;

    if (situation === "a brushed range is active") {
      usage.setTimeRange("2026-07-01", "2026-07-02");
      usage.cancelInFlightReads();
    } else usage.summary = summary();
    await tick();
    expect(document.querySelectorAll(".summary-cards .card")).toHaveLength(cardCount);
    expect(document.body.textContent).toContain("Copilot AI Credits");
  });

  it("labels uncached input and adds total input", async () => {
    usage.summary = issueSummary();

    component = mount(UsageSummaryCards, {
      target: document.body,
    });
    await tick();

    expect(cardLabels()).toEqual([
      "Total Cost",
      "Total Input",
      "Uncached Input",
      "Output Tokens",
      "Daily Burn",
      "Peak Day",
      "Cache Hit",
      "Projects",
      "Models",
      "Active Days",
    ]);
    expect(cardValue("Total Input")).toBe("7.9B");
    expect(cardValue("Uncached Input")).toBe("248.6M");
    expect(cardSub("Uncached Input")).toBe("+7.6B cached");

    unmount(component);
    component = undefined;
    const uncachedOnly = issueSummary();
    uncachedOnly.totals.cacheReadTokens = 0;
    usage.summary = uncachedOnly;
    component = mount(UsageSummaryCards, {
      target: document.body,
    });
    await tick();

    expect(cardValue("Uncached Input")).toBe("248.6M");
    expect(cardSub("Uncached Input")).toBeUndefined();
  });

  it("shows total and uncached input in token mode", async () => {
    usage.summary = issueSummary();
    usage.mode = "token";

    component = mount(UsageSummaryCards, {
      target: document.body,
    });
    await tick();

    expect(cardLabels().slice(0, 4)).toEqual([
      "Total Tokens",
      "Total Input",
      "Uncached Input",
      "Output Tokens",
    ]);
    expect(cardValue("Total Input")).toBe("7.9B");
    expect(cardValue("Uncached Input")).toBe("248.6M");

    usage.setSelectedTokenTypes(["input"]);
    await tick();
    expect(document.querySelector(".featured .card-label")?.textContent?.trim()).toBe(
      "Uncached Input",
    );

    usage.setSelectedTokenTypes(["input", "output"]);
    await tick();
    expect(document.querySelector(".featured .card-label")?.textContent?.trim()).toBe(
      "Selected Tokens",
    );

    usage.setSelectedTokenTypes(["output"]);
    await tick();
    expect(document.querySelector(".featured .card-value")?.textContent?.trim()).toBe("20M");
    expect(cardValue("Total Input")).toBe("7.9B");
  });

  it("recomputes total input for a brushed range", async () => {
    const parent = summary();
    parent.from = "2026-07-01";
    parent.to = "2026-07-02";
    const day = parent.daily[0]!;
    parent.daily = [
      { ...day, date: "2026-07-01", inputTokens: 100, cacheCreationTokens: 10, cacheReadTokens: 900 },
      { ...day, date: "2026-07-02", inputTokens: 50, cacheCreationTokens: 5, cacheReadTokens: 450 },
    ];
    parent.totals = {
      ...parent.totals,
      inputTokens: 150,
      cacheCreationTokens: 15,
      cacheReadTokens: 1350,
    };
    usage.summary = parent;

    component = mount(UsageSummaryCards, {
      target: document.body,
    });
    await tick();
    expect(cardValue("Total Input")).toBe("1.5K");

    usage.setTimeRange("2026-07-02", "2026-07-03");
    usage.cancelInFlightReads();
    await tick();

    expect(cardValue("Total Input")).toBe("505");
  });

  it("renders the total input card in the summary error state and retries the full refresh", async () => {
    const refresh = vi.spyOn(usage, "fetchAll").mockResolvedValue();
    usage.summary = issueSummary();
    usage.errors.summary = "boom";

    component = mount(UsageSummaryCards, {
      target: document.body,
    });
    await tick();

    expect(document.querySelectorAll(".summary-cards .card")).toHaveLength(10);
    expect(cardValue("Total Input")).toBe("--");
    // Retry keeps the brush and refetches its full-window context like any refresh.
    document.querySelector<HTMLButtonElement>(".retry-btn")!.click();
    expect(refresh).toHaveBeenCalledExactlyOnceWith({ preserveTimeRange: true });
    refresh.mockRestore();
  });
});
