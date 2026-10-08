import { afterEach, beforeEach, describe, expect, it, vi } from "vite-plus/test";
import { mount, tick, unmount } from "svelte";
import type { DbTopSessionEntry, UsageSummaryResponse } from "../../api/generated/index";
import { testMoney } from "../../test/money.js";

const usageServiceMocks = vi.hoisted(() => ({
  getApiV1UsageSummary: vi.fn().mockResolvedValue({}),
  getApiV1UsageComparison: vi.fn().mockResolvedValue({}),
  getApiV1UsagePairwiseComparison: vi.fn().mockResolvedValue({}),
  getApiV1UsageTopSessions: vi.fn().mockResolvedValue([]),
}));

vi.mock("../../api/runtime.js", async (importOriginal) => ({
  ...(await importOriginal<typeof import("../../api/runtime.js")>()),
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
    projects: {},
    totals: {
      inputTokens: 100,
      outputTokens: 50,
      cacheCreationTokens: 0,
      cacheReadTokens: 0,
      totalCost: testMoney(12),
      cacheSavings: testMoney(0),
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
      expect(usageServiceMocks.getApiV1UsageSummary.mock.lastCall?.[0]).toEqual(
        expect.objectContaining({ exclude_agent: "codex" }),
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
          params.exclude_agent === undefined,
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
    usage.backToProjects();
    usage.summary = null;
    usage.excludedProjectKeys = "";
    document.body.innerHTML = "";
  });

  it("keeps duplicate display labels distinct and zooms by project key", async () => {
    const component = mountPanel();
    await tick();

    const rows = document.querySelectorAll<HTMLElement>(".list-row");
    expect(rows.length).toBe(2);
    rows[1]!.click();

    await vi.waitFor(() =>
      expect(usageServiceMocks.getApiV1UsageTopSessions.mock.lastCall?.[0]).toEqual(
        expect.objectContaining({
          project_key: "pl1:sha256:second",
        }),
      ),
    );
    unmount(component);
  });
});

describe("AttributionPanel model exclusion", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    usage.summary = summaryWithModels();
    usage.excludedModels = "";
    usage.toggles.attribution.groupBy = "model";
  });

  afterEach(() => {
    usage.cancelInFlightReads();
    usage.summary = null;
    usage.excludedModels = "";
    usage.applyDateRange(usage.from, usage.to);
    usage.toggles.attribution.groupBy = "project";
    usage.toggles.attribution.view = "list";
    document.body.innerHTML = "";
  });

  it.each([
    ["treemap", ".tile"],
    ["treemap", ".rail-row"],
    ["list", ".list-row"],
  ] as const)("hides a model through %s %s instead of selecting it", async (view, selector) => {
    usage.toggles.attribution.view = view;
    const remaining = summaryWithModels();
    remaining.modelTotals = [remaining.modelTotals[1]!];
    usageServiceMocks.getApiV1UsageSummary.mockResolvedValue(remaining);
    const component = mountPanel();
    await tick();

    try {
      document.querySelector(selector)!.dispatchEvent(new MouseEvent("click", { bubbles: true }));

      await vi.waitFor(() => {
        const params = usageServiceMocks.getApiV1UsageSummary.mock.lastCall?.[0];
        expect(params).toEqual(expect.objectContaining({ exclude_model: "gpt-5.6-sol" }));
        expect(params.model).toBeUndefined();
      });
      await tick();
      expect(Array.from(document.querySelectorAll(selector), (row) => row.textContent)).toEqual([
        expect.stringContaining("claude-opus-5"),
      ]);
      expect(usage.hasActiveFilters).toBe(true);

      const empty = summaryWithModels();
      empty.modelTotals = [];
      usageServiceMocks.getApiV1UsageSummary.mockResolvedValue(empty);
      document.querySelector(selector)!.dispatchEvent(new MouseEvent("click", { bubbles: true }));
      await vi.waitFor(() =>
        expect(usageServiceMocks.getApiV1UsageSummary.mock.lastCall?.[0]).toEqual(
          expect.objectContaining({ exclude_model: "gpt-5.6-sol,claude-opus-5" }),
        ),
      );
      await tick();
      expect(document.querySelectorAll(selector)).toHaveLength(0);

      usageServiceMocks.getApiV1UsageSummary.mockResolvedValue(summaryWithModels());
      usage.clearFilters();
      await vi.waitFor(() => expect(document.querySelectorAll(selector)).toHaveLength(2));
      expect(
        usageServiceMocks.getApiV1UsageSummary.mock.lastCall?.[0].exclude_model,
      ).toBeUndefined();
    } finally {
      await unmount(component);
    }
  });

  it("keeps other hidden models and the chart brush when hiding a model", async () => {
    usage.excludedModels = "model-other";
    usage.selectedTimeRange = { from: "2024-01-08", to: "2024-01-14" };
    usage.toggles.attribution.view = "treemap";
    usageServiceMocks.getApiV1UsageSummary.mockResolvedValue(summaryWithModels());
    const component = mountPanel();
    await tick();

    try {
      document.querySelector(".tile")!.dispatchEvent(new MouseEvent("click", { bubbles: true }));
      await vi.waitFor(() =>
        expect(
          usageServiceMocks.getApiV1UsageSummary.mock.calls.map(([params]) => params),
        ).toContainEqual(
          expect.objectContaining({
            from: "2024-01-08",
            to: "2024-01-14",
            exclude_model: "model-other,gpt-5.6-sol",
          }),
        ),
      );
      expect(usage.selectedTimeRange).toEqual({ from: "2024-01-08", to: "2024-01-14" });
    } finally {
      await unmount(component);
    }
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

  it("uses aggregate-cost-ranked Matplotlib colors for model representations", async () => {
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
    expect(tileColors).toEqual(["#1f77b4", "#ff7f0e"]);
    expect(railColors).toEqual(["rgb(31, 119, 180)", "rgb(255, 127, 14)"]);
    unmount(component);
  });
});

