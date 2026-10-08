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

describe("AttributionPanel agent focus", () => {
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

  it("focuses page stats on the clicked agent", async () => {
    const component = mountPanel();
    await tick();

    const rows = document.querySelectorAll<HTMLElement>(".list-row");
    expect(rows.length).toBe(2);
    rows[1]!.click();

    await vi.waitFor(() =>
      expect(usageServiceMocks.getApiV1UsageSummary.mock.lastCall?.[0]).toEqual(
        expect.objectContaining({ exclude_agent: "claude" }),
      ),
    );
    unmount(component);
  });

  it("keeps the active chart brush when focusing an attribution row", async () => {
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

  it("rolls back agent focus when its active-range refresh fails", async () => {
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
    vi.spyOn(window.history, "back").mockImplementation(() =>
      window.dispatchEvent(new PopStateEvent("popstate")),
    );
    usage.backToProjects();
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
    vi.restoreAllMocks();
  });

  it("keeps duplicate display labels distinct and zooms by project key", async () => {
    const component = mountPanel();
    await tick();

    const rows = document.querySelectorAll<HTMLElement>(".list-row");
    expect(rows.length).toBe(2);
    rows[1]!.dispatchEvent(new MouseEvent("dblclick", { bubbles: true }));

    await vi.waitFor(() =>
      expect(usageServiceMocks.getApiV1UsageTopSessions.mock.lastCall?.[0]).toEqual(
        expect.objectContaining({
          project_key: "pl1:sha256:second",
        }),
      ),
    );
    unmount(component);
  });

  it.each([
    ["treemap", ".tile"],
    ["treemap", ".rail-row"],
    ["list", ".list-row"],
  ] as const)(
    "selects on click and opens on double-click through %s %s",
    async (view, selector) => {
      usage.toggles.attribution.view = view;
      const component = mountPanel();
      await tick();
      const row = document.querySelectorAll(selector)[1]!;
      row.dispatchEvent(new MouseEvent("click", { detail: 1, bubbles: true }));
      expect(usage.excludedProjectKeys).toBe("pl1:sha256:first");
      expect(usage.zoomedProject).toBeNull();
      expect(usageServiceMocks.getApiV1UsageSummary.mock.lastCall?.[0]).toEqual(
        expect.objectContaining({ exclude_project_key: "pl1:sha256:first" }),
      );
      row.dispatchEvent(new MouseEvent("click", { detail: 2, bubbles: true }));
      row.dispatchEvent(new MouseEvent("dblclick", { bubbles: true }));
      expect(usage.excludedProjectKeys).toBe("pl1:sha256:first");
      expect(usage.zoomedProject?.key).toBe("pl1:sha256:second");
      await unmount(component);
    },
  );

  it.each([
    ["treemap", ".tile"],
    ["treemap", ".rail-row"],
    ["list", ".list-row"],
  ] as const)("selects with Space and opens with Enter through %s %s", async (view, selector) => {
    usage.toggles.attribution.view = view;
    const component = mountPanel();
    await tick();
    const row = document.querySelectorAll(selector)[1]!;
    row.dispatchEvent(new KeyboardEvent("keydown", { key: " ", bubbles: true, cancelable: true }));
    expect(usage.excludedProjectKeys).toBe("pl1:sha256:first");
    expect(usage.zoomedProject).toBeNull();
    row.dispatchEvent(
      new KeyboardEvent("keydown", { key: "Enter", bubbles: true, cancelable: true }),
    );
    expect(usage.zoomedProject?.key).toBe("pl1:sha256:second");
    await unmount(component);
  });

  it.each([
    ["treemap", ".tile"],
    ["treemap", ".rail-row"],
    ["list", ".list-row"],
  ] as const)("keeps touch taps as selection through %s %s", async (view, selector) => {
    usage.toggles.attribution.view = view;
    const component = mountPanel();
    await tick();
    const row = document.querySelectorAll(selector)[1]!;
    for (const detail of [1, 2]) {
      const tap = new MouseEvent("click", { detail, bubbles: true });
      Object.defineProperty(tap, "pointerType", { value: "touch" });
      row.dispatchEvent(tap);
      expect(usage.excludedProjectKeys).toBe(detail === 1 ? "pl1:sha256:first" : "");
    }
    row.dispatchEvent(new MouseEvent("dblclick", { bubbles: true }));
    expect(usage.zoomedProject).toBeNull();
    await unmount(component);
  });
});

describe("AttributionPanel model focus", () => {
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
  ] as const)(
    "focuses a model through %s %s and clears focus on another click",
    async (view, selector) => {
      usage.toggles.attribution.view = view;
      const remaining = summaryWithModels();
      remaining.modelTotals = [remaining.modelTotals[0]!];
      usageServiceMocks.getApiV1UsageSummary.mockResolvedValue(remaining);
      const component = mountPanel();
      await tick();

      try {
        document.querySelector(selector)!.dispatchEvent(new MouseEvent("click", { bubbles: true }));

        await vi.waitFor(() => {
          const params = usageServiceMocks.getApiV1UsageSummary.mock.lastCall?.[0];
          expect(params).toEqual(expect.objectContaining({ exclude_model: "claude-opus-5" }));
          expect(params.model).toBeUndefined();
        });
        await tick();
        expect(Array.from(document.querySelectorAll(selector), (row) => row.textContent)).toEqual([
          expect.stringContaining("gpt-5.6-sol"),
        ]);
        expect(usage.hasActiveFilters).toBe(true);

        expect(document.querySelector(selector)?.getAttribute("aria-pressed")).toBe("true");
        usageServiceMocks.getApiV1UsageSummary.mockResolvedValue(summaryWithModels());
        document.querySelector(selector)!.dispatchEvent(new MouseEvent("click", { detail: 2, bubbles: true }));
        await vi.waitFor(() => expect(document.querySelectorAll(selector)).toHaveLength(2));
        expect(
          usageServiceMocks.getApiV1UsageSummary.mock.lastCall?.[0].exclude_model,
        ).toBeUndefined();
      } finally {
        await unmount(component);
      }
    },
  );

  it("keeps other hidden models and the chart brush when focusing a model", async () => {
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
            exclude_model: "model-other,claude-opus-5",
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

const topSessionForRemainder = (): DbTopSessionEntry => ({
  sessionId: "",
  displayName: "",
  project: "",
  agent: "",
  startedAt: "",
  inputTokens: 0,
  outputTokens: 0,
  cacheCreationTokens: 0,
  cacheReadTokens: 0,
  totalTokens: 0,
  cost: testMoney(1),
});

describe("AttributionPanel job groups", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    vi.spyOn(window.history, "back").mockImplementation(() =>
      window.dispatchEvent(new PopStateEvent("popstate")),
    );
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
    vi.restoreAllMocks();
  });

  it("zooms into stable jobs, keeps the remainder, and returns to projects", async () => {
    usageServiceMocks.getApiV1UsageSummary.mockResolvedValue(usage.summary);
    await usage.fetchSummary({ loadComparison: false });
    const group = (key: string, cost: number): DbTopSessionEntry => ({
      groupKey: key,
      groupLabel: "Daily digest",
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
      { ...group("", 2), groupLabel: "", sessionId: "", displayName: "" },
      { ...group("remainder", 0.5), groupLabel: "Other" },
      { ...group("", 0.4), sessionId: "hermes:", displayName: "Repeated run" },
      { ...group("", 0.3), sessionId: "hermes:run-b", displayName: "Repeated run" },
    ]);
    const component = mountPanel();
    await tick();
    document
      .querySelectorAll<HTMLElement>(".list-row")[0]!
      .dispatchEvent(new MouseEvent("dblclick", { bubbles: true }));
    await vi.waitFor(() => expect(usage.zoomedProject?.key).toBe("pl1:sha256:first"));
    await vi.waitFor(() => expect(usage.loading.zoom).toBe(false));
    await tick();
    expect(document.activeElement).toBe(document.querySelector(".attribution-panel"));
    const rows = [...document.querySelectorAll<HTMLElement>(".list-row")];
    expect(rows.map((row) => row.querySelector(".list-label")!.textContent)).toEqual([
      "Daily digest · abcdef-j",
      "Daily digest · abcdef-o",
      "Other",
      "Ungrouped run",
      "Other",
      "Repeated run · hermes:",
      "Repeated run · run-b",
    ]);
    expect(rows.map((row) => row.querySelector(".list-cost")!.textContent?.trim())).toEqual([
      "$3.00",
      "$2.00",
      "$2.00",
      "$1.00",
      "$0.50",
      "$0.40",
      "$0.30",
    ]);
    expect(rows[0]!.title).toBe("Daily digest · abcdef-job");
    expect(rows[4]!.title).toBe("Other · remainder");
    expect(rows[2]!.title).toBe("Other");
    expect(
      new Set(rows.map((row) => row.querySelector(".list-dot")?.getAttribute("style"))).size,
    ).toBe(1);
    rows[0]!.click();
    const back = [...document.querySelectorAll<HTMLButtonElement>("button")].find(
      (button) => button.textContent?.trim() === "← All projects",
    )!;
    back.click();
    await tick();
    expect(usage.zoomedProject).toBeNull();
    expect(document.querySelectorAll(".list-row")).toHaveLength(2);
    unmount(component);
  });

  it("uses zoom rows and remainder when summary fails after a date change", async () => {
    usage.applyDateRange("2024-02-01", "2024-02-29");
    usage.summary = null;
    usage.zoomedProject = { key: "pl1:sha256:first", label: "hermes-cron" };
    usage.zoomRows = [
      { ...topSessionForRemainder(), groupKey: "job-a", groupLabel: "Digest", cost: testMoney(2) },
      {
        ...topSessionForRemainder(),
        groupKey: "job-b",
        groupLabel: "Research",
        cost: testMoney(3),
      },
      topSessionForRemainder(),
    ];
    const component = mountPanel();
    await tick();
    expect([...document.querySelectorAll(".list-label")].map((row) => row.textContent)).toEqual([
      "Research",
      "Digest",
      "Other",
    ]);
    expect(
      [...document.querySelectorAll(".list-cost")].map((row) => row.textContent?.trim()),
    ).toEqual(["$3.00", "$2.00", "$1.00"]);
    expect(
      [...document.querySelectorAll(".list-pct")].map((row) => row.textContent?.trim()),
    ).toEqual(["50.0%", "33.3%", "16.7%"]);
    await unmount(component);
  });

  it("shows the job ID for an unnamed group in its label and tooltip", async () => {
    usage.zoomedProject = { key: "pl1:sha256:first", label: "hermes-cron" };
    usage.zoomRows = [
      {
        ...topSessionForRemainder(),
        groupKey: "digest",
        groupLabel: "",
        displayName: "digest",
      },
    ];
    const component = mountPanel();
    await tick();
    const row = document.querySelector<HTMLElement>(".list-row")!;
    expect(row.querySelector(".list-label")!.textContent).toBe("digest");
    expect(row.title).toBe("digest");
    await unmount(component);
  });

  it("distinguishes matching jobs across machines", async () => {
    usage.zoomedProject = { key: "pl1:sha256:first", label: "hermes-cron" };
    usage.zoomRows = ["host-a", "host-b"].map((machine) => ({
      ...topSessionForRemainder(), groupKey: "digest", machine, groupLabel: "Daily digest",
    }));
    const component = mountPanel();
    await tick();
    const rows = document.querySelectorAll<HTMLElement>(".list-row");
    expect(Array.from(rows, (row) => row.querySelector(".list-label")!.textContent)).toEqual([
      "Daily digest · host-a",
      "Daily digest · host-b",
    ]);
    expect(Array.from(rows, (row) => row.title)).toEqual([
      "Daily digest · digest · host-a",
      "Daily digest · digest · host-b",
    ]);
    await unmount(component);
  });

  it.each(["Escape", "Backspace"])(
    "returns to projects with %s while the panel has focus",
    async (key) => {
      const back = vi
        .spyOn(window.history, "back")
        .mockImplementation(() => window.dispatchEvent(new PopStateEvent("popstate")));
      const component = mountPanel();
      usage.selectAttributionProject("pl1:sha256:first", "hermes-cron");
      await tick();
      const panel = document.querySelector<HTMLElement>(".attribution-panel")!;
      panel.focus();
      panel.dispatchEvent(new KeyboardEvent("keydown", { key, bubbles: true, cancelable: true }));
      await tick();
      expect(usage.zoomedProject).toBeNull();
      expect(back).toHaveBeenCalledOnce();
      expect(document.querySelectorAll(".list-row")).toHaveLength(2);
      await unmount(component);
      back.mockRestore();
    },
  );

  it("balances button Back and reopening before the history event", async () => {
    const back = vi.mocked(window.history.back).mockImplementation(() => {});
    const push = vi.spyOn(window.history, "pushState");
    const component = mountPanel();
    usage.selectAttributionProject("pl1:sha256:first", "hermes-cron");
    await tick();
    [...document.querySelectorAll<HTMLButtonElement>("button")]
      .find((button) => button.textContent?.trim() === "← All projects")!.click();
    expect(usage.zoomedProject).toBeNull();
    expect(back).toHaveBeenCalledOnce();
    usage.selectAttributionProject("pl1:sha256:second", "Another project");
    expect(push).toHaveBeenCalledOnce();
    window.dispatchEvent(new PopStateEvent("popstate"));
    await tick();
    expect(usage.zoomedProject?.key).toBe("pl1:sha256:second");
    expect(push).toHaveBeenCalledTimes(2);
    back.mockImplementation(() => window.dispatchEvent(new PopStateEvent("popstate")));
    await unmount(component);
    push.mockRestore();
  });

  it("opens without changing the URL and returns on browser Back without another pop", async () => {
    const push = vi.spyOn(window.history, "pushState");
    const back = vi.spyOn(window.history, "back");
    const url = window.location.href;
    const component = mountPanel();
    usage.selectAttributionProject("pl1:sha256:first", "hermes-cron");
    await tick();
    expect(push).toHaveBeenCalledOnce();
    expect(window.location.href).toBe(url);
    window.dispatchEvent(new PopStateEvent("popstate"));
    await tick();
    expect(usage.zoomedProject).toBeNull();
    expect(back).not.toHaveBeenCalled();
    expect(document.querySelectorAll(".list-row")).toHaveLength(2);
    await unmount(component);
    push.mockRestore();
    back.mockRestore();
  });

  it("keeps providers when ungrouped session IDs collide", async () => {
    usage.zoomedProject = { key: "pl1:sha256:first", label: "hermes-cron" };
    usage.zoomRows = ["hermes:run-a", "augure-desktop:run-a"].map((sessionId) => ({
      ...topSessionForRemainder(),
      sessionId,
      displayName: "Repeated run",
    }));
    const component = mountPanel();
    await tick();
    const rows = document.querySelectorAll<HTMLElement>(".list-row");
    expect(Array.from(rows, (row) => row.querySelector(".list-label")!.textContent)).toEqual([
      "Repeated run · hermes:r",
      "Repeated run · augure-d",
    ]);
    expect(Array.from(rows, (row) => row.title)).toEqual([
      "Repeated run · hermes:run-a",
      "Repeated run · augure-desktop:run-a",
    ]);
    await unmount(component);
  });

  it("hides the project from the zoom breadcrumb", async () => {
    usageServiceMocks.getApiV1UsageTopSessions.mockResolvedValue([]);
    usageServiceMocks.getApiV1UsageSummary.mockResolvedValue(summaryWithDuplicateProjectLabels());
    const component = mountPanel();
    await tick();
    document
      .querySelectorAll<HTMLElement>(".list-row")[0]!
      .dispatchEvent(new MouseEvent("dblclick", { bubbles: true }));
    await tick();
    const hide = [...document.querySelectorAll<HTMLButtonElement>("button")].find(
      (button) => button.textContent?.trim() === "Hide project",
    )!;
    hide.click();
    expect(usage.zoomedProject).toBeNull();
    expect(usage.excludedProjectKeys).toBe("pl1:sha256:first");
    usage.zoomedProject = { key: "pl1:sha256:first", label: "hermes-cron" };
    await tick();
    [...document.querySelectorAll<HTMLButtonElement>("button")]
      .find((button) => button.textContent?.trim() === "Hide project")!
      .click();
    expect(usage.zoomedProject).toBeNull();
    expect(usage.excludedProjectKeys).toBe("pl1:sha256:first");
    await unmount(component);
  });

  it("clears zoom when switching attribution dimensions", async () => {
    usage.zoomedProject = { key: "pl1:sha256:first", label: "hermes-cron" };
    const component = mountPanel();
    await tick();
    [...document.querySelectorAll<HTMLButtonElement>("button")]
      .find((button) => button.textContent?.trim() === "Agent")!
      .click();
    expect(usage.zoomedProject).toBeNull();
    unmount(component);
  });
  it("keeps zoom when the project leaves the refreshed summary", async () => {
    usage.zoomedProject = { key: "pl1:sha256:first", label: "hermes-cron" };
    usage.zoomRows = [];
    usageServiceMocks.getApiV1UsageSummary.mockResolvedValue(summaryWithAgents([]));
    usageServiceMocks.getApiV1UsageTopSessions.mockResolvedValue([]);
    await usage.fetchAll({ preserveTimeRange: true });
    const component = mountPanel();
    await tick();
    expect(document.querySelector(".panel-header")?.textContent).toContain("hermes-cron");
    expect(document.querySelector(".empty")?.textContent).toBe("No data for this period");
    unmount(component);
  });
});
