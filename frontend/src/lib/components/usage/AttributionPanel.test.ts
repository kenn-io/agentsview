import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { mount, tick, unmount } from "svelte";
import type { UsageSummaryResponse } from "../../api/types/usage.js";
import { testMoney } from "../../test/money.js";

const usageServiceMocks = vi.hoisted(() => ({
  getApiV1UsageSummary: vi.fn().mockResolvedValue({}),
  getApiV1UsageComparison: vi.fn().mockResolvedValue({}),
  getApiV1UsagePairwiseComparison: vi.fn().mockResolvedValue({}),
  getApiV1UsageTopSessions: vi.fn().mockResolvedValue([]),
}));

vi.mock("../../api/runtime.js", () => ({
  configureGeneratedClient: vi.fn(),
  callGenerated: vi.fn((request: () => Promise<unknown>) => request()),
  isAbortError: vi.fn(() => false),
}));

vi.mock("../../api/generated/index", () => ({
  UsageService: usageServiceMocks,
}));

import AttributionPanel from "./AttributionPanel.svelte";
import { settings } from "../../stores/settings.svelte.js";
import { usage } from "../../stores/usage.svelte.js";
import { usageChartColorMaps } from "../../utils/usageChartColors.js";

function summaryWithAgents(agents: string[]): UsageSummaryResponse {
  return {
    from: "2024-01-01",
    to: "2024-01-31",
    totals: {
      inputTokens: 100,
      outputTokens: 50,
      cacheCreationTokens: 0,
      cacheReadTokens: 0,
      totalCost: testMoney(12),
    },
    daily: [],
    projectTotals: [],
    modelTotals: [],
    agentTotals: agents.map((agent, i) => ({
      agent,
      inputTokens: 60 - i * 20,
      outputTokens: 30 - i * 10,
      cacheCreationTokens: 0,
      cacheReadTokens: 0,
      cost: testMoney(8 - i * 4),
    })),
    sessionCounts: { total: 2, byProject: {}, byAgent: {} },
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

function summaryWithDuplicateProjectLabels(): UsageSummaryResponse {
  const summary = summaryWithAgents([]);
  summary.projectTotals = [
    {
      project_key: "pl1:sha256:first",
      project: "",
      inputTokens: 60,
      outputTokens: 30,
      cacheCreationTokens: 0,
      cacheReadTokens: 0,
      cost: testMoney(8),
    },
    {
      project_key: "pl1:sha256:second",
      project: "",
      inputTokens: 40,
      outputTokens: 20,
      cacheCreationTokens: 0,
      cacheReadTokens: 0,
      cost: testMoney(4),
    },
  ];
  return summary;
}

function summaryWithModels(): UsageSummaryResponse {
  const summary = summaryWithAgents([]);
  summary.modelTotals = [
    {
      model: "gpt-5.6-sol",
      inputTokens: 60,
      outputTokens: 30,
      cacheCreationTokens: 0,
      cacheReadTokens: 0,
      cost: testMoney(8),
    },
    {
      model: "claude-opus-5",
      inputTokens: 40,
      outputTokens: 20,
      cacheCreationTokens: 0,
      cacheReadTokens: 0,
      cost: testMoney(4),
    },
  ];
  return summary;
}

function mountPanel(colorMap?: ReadonlyMap<string, string>) {
  const groupBy = usage.toggles.attribution.groupBy;
  return mount(AttributionPanel, {
    target: document.body,
    props: {
      colorMap: colorMap ?? usageChartColorMaps(usage.summary, settings.chartPalette)[groupBy],
    },
  });
}

describe("AttributionPanel agent exclusion", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    usage.summary = summaryWithAgents(["claude", "codex"]);
    usageServiceMocks.getApiV1UsageSummary.mockResolvedValue(
      summaryWithAgents(["claude", "codex"]),
    );
    usage.excludedAgents = "";
    usage.toggles.attribution.groupBy = "agent";
    usage.toggles.attribution.view = "list";
    settings.chartPalette = "agentsview";
  });

  afterEach(() => {
    usage.cancelInFlightReads();
    usage.summary = null;
    usage.excludedAgents = "";
    usage.applyDateRange(usage.from, usage.to);
    usage.toggles.attribution.groupBy = "project";
    document.body.innerHTML = "";
  });

  // Drives the real click path: panel click -> store toggle -> outgoing
  // request. Fails without the baseParams excludeAgent wiring.
  it("sends agent exclusions in usage queries after an attribution click", async () => {
    const component = mountPanel();
    await tick();

    const rows = document.querySelectorAll<HTMLElement>(".list-row");
    expect(rows.length).toBe(2);
    rows[1]!.click(); // exclude "codex"

    await vi.waitFor(() =>
      expect(usageServiceMocks.getApiV1UsageSummary).toHaveBeenLastCalledWith(
        expect.objectContaining({ excludeAgent: "codex" }),
      ),
    );
    unmount(component);
  });

  it("keeps the active chart brush when excluding an attribution row", async () => {
    usageServiceMocks.getApiV1UsageSummary.mockImplementationOnce(() => new Promise(() => {}));
    usage.selectedTimeRange = { from: "2024-01-08", to: "2024-01-14" };
    const component = mountPanel();
    await tick();

    document.querySelectorAll<HTMLElement>(".list-row")[1]!.click();

    expect(usage.selectedTimeRange).toEqual({
      from: "2024-01-08",
      to: "2024-01-14",
    });
    unmount(component);
  });

  it("rolls back an agent exclusion when its active-range refresh fails", async () => {
    usage.selectedTimeRange = { from: "2024-01-08", to: "2024-01-14" };
    usage.isTimeRangeSummaryProvisional = false;
    usageServiceMocks.getApiV1UsageSummary
      .mockRejectedValueOnce(new Error("filter request failed"))
      .mockResolvedValueOnce(summaryWithAgents(["claude", "codex"]));
    const component = mountPanel();
    await tick();

    document.querySelectorAll<HTMLElement>(".list-row")[1]!.click();

    await vi.waitFor(() => expect(usage.excludedAgents).toBe(""));
    expect(usage.selectedTimeRange).toEqual({
      from: "2024-01-08",
      to: "2024-01-14",
    });
    const restoredSelectionParams = usageServiceMocks.getApiV1UsageSummary.mock.calls
      .map(([params]) => params)
      .find(
        (params) =>
          params.from === "2024-01-08" &&
          params.to === "2024-01-14" &&
          params.excludeAgent === undefined,
      );
    expect(restoredSelectionParams).toEqual(
      expect.objectContaining({
        from: "2024-01-08",
        to: "2024-01-14",
      }),
    );
    unmount(component);
  });
});