describe("AttributionPanel job groups", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    usage.backToProjects();
    usage.zoomRows = null;
    usage.summary = summaryWithDuplicateProjectLabels();
    usage.summary.projectTotals[0]!.project = "hermes-cron";
    usage.excludedProjectKeys = "";
    usage.mode = "cost";
    usage.toggles.attribution.groupBy = "project";
    usage.toggles.attribution.view = "list";
  });
  afterEach(() => {
    usage.cancelInFlightReads();
    usage.backToProjects();
    usage.summary = null;
    usage.excludedProjectKeys = "";
    usageServiceMocks.getApiV1UsageTopSessions.mockResolvedValue([]);
    document.body.innerHTML = "";
  });

  it("zooms into stable jobs, keeps the remainder, and returns to projects", async () => {
    usageServiceMocks.getApiV1UsageSummary.mockResolvedValue(usage.summary);
    await usage.fetchSummary({ loadComparison: false });
    const group = (key: string, cost: number): DbTopSessionEntry => ({
      group_key: key,
      group_label: "Daily digest",
      sessionId: key,
      displayName: "Daily digest",
      project: "hermes-cron",
      agent: "hermes",
      startedAt: "",
      inputTokens: 10,
      outputTokens: 0,
      cacheCreationTokens: 0,
      cacheReadTokens: 0,
      totalTokens: 10,
      cost: testMoney(cost),
    });
    usageServiceMocks.getApiV1UsageTopSessions.mockResolvedValue([
      group("abcdef-job", 3),
      group("abcdef-other", 2),
      { ...group("", 1), sessionId: "hermes:ungrouped", displayName: "Ungrouped run" },
    ]);
    const component = mountPanel();
    await tick();
    document.querySelectorAll<HTMLElement>(".list-row")[0]!.click();
    await vi.waitFor(() => expect(usage.zoomedProjectKey).toBe("pl1:sha256:first"));
    expect(usage.excludedProjectKeys).toBe("");
    expect(usageServiceMocks.getApiV1UsageTopSessions).toHaveBeenCalledTimes(1);
    await vi.waitFor(() => expect(usage.loading.zoom).toBe(false));
    await tick();
    const rows = [...document.querySelectorAll<HTMLElement>(".list-row")];
    expect(rows.map((row) => row.querySelector(".list-label")!.textContent)).toEqual([
      "Daily digest · abcdef-j",
      "Daily digest · abcdef-o",
      "Other",
      "Ungrouped run",
    ]);
    expect(rows.map((row) => row.querySelector(".list-cost")!.textContent?.trim())).toEqual([
      "$3.00",
      "$2.00",
      "$2.00",
      "$1.00",
    ]);
    expect(rows[0]!.title).toBe("Daily digest · abcdef-job");
    expect(
      new Set(rows.map((row) => row.querySelector(".list-dot")?.getAttribute("style"))).size,
    ).toBe(4);
    rows[0]!.click();
    expect(usage.excludedProjectKeys).toBe("");
    const back = [...document.querySelectorAll<HTMLButtonElement>("button")].find(
      (button) => button.textContent?.trim() === "All projects",
    )!;
    back.click();
    await tick();
    expect(usage.zoomedProjectKey).toBeNull();
    expect(document.querySelectorAll(".list-row")).toHaveLength(2);
    unmount(component);
  });

  it("omits Other when summary fails and zoom succeeds after a date change", async () => {
    const summary = summaryWithDuplicateProjectLabels();
    summary.projectTotals[0]!.project = "hermes-cron";
    summary.projectTotals[0]!.cost = testMoney(100);
    usageServiceMocks.getApiV1UsageSummary.mockResolvedValue(summary);
    await usage.fetchAll();
    const component = mountPanel();
    await tick();
    document.querySelectorAll<HTMLElement>(".list-row")[0]!.click();
    await vi.waitFor(() => expect(usage.loading.zoom).toBe(false));
    usageServiceMocks.getApiV1UsageSummary.mockRejectedValue(new Error("Summary unavailable"));
    usageServiceMocks.getApiV1UsageTopSessions.mockImplementation((params) =>
      Promise.resolve(params.group_by === "group" ? [{
        group_key: "job-a",
        group_label: "Digest",
        sessionId: "",
        displayName: "Digest",
        project: "hermes-cron",
        agent: "hermes",
        startedAt: "",
        inputTokens: 10,
        outputTokens: 0,
        cacheCreationTokens: 0,
        cacheReadTokens: 0,
        totalTokens: 10,
        cost: testMoney(2),
      }] : []),
    );
    usage.applyDateRange("2024-02-01", "2024-02-29");
    await usage.fetchAll();
    await tick();
    expect(usageServiceMocks.getApiV1UsageSummary.mock.lastCall?.[0]).toEqual(expect.objectContaining({ from: "2024-02-01", to: "2024-02-29" }));
    expect(usageServiceMocks.getApiV1UsageTopSessions.mock.calls).toContainEqual([
      expect.objectContaining({ from: "2024-02-01", to: "2024-02-29", group_by: "group" }),
      expect.anything(),
    ]);
    expect([...document.querySelectorAll(".list-label")].map((row) => row.textContent)).toEqual(["Digest"]);
    expect(document.querySelector(".list-cost")?.textContent?.trim()).toBe("$2.00");
    await unmount(component);
  });

  it("hides the project from the zoom breadcrumb", async () => {
    usageServiceMocks.getApiV1UsageTopSessions.mockResolvedValue([]);
    usageServiceMocks.getApiV1UsageSummary.mockResolvedValue(summaryWithDuplicateProjectLabels());
    const component = mountPanel();
    await tick();
    document.querySelectorAll<HTMLElement>(".list-row")[0]!.click();
    await tick();
    [...document.querySelectorAll<HTMLButtonElement>("button")]
      .find((button) => button.textContent?.trim() === "Hide project")!
      .click();
    expect(usage.zoomedProjectKey).toBeNull();
    expect(usage.excludedProjectKeys).toBe("pl1:sha256:first");
    await vi.waitFor(() =>
      expect(usageServiceMocks.getApiV1UsageSummary.mock.lastCall?.[0]).toEqual(
        expect.objectContaining({ exclude_project_key: "pl1:sha256:first" }),
      ),
    );
    await unmount(component);
  });

  it("clears zoom when switching attribution dimensions", async () => {
    usage.zoomedProjectKey = "pl1:sha256:first";
    const component = mountPanel();
    await tick();
    [...document.querySelectorAll<HTMLButtonElement>("button")]
      .find((button) => button.textContent?.trim() === "Agent")!
      .click();
    expect(usage.zoomedProjectKey).toBeNull();
    unmount(component);
  });
  it("clears zoom when the project leaves the refreshed summary", async () => {
    usage.zoomedProjectKey = "pl1:sha256:first";
    usage.zoomRows = [];
    usageServiceMocks.getApiV1UsageSummary.mockResolvedValue(summaryWithAgents([]));
    usageServiceMocks.getApiV1UsageTopSessions.mockResolvedValue([]);
    await usage.fetchAll({ preserveTimeRange: true });
    expect(usage.zoomedProjectKey).toBeNull();
    expect(usage.zoomRows).toBeNull();
  });

  it("clears zoom rows when returning to projects", async () => {
    usage.zoomRows = [];
    usage.zoomedProjectKey = "pl1:sha256:first";
    usage.backToProjects();
    expect(usage.zoomedProjectKey).toBeNull();
    expect(usage.zoomRows).toBeNull();
    expect(usageServiceMocks.getApiV1UsageTopSessions).not.toHaveBeenCalled();
  });
});
