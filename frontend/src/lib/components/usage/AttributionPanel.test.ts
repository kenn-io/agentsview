import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { mount, tick, unmount } from "svelte";
import type { UsageSummaryResponse } from "../../api/types/usage.js";

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
import { usage } from "../../stores/usage.svelte.js";
import { branchFilterToken } from "../../branchFilters.js";

function summaryWithAgents(agents: string[]): UsageSummaryResponse {
  return {
    from: "2024-01-01",
    to: "2024-01-31",
    totals: {
      inputTokens: 100,
      outputTokens: 50,
      cacheCreationTokens: 0,
      cacheReadTokens: 0,
      totalCost: 12,
    },
    daily: [],
    projectTotals: [],
    modelTotals: [],
    branchTotals: [],
    agentTotals: agents.map((agent, i) => ({
      agent,
      inputTokens: 60 - i * 20,
      outputTokens: 30 - i * 10,
      cacheCreationTokens: 0,
      cacheReadTokens: 0,
      cost: 8 - i * 4,
    })),
    sessionCounts: { total: 2, byProject: {}, byAgent: {} },
    cacheStats: {
      cacheReadTokens: 0,
      cacheCreationTokens: 0,
      uncachedInputTokens: 100,
      outputTokens: 50,
      hitRate: 0,
      savingsVsUncached: 0,
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
      cost: 8,
    },
    {
      project_key: "pl1:sha256:second",
      project: "",
      inputTokens: 40,
      outputTokens: 20,
      cacheCreationTokens: 0,
      cacheReadTokens: 0,
      cost: 4,
    },
  ];
  return summary;
}

function summaryWithModels(): UsageSummaryResponse {
  const summary = summaryWithAgents([]);
  summary.modelTotals = [
    {
      model: "claude-sonnet-5",
      inputTokens: 60,
      outputTokens: 30,
      cacheCreationTokens: 0,
      cacheReadTokens: 0,
      cost: 8,
    },
    {
      model: "claude-opus-4-8",
      inputTokens: 40,
      outputTokens: 20,
      cacheCreationTokens: 0,
      cacheReadTokens: 0,
      cost: 4,
    },
  ];
  return summary;
}

describe("AttributionPanel agent exclusion", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    usage.summary = summaryWithAgents(["claude", "codex"]);
    usage.excludedAgents = "";
    usage.toggles.attribution.groupBy = "agent";
    usage.toggles.attribution.view = "list";
  });

  afterEach(() => {
    usage.summary = null;
    usage.excludedAgents = "";
    usage.toggles.attribution.groupBy = "project";
    document.body.innerHTML = "";
  });

  // Drives the real click path: panel click -> store toggle -> outgoing
  // request. Fails without the baseParams excludeAgent wiring.
  it("sends agent exclusions in usage queries after an attribution click", async () => {
    const component = mount(AttributionPanel, { target: document.body });
    await tick();

    const rows = document.querySelectorAll<HTMLElement>(".list-row");
    expect(rows.length).toBe(2);
    rows[1]!.click(); // exclude "codex"

    await vi.waitFor(() =>
      expect(
        usageServiceMocks.getApiV1UsageSummary,
      ).toHaveBeenLastCalledWith(
        expect.objectContaining({ excludeAgent: "codex" }),
      ),
    );
    unmount(component);
  });

  // Agent clicks exclude the clicked agent, so the hide copy stays.
  it("describes agent rows as hide actions", async () => {
    const component = mount(AttributionPanel, { target: document.body });
    await tick();

    expect(
      document.querySelector(".hint")?.textContent?.trim(),
    ).toBe("Click to hide from chart");
    const rows = document.querySelectorAll<HTMLElement>(".list-row");
    expect(rows[0]?.getAttribute("title")).toBe("Click to hide claude");
    unmount(component);
  });
});

function usageSummary(): UsageSummaryResponse {
  return {
    from: "2024-01-01",
    to: "2024-01-31",
    totals: {
      inputTokens: 100,
      outputTokens: 50,
      cacheCreationTokens: 0,
      cacheReadTokens: 0,
      totalCost: 12,
    },
    daily: [],
    projectTotals: [],
    modelTotals: [],
    agentTotals: [],
    branchTotals: [
      {
        project: "alpha",
        branch: "main",
        inputTokens: 60,
        outputTokens: 30,
        cacheCreationTokens: 0,
        cacheReadTokens: 0,
        cost: 8,
      },
      {
        project: "alpha",
        branch: "dev",
        inputTokens: 40,
        outputTokens: 20,
        cacheCreationTokens: 0,
        cacheReadTokens: 0,
        cost: 4,
      },
    ],
    sessionCounts: {
      total: 2,
      byProject: { alpha: 2 },
      byAgent: {},
    },
    cacheStats: {
      cacheReadTokens: 0,
      cacheCreationTokens: 0,
      uncachedInputTokens: 100,
      outputTokens: 50,
      hitRate: 0,
      savingsVsUncached: 0,
    },
  };
}