describe("AttributionPanel project identity", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    usage.summary = summaryWithDuplicateProjectLabels();
    usageServiceMocks.getApiV1UsageSummary.mockResolvedValue(summaryWithDuplicateProjectLabels());
    usage.excludedProjectKeys = "";
    usage.toggles.attribution.groupBy = "project";
    usage.toggles.attribution.view = "list";
    settings.chartPalette = "agentsview";
  });

  afterEach(() => {
    usage.summary = null;
    usage.excludedProjectKeys = "";
    document.body.innerHTML = "";
  });

  it("keeps duplicate display labels distinct and filters by project key", async () => {
    const component = mountPanel();
    await tick();

    const rows = document.querySelectorAll<HTMLElement>(".list-row");
    expect(rows.length).toBe(2);
    rows[1]!.click();

    await vi.waitFor(() =>
      expect(usageServiceMocks.getApiV1UsageSummary).toHaveBeenLastCalledWith(
        expect.objectContaining({
          excludeProjectKey: "pl1:sha256:second",
        }),
      ),
    );
    unmount(component);
  });
});

describe("AttributionPanel colors", () => {
  afterEach(() => {
    usage.summary = null;
    usage.mode = "cost";
    usage.setSelectedTokenTypes(["input", "cache_write", "cache_read", "output"]);
    usage.toggles.attribution.groupBy = "project";
    usage.toggles.attribution.view = "list";
    settings.chartPalette = "agentsview";
    document.body.innerHTML = "";
  });

  it("keeps colliding model rows distinct", async () => {
    usage.summary = summaryWithModels();
    usage.toggles.attribution.groupBy = "model";
    usage.toggles.attribution.view = "list";

    const component = mountPanel();
    await tick();

    const colors = Array.from(document.querySelectorAll<HTMLElement>(".list-dot")).map((dot) =>
      dot.getAttribute("style"),
    );
    expect(new Set(colors).size).toBe(2);
    unmount(component);
  });

  it("routes distinct model colors through the treemap and rail", async () => {
    usage.summary = summaryWithModels();
    usage.toggles.attribution.groupBy = "model";
    usage.toggles.attribution.view = "treemap";

    const component = mountPanel();
    await tick();

    const tileColors = Array.from(document.querySelectorAll<SVGRectElement>(".tile rect")).map(
      (tile) => tile.getAttribute("fill"),
    );
    const railColors = Array.from(document.querySelectorAll<HTMLElement>(".rail-dot")).map(
      (dot) => dot.style.background,
    );
    expect(new Set(tileColors).size).toBe(2);
    expect(railColors).toEqual(tileColors);
    unmount(component);
  });

  it("formats treemap values as tokens in token mode", async () => {
    const summary = summaryWithAgents(["codex"]);
    summary.agentTotals[0]!.inputTokens = 750_000;
    summary.agentTotals[0]!.outputTokens = 250_000;
    usage.summary = summary;
    usage.mode = "token";
    usage.toggles.attribution.groupBy = "agent";
    usage.toggles.attribution.view = "treemap";

    const component = mountPanel();
    await tick();

    const value = document.querySelector(".tile-value")?.textContent?.trim();
    expect(value).toBe("1M");
    expect(value).not.toContain("$");
    unmount(component);
  });

  it("attributes only output tokens when Output is selected", async () => {
    const summary = summaryWithAgents(["codex"]);
    summary.agentTotals[0]!.inputTokens = 750_000;
    summary.agentTotals[0]!.cacheCreationTokens = 125_000;
    summary.agentTotals[0]!.cacheReadTokens = 2_000_000;
    summary.agentTotals[0]!.outputTokens = 250_000;
    usage.summary = summary;
    usage.mode = "token";
    usage.setSelectedTokenTypes(["output"]);
    usage.toggles.attribution.groupBy = "agent";
    usage.toggles.attribution.view = "treemap";

    const component = mountPanel();
    await tick();

    expect(document.querySelector(".tile-value")?.textContent?.trim()).toBe("250k");
    unmount(component);
  });

  it("uses the supplied map for list, treemap, and rail colors", async () => {
    usage.summary = summaryWithModels();
    usage.toggles.attribution.groupBy = "model";
    usage.toggles.attribution.view = "list";
    const supplied = new Map([
      ["gpt-5.6-sol", "#123456"],
      ["claude-opus-5", "#abcdef"],
    ]);

    const component = mountPanel(supplied);
    await tick();

    const listColors = Array.from(document.querySelectorAll<HTMLElement>(".list-dot")).map(
      (dot) => dot.style.background,
    );
    expect(listColors).toEqual(["rgb(18, 52, 86)", "rgb(171, 205, 239)"]);

    usage.toggles.attribution.view = "treemap";
    await tick();
    const tileColors = Array.from(document.querySelectorAll<SVGRectElement>(".tile rect")).map(
      (tile) => tile.getAttribute("fill"),
    );
    const railColors = Array.from(document.querySelectorAll<HTMLElement>(".rail-dot")).map(
      (dot) => dot.style.background,
    );
    expect(tileColors).toEqual(["#123456", "#abcdef"]);
    expect(railColors).toEqual(["rgb(18, 52, 86)", "rgb(171, 205, 239)"]);
    unmount(component);
  });

  it("uses lexical Matplotlib colors for colliding model representations", async () => {
    settings.chartPalette = "matplotlib";
    usage.summary = summaryWithModels();
    usage.toggles.attribution.groupBy = "model";
    usage.toggles.attribution.view = "treemap";

    const component = mountPanel();
    await tick();

    const tileColors = Array.from(document.querySelectorAll<SVGRectElement>(".tile rect")).map(
      (tile) => tile.getAttribute("fill"),
    );
    const railColors = Array.from(document.querySelectorAll<HTMLElement>(".rail-dot")).map(
      (dot) => dot.style.background,
    );
    expect(tileColors).toEqual(["#ff7f0e", "#1f77b4"]);
    expect(railColors).toEqual(["rgb(255, 127, 14)", "rgb(31, 119, 180)"]);
    unmount(component);
  });
});
