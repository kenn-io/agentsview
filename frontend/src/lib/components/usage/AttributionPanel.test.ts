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
import { sessions } from "../../stores/sessions.svelte.js";
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
  usage.mergeKnownProjects((usage.attributionSummary ?? usage.summary)?.projectTotals ?? [], {});
  return mount(AttributionPanel, {
    target: document.body,
    props: {
      colorMap: colorMap ?? usageChartColorMaps(usage.summary, settings.chartPalette)[groupBy],
    },
  });
}

beforeEach(() => {
  vi.clearAllMocks();
  usage.selectedProjectKey = "";
  usage.selectedModel = "";
  usage.knownProjects = [];
  usage.attributionSummary = null;
  usage.backToProjects();
  usage.excludedProjectKeys = "";
  usage.excludedAgents = "";
  usage.excludedModels = "";
  usageServiceMocks.getApiV1UsageTopSessions.mockResolvedValue([]);
});

afterEach(() => {
  usage.backToProjects();
  usage.selectedProjectKey = "";
  usage.selectedModel = "";
  usage.attributionSummary = null;
  sessions.filters.agent = "";
});

describe("AttributionPanel selection", () => {
  it("reveals Clear selection in the actions and clears the row", async () => {
    const full = summaryWithModels();
    usage.summary = full;
    usageServiceMocks.getApiV1UsageSummary.mockResolvedValue(full);
    usage.toggles.attribution.groupBy = "model";
    usage.toggles.attribution.view = "list";
    const component = mountPanel();
    await tick();
    const actions = document.querySelector<HTMLElement>(".selection-actions")!;
    expect(actions.classList.contains("inactive")).toBe(true);
    document.querySelector<HTMLElement>(".list-row")!.click();
    await tick();
    expect(actions.classList.contains("inactive")).toBe(false);
    [...actions.querySelectorAll<HTMLButtonElement>("button")].find((button) => button.textContent?.trim() === "Clear selection")!.click();
    await tick();
    expect(actions.classList.contains("inactive")).toBe(true);
    expect(document.querySelector('.list-row[aria-pressed="true"]')).toBeNull();
    await unmount(component);
    usage.cancelInFlightReads();
  });

  it("highlights both agents picked in the header", async () => {
    usage.summary = summaryWithAgents(["claude", "codex"]);
    sessions.filters.agent = "claude,codex";
    usage.toggles.attribution.groupBy = "agent";
    usage.toggles.attribution.view = "list";
    const component = mountPanel();
    await tick();
    expect(document.querySelectorAll('.list-row[aria-pressed="true"]')).toHaveLength(2);
    expect(document.querySelectorAll(".dimmed")).toHaveLength(0);
    await unmount(component);
  });

  it.each([
    ["treemap", ".tile"],
    ["treemap", ".rail-row"],
    ["list", ".list-row"],
  ] as const)("keeps every item and color through %s %s", async (view, selector) => {
    const full = summaryWithDuplicateProjectLabels();
    full.projectTotals[0]!.project = "Project A";
    full.projectTotals[1]!.project = "Project B";
    const narrowed = structuredClone(full);
    narrowed.projectTotals = [narrowed.projectTotals[1]!];
    usage.summary = full;
    usageServiceMocks.getApiV1UsageSummary.mockImplementation(async (params) => params.project_key ? narrowed : full);
    usage.toggles.attribution.groupBy = "project";
    usage.toggles.attribution.view = view;
    const component = mountPanel();
    await tick();
    const colors = Array.from(document.querySelectorAll(selector), (row) =>
      row.querySelector("rect")?.getAttribute("fill") ?? row.querySelector(".rail-dot, .list-dot")?.getAttribute("style"),
    );
    document.querySelectorAll(selector)[1]!.dispatchEvent(new MouseEvent("click", { detail: 1, bubbles: true }));
    await vi.waitFor(() => expect(usage.attributionSummary).toEqual(full));
    await tick();
    const rows = document.querySelectorAll(selector);
    expect(rows).toHaveLength(2);
    expect(rows[1]!.getAttribute("aria-pressed")).toBe("true");
    expect(rows[0]!.classList.contains("dimmed")).toBe(true);
    expect(Array.from(rows, (row) => row.querySelector("rect")?.getAttribute("fill") ?? row.querySelector(".rail-dot, .list-dot")?.getAttribute("style"))).toEqual(colors);
    if (selector !== ".tile") {
      expect(usage.zoomedProject).toBeNull();
      rows[1]!.dispatchEvent(new MouseEvent("click", { detail: 2, bubbles: true }));
      rows[1]!.dispatchEvent(new MouseEvent("dblclick", { detail: 2, bubbles: true }));
      expect(usage.zoomedProject).toEqual({ key: "pl1:sha256:second", label: "Project B" });
      expect(usageServiceMocks.getApiV1UsageTopSessions.mock.lastCall?.[0]).toEqual(expect.objectContaining({ project_key: "pl1:sha256:second", group_by: "group" }));
    }
    await unmount(component);
  });

  it.each(["model", "agent"] as const)("Enter selects a %s row and a quick second click clears it", async (by) => {
    const full = by === "model" ? summaryWithModels() : summaryWithAgents(["claude", "codex"]);
    usage.summary = full;
    usageServiceMocks.getApiV1UsageSummary.mockResolvedValue(full);
    usage.toggles.attribution.groupBy = by;
    usage.toggles.attribution.view = "list";
    const component = mountPanel();
    await tick();
    const row = document.querySelectorAll(".list-row")[1]!;
    row.dispatchEvent(new KeyboardEvent("keydown", { key: "Enter", bubbles: true, cancelable: true }));
    if (by === "agent") await usage.fetchAll({ preserveTimeRange: true });
    await vi.waitFor(() => expect(usage.attributionSummary).toEqual(full));
    await tick();
    expect(row.getAttribute("aria-pressed")).toBe("true");
    expect(document.querySelector(".list-row.dimmed")).not.toBeNull();
    expect(document.querySelectorAll(".list-row")).toHaveLength(2);
    expect(usage.zoomedProject).toBeNull();
    expect([...document.querySelectorAll("button")].some((button) => button.textContent?.trim() === "Open")).toBe(false);
    row.dispatchEvent(new MouseEvent("click", { detail: 2, bubbles: true }));
    await tick();
    expect(row.getAttribute("aria-pressed")).toBe("false");
    await unmount(component);
  });

  it("keeps every row during the request that clears focus", async () => {
    const full = summaryWithDuplicateProjectLabels();
    const narrowed = structuredClone(full);
    narrowed.projectTotals = [narrowed.projectTotals[0]!];
    usage.selectedProjectKey = "pl1:sha256:first";
    usage.summary = narrowed;
    usage.attributionSummary = full;
    usageServiceMocks.getApiV1UsageSummary.mockImplementationOnce(() => new Promise(() => {}));
    usage.toggles.attribution.groupBy = "project";
    usage.toggles.attribution.view = "list";
    const component = mountPanel();
    await tick();
    const rows = document.querySelectorAll(".list-row");
    rows[0]!.dispatchEvent(new MouseEvent("click", { detail: 1, bubbles: true }));
    await tick();
    expect(document.querySelectorAll(".list-row")).toHaveLength(2);
    expect(rows[0]!.getAttribute("aria-pressed")).toBe("false");
    expect(document.querySelectorAll(".dimmed")).toHaveLength(0);
    await unmount(component);
    usage.cancelInFlightReads();
  });

  it.each(["Enter", " "])("%s selects a project and Open opens it", async (key) => {
    const full = summaryWithDuplicateProjectLabels();
    full.projectTotals[1]!.project = "Project B";
    usage.summary = full;
    usageServiceMocks.getApiV1UsageSummary.mockResolvedValue(full);
    usage.toggles.attribution.groupBy = "project";
    usage.toggles.attribution.view = "list";
    const component = mountPanel();
    await tick();
    const openButton = () => [...document.querySelectorAll<HTMLButtonElement>(".selection-actions:not(.inactive) span:not(.inactive) button")].find((button) => button.textContent?.trim() === "Open");
    expect(openButton()).toBeUndefined();
    const row = document.querySelectorAll(".list-row")[1]!;
    row.dispatchEvent(new KeyboardEvent("keydown", { key, bubbles: true, cancelable: true }));
    await tick();
    expect(row.getAttribute("aria-pressed")).toBe("true");
    expect(usage.zoomedProject).toBeNull();
    expect(openButton()).toBeDefined();
    await vi.waitFor(() => expect(usage.attributionSummary).toEqual(full));
    const narrowed = { ...full, projectTotals: [full.projectTotals[0]!] };
    usage.attributionSummary = narrowed;
    await tick();
    expect(usage.selectedProjectKey).toBe("pl1:sha256:second");
    expect(openButton()).toBeUndefined();
    usage.attributionSummary = full;
    await tick();
    expect(openButton()).toBeDefined();
    openButton()!.click();
    await tick();
    expect(usage.zoomedProject).toEqual({ key: "pl1:sha256:second", label: "Project B" });
    expect(openButton()).toBeUndefined();
    await unmount(component);
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
    usage.backToProjects();
    usage.zoomRows = null;
    usage.summary = summaryWithDuplicateProjectLabels();
    usage.summary.projectTotals[0]!.project = "hermes-cron";
    usage.mergeKnownProjects(usage.summary.projectTotals, {});
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
      "Other · remainde",
      "Repeated run · hermes:",
      "Repeated run · hermes:r",
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
    usage.setOpenProject("pl1:sha256:first");
    await vi.waitFor(() => expect(usage.loading.zoom).toBe(false));
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
    usage.setOpenProject("pl1:sha256:first");
    await vi.waitFor(() => expect(usage.loading.zoom).toBe(false));
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

  it("hides the home hash in tooltips and shows it only for the same job in two homes", async () => {
    usage.setOpenProject("pl1:sha256:first");
    await vi.waitFor(() => expect(usage.loading.zoom).toBe(false));
    usage.zoomRows = [["job-1:0a1b2c3d", "Digest"], ["job-1:4e5f6a7b", "Digest"], ["job-2:0a1b2c3d", "Backup"]].map(([groupKey, groupLabel]) => ({
      ...topSessionForRemainder(), groupKey, groupLabel,
    }));
    const component = mountPanel();
    await tick();
    const rows = document.querySelectorAll<HTMLElement>(".list-row");
    expect(Array.from(rows, (row) => row.querySelector(".list-label")!.textContent)).toEqual(["Digest · 0a1b2c3d", "Digest · 4e5f6a7b", "Backup"]);
    expect(Array.from(rows, (row) => row.title)).toEqual(["Digest · job-1", "Digest · job-1", "Backup · job-2"]);
    await unmount(component);
  });

  it.each(["list", "treemap"] as const)("distinguishes two same-named jobs on two machines in %s", async (view) => {
    usage.setOpenProject("pl1:sha256:first");
    await vi.waitFor(() => expect(usage.loading.zoom).toBe(false));
    usage.toggles.attribution.view = view;
    usage.zoomRows = ["a1b2c3d4-job", "e5f6a7b8-job", "digest"].flatMap((groupKey) => ["host-a", "host-b"].map((machine) => ({
      ...topSessionForRemainder(), groupKey, machine, groupLabel: groupKey === "digest" ? "Daily digest" : "Digest",
    })));
    const component = mountPanel();
    await tick();
    const rows = document.querySelectorAll<HTMLElement>(view === "list" ? ".list-row" : ".rail-row");
    expect(Array.from(rows, (row) => row.querySelector(".list-label, .rail-label")!.textContent)).toEqual([
      "Digest · a1b2c3d4 · host-a",
      "Digest · a1b2c3d4 · host-b",
      "Digest · e5f6a7b8 · host-a",
      "Digest · e5f6a7b8 · host-b",
      "Daily digest · host-a",
      "Daily digest · host-b",
    ]);
    expect(Array.from(rows, (row) => row.title)).toEqual([
      "Digest · a1b2c3d4-job · host-a",
      "Digest · a1b2c3d4-job · host-b",
      "Digest · e5f6a7b8-job · host-a",
      "Digest · e5f6a7b8-job · host-b",
      "Daily digest · digest · host-a",
      "Daily digest · digest · host-b",
    ]);
    for (const row of document.querySelectorAll(".list-row, .rail-row, .tile")) {
      expect(row.hasAttribute("role")).toBe(false);
      expect(row.hasAttribute("tabindex")).toBe(false);
      expect(row.hasAttribute("aria-pressed")).toBe(false);
    }
    rows[0]!.click();
    expect(usage.zoomedProject?.key).toBe("pl1:sha256:first");
    expect(usage.selectedProjectKey).toBe("pl1:sha256:first");
    await unmount(component);
  });

  it("shortens full IDs for same-named ungrouped sessions", async () => {
    usage.setOpenProject("pl1:sha256:first");
    await vi.waitFor(() => expect(usage.loading.zoom).toBe(false));
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
    await unmount(component);
  });

  it("clears zoom when switching attribution dimensions", async () => {
    usage.setOpenProject("pl1:sha256:first");
    await vi.waitFor(() => expect(usage.loading.zoom).toBe(false));
    const component = mountPanel();
    await tick();
    [...document.querySelectorAll<HTMLButtonElement>("button")]
      .find((button) => button.textContent?.trim() === "Agent")!
      .click();
    expect(usage.zoomedProject).toBeNull();
    unmount(component);
  });
  it("keeps zoom when the project leaves the refreshed summary", async () => {
    usage.setOpenProject("pl1:sha256:first");
    await vi.waitFor(() => expect(usage.loading.zoom).toBe(false));
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