describe("AttributionPanel project identity", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    usage.summary = summaryWithDuplicateProjectLabels();
    usage.excludedProjectKeys = "";
    usage.toggles.attribution.groupBy = "project";
    usage.toggles.attribution.view = "list";
  });

  afterEach(() => {
    usage.summary = null;
    usage.excludedProjectKeys = "";
    document.body.innerHTML = "";
  });

  it("keeps duplicate display labels distinct and filters by project key", async () => {
    const component = mount(AttributionPanel, { target: document.body });
    await tick();

    const rows = document.querySelectorAll<HTMLElement>(".list-row");
    expect(rows.length).toBe(2);
    rows[1]!.click();

    await vi.waitFor(() =>
      expect(
        usageServiceMocks.getApiV1UsageSummary,
      ).toHaveBeenLastCalledWith(
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
    usage.toggles.attribution.groupBy = "project";
    usage.toggles.attribution.view = "list";
    document.body.innerHTML = "";
  });

  it("keeps colliding model rows distinct", async () => {
    usage.summary = summaryWithModels();
    usage.toggles.attribution.groupBy = "model";
    usage.toggles.attribution.view = "list";

    const component = mount(AttributionPanel, { target: document.body });
    await tick();

    const colors = Array.from(
      document.querySelectorAll<HTMLElement>(".list-dot"),
    ).map((dot) => dot.getAttribute("style"));
    expect(new Set(colors).size).toBe(2);
    unmount(component);
  });

  it("routes distinct model colors through the treemap and rail", async () => {
    usage.summary = summaryWithModels();
    usage.toggles.attribution.groupBy = "model";
    usage.toggles.attribution.view = "treemap";

    const component = mount(AttributionPanel, { target: document.body });
    await tick();

    const tileColors = Array.from(
      document.querySelectorAll<SVGRectElement>(".tile rect"),
    ).map((tile) => tile.getAttribute("fill"));
    const railColors = Array.from(
      document.querySelectorAll<HTMLElement>(".rail-dot"),
    ).map((dot) => dot.style.background);
    expect(new Set(tileColors).size).toBe(2);
    expect(railColors).toEqual(tileColors);
    unmount(component);
  });
});

describe("AttributionPanel branch mode", () => {
  beforeEach(() => {
    usage.summary = usageSummary();
    usage.toggles.attribution.groupBy = "branch";
    usage.toggles.attribution.view = "list";
  });

  afterEach(() => {
    usage.summary = null;
    usage.toggles.attribution.groupBy = "project";
    usage.selectedGitBranch = "";
    document.body.innerHTML = "";
    vi.restoreAllMocks();
  });

  it("routes a branch row click into the branch exclusion toggle", async () => {
    const spy = vi
      .spyOn(usage, "toggleBranch")
      .mockImplementation(() => {});
    const component = mount(AttributionPanel, { target: document.body });
    await tick();

    const row = Array.from(
      document.querySelectorAll<HTMLDivElement>(".list-row"),
    ).find((r) => r.textContent?.includes("alpha/dev"));
    expect(row).toBeTruthy();
    row?.click();
    await tick();

    expect(spy).toHaveBeenCalledWith(branchFilterToken("alpha", "dev"));
    unmount(component);
  });

  // Branch clicks filter the dashboard to the clicked branch (include
  // semantics), so the copy must say "filter", not "hide".
  it("describes branch rows as filter actions", async () => {
    const component = mount(AttributionPanel, { target: document.body });
    await tick();

    expect(
      document.querySelector(".hint")?.textContent?.trim(),
    ).toBe("Click to filter the chart");

    const row = Array.from(
      document.querySelectorAll<HTMLDivElement>(".list-row"),
    ).find((r) => r.textContent?.includes("alpha/dev"));
    expect(row?.getAttribute("title")).toBe(
      "Click to filter by alpha/dev",
    );
    unmount(component);
  });

  it("describes a selected branch row as clearing its filter", async () => {
    usage.selectedGitBranch = branchFilterToken("alpha", "dev");
    const component = mount(AttributionPanel, { target: document.body });
    await tick();

    const row = Array.from(
      document.querySelectorAll<HTMLDivElement>(".list-row"),
    ).find((r) => r.textContent?.includes("alpha/dev"));
    expect(row?.getAttribute("title")).toBe(
      "Click to clear the alpha/dev filter",
    );
    unmount(component);
  });
});
