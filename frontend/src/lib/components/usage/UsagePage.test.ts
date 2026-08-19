import {
  afterEach,
  describe,
  expect,
  it,
  vi,
} from "vite-plus/test";
import { mount, tick, unmount } from "svelte";
import { router } from "../../stores/router.svelte.js";
import { sessions } from "../../stores/sessions.svelte.js";
import { usage } from "../../stores/usage.svelte.js";
import { settings } from "../../stores/settings.svelte.js";
import { yokedDates } from "../../stores/yokedDates.svelte.js";
import { testMoney } from "../../test/money.js";
import type { UsageSummaryResponse } from "../../api/types/usage.js";
import source from "./UsagePage.svelte?raw";
import UsagePage from "./UsagePage.svelte";

async function flushEffects() {
  await tick();
  await Promise.resolve();
  await tick();
}

let component: ReturnType<typeof mount> | undefined;

function usageSummaryWithUnsupported(kind?: string): UsageSummaryResponse {
  return {
    from: "2024-06-01",
    to: "2024-06-01",
    totals: {
      inputTokens: 0,
      outputTokens: 0,
      cacheCreationTokens: 0,
      cacheReadTokens: 0,
      totalCost: testMoney(0),
    },
    daily: [],
    projectTotals: [],
    modelTotals: [],
    agentTotals: [],
    sessionCounts: {
      total: 0,
      byProject: {},
      byAgent: {},
    },
    cacheStats: {
      cacheReadTokens: 0,
      cacheCreationTokens: 0,
      uncachedInputTokens: 0,
      outputTokens: 0,
      hitRate: 0,
      savingsVsUncached: testMoney(0),
    },
    ...(kind ? { unsupportedUsage: { kind } } : {}),
  };
}

function tenModelUsageSummary(): UsageSummaryResponse {
  const models = [
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
  ];
  return {
    ...usageSummaryWithUnsupported(),
    totals: {
      inputTokens: 100,
      outputTokens: 50,
      cacheCreationTokens: 0,
      cacheReadTokens: 0,
      totalCost: testMoney(55),
    },
    daily: [{
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
    }],
    modelTotals: models.map((model, index) => ({
      model,
      inputTokens: 10,
      outputTokens: 5,
      cacheCreationTokens: 0,
      cacheReadTokens: 0,
      cost: testMoney(index + 1),
    })),
    sessionCounts: { total: 10, byProject: {}, byAgent: {} },
  };
}

afterEach(() => {
  if (component) {
    unmount(component);
    component = undefined;
  }
  vi.restoreAllMocks();
  vi.useRealTimers();
  vi.unstubAllGlobals();
  document.body.innerHTML = "";
  router.route = "sessions";
  router.params = {};
  router.sessionId = null;
  window.history.replaceState(null, "", "/");
  usage.summary = null;
  usage.topSessions = null;
  usage.errors.summary = null;
  usage.errors.topSessions = null;
  usage.mode = "cost";
  usage.setSelectedTokenTypes([
    "input",
    "cache_write",
    "cache_read",
    "output",
  ]);
  usage.isPinned = false;
  usage.windowDays = 30;
  usage.from = "";
  usage.to = "";
  usage.toggles.timeSeries.groupBy = "project";
  usage.toggles.attribution.groupBy = "project";
  usage.toggles.attribution.view = "treemap";
  usage.excludedProjects = "";
  usage.excludedProjectKeys = "";
  usage.knownProjects = [];
  settings.chartPalette = "agentsview";
  sessions.projects = [];
  yokedDates.setEnabled(false);
  localStorage.clear();
});

describe("UsagePage refresh behavior", () => {
  it("uses stable project keys in the Project filter", async () => {
    vi.stubGlobal(
      "ResizeObserver",
      class {
        observe() {}
        disconnect() {}
      },
    );
    vi.spyOn(usage, "fetchAll").mockResolvedValue();
    vi.spyOn(sessions, "loadAgents").mockResolvedValue();
    router.route = "usage";
    router.params = {};
    const summary = usageSummaryWithUnsupported();
    summary.projectTotals = [
      {
        project_key: "project-key-1",
        project: "shared-label",
        inputTokens: 80,
        outputTokens: 40,
        cacheCreationTokens: 0,
        cacheReadTokens: 0,
        cost: testMoney(8),
      },
      {
        project_key: "project-key-2",
        project: "shared-label",
        inputTokens: 20,
        outputTokens: 10,
        cacheCreationTokens: 0,
        cacheReadTokens: 0,
        cost: testMoney(2),
      },
    ];
    usage.summary = summary;

    component = mount(UsagePage, { target: document.body });
    await flushEffects();

    const projectFilter = document.querySelector<HTMLButtonElement>(
      '.kit-filter-dropdown__btn[aria-label="Project: All"]',
    );
    expect(projectFilter).not.toBeNull();

    projectFilter!.click();
    await tick();
    const projectOptions = document.querySelectorAll<HTMLButtonElement>(
      ".kit-filter-dropdown__item",
    );
    expect(projectOptions).toHaveLength(2);
    expect(projectOptions[0]?.textContent).toContain("shared-label");
    expect(projectOptions[1]?.textContent).toContain("shared-label");

    projectOptions[1]!.click();
    expect(usage.excludedProjectKeys).toBe("project-key-2");
    expect(usage.excludedProjects).toBe("");

    usage.summary = {
      ...summary,
      projectTotals: [summary.projectTotals[0]!],
    };
    await unmount(component);
    component = mount(UsagePage, { target: document.body });
    await flushEffects();

    const remountedProjectFilter = document.querySelector<HTMLButtonElement>(
      ".usage-toolbar .kit-filter-dropdown__btn",
    );
    expect(remountedProjectFilter).not.toBeNull();
    remountedProjectFilter!.click();
    await tick();
    expect(
      document.querySelectorAll(".kit-filter-dropdown__item"),
    ).toHaveLength(2);
    const remountedOptions = document.querySelectorAll<HTMLButtonElement>(
      ".kit-filter-dropdown__item",
    );
    remountedOptions[1]!.click();
    expect(usage.excludedProjectKeys).toBe("");

    usage.excludedProjectKeys = "unlisted-project-key";

    const deselectAll = Array.from(
      document.querySelectorAll<HTMLButtonElement>(
        ".kit-filter-dropdown__bulk-btn",
      ),
    ).find((button) => button.textContent?.trim() === "Deselect all");
    expect(deselectAll).not.toBeUndefined();

    deselectAll!.click();
    expect(usage.excludedProjectKeys).toBe(
      "unlisted-project-key,project-key-1,project-key-2",
    );

    const selectAll = Array.from(
      document.querySelectorAll<HTMLButtonElement>(
        ".kit-filter-dropdown__bulk-btn",
      ),
    ).find((button) => button.textContent?.trim() === "Select all");
    expect(selectAll).not.toBeUndefined();

    selectAll!.click();
    expect(usage.excludedProjectKeys).toBe("");
  });

  it("shows legacy project-name exclusions until they are cleared", async () => {
    vi.stubGlobal(
      "ResizeObserver",
      class {
        observe() {}
        disconnect() {}
      },
    );
    vi.spyOn(usage, "fetchAll").mockResolvedValue();
    vi.spyOn(sessions, "loadAgents").mockResolvedValue();
    router.route = "usage";
    router.params = {};
    usage.excludedProjects = "legacy-project";
    usage.summary = usageSummaryWithUnsupported();

    component = mount(UsagePage, { target: document.body });
    await flushEffects();

    const projectFilter = document.querySelector<HTMLButtonElement>(
      '.kit-filter-dropdown__btn[aria-label="Project: 1 hidden"]',
    );
    expect(projectFilter).not.toBeNull();

    projectFilter!.click();
    await tick();
    const selectAll = Array.from(
      document.querySelectorAll<HTMLButtonElement>(
        ".kit-filter-dropdown__bulk-btn",
      ),
    ).find((button) => button.textContent?.trim() === "Select all");
    expect(selectAll).not.toBeUndefined();

    selectAll!.click();
    expect(usage.excludedProjects).toBe("");
  });

  it("hydrates token mode from the canonical URL before fetching", async () => {
    vi.stubGlobal(
      "ResizeObserver",
      class {
        observe() {}
        disconnect() {}
      },
    );
    const fetchAll = vi.spyOn(usage, "fetchAll").mockResolvedValue();
    vi.spyOn(sessions, "loadAgents").mockResolvedValue();
    router.route = "usage";
    router.params = { view: "tokens", project: "demo" };

    component = mount(UsagePage, { target: document.body });
    await flushEffects();

    expect(usage.mode).toBe("token");
    expect(document.querySelector(
      '[role="radiogroup"][aria-label="Usage metric"] '
        + '[role="radio"][aria-checked="true"]',
    )?.textContent?.trim()).toBe("Tokens");
    expect(fetchAll).toHaveBeenCalled();
  });

  it("hydrates and renders an Output-only token selection", async () => {
    vi.stubGlobal(
      "ResizeObserver",
      class {
        observe() {}
        disconnect() {}
      },
    );
    vi.spyOn(usage, "fetchAll").mockResolvedValue();
    vi.spyOn(sessions, "loadAgents").mockResolvedValue();
    router.route = "usage";
    router.params = {
      view: "tokens",
      token_types: "output",
      project: "demo",
    };

    component = mount(UsagePage, { target: document.body });
    await flushEffects();

    expect(usage.selectedTokenTypes).toEqual(["output"]);
    expect(document.querySelector(
      'button[aria-label="Token types: Output"]',
    )).not.toBeNull();
    expect(router.params).toEqual(expect.objectContaining({
      view: "tokens",
      token_types: "output",
      project: "demo",
    }));
  });

  it("switches metrics without dropping filters", async () => {
    vi.stubGlobal(
      "ResizeObserver",
      class {
        observe() {}
        disconnect() {}
      },
    );
    vi.spyOn(usage, "fetchAll").mockResolvedValue();
    vi.spyOn(sessions, "loadAgents").mockResolvedValue();
    const fetchTopSessions = vi.spyOn(usage, "fetchTopSessions")
      .mockResolvedValue("ok");
    const replaceParams = vi.spyOn(router, "replaceParams");
    window.history.replaceState(
      null,
      "",
      "/usage?view=tokens&project=demo&window_days=90",
    );
    router.route = "usage";
    router.params = {
      view: "tokens",
      project: "demo",
      window_days: "90",
    };
    usage.mode = "token";

    component = mount(UsagePage, { target: document.body });
    await flushEffects();
    const costOption = document.querySelector<HTMLButtonElement>(
      '[role="radiogroup"][aria-label="Usage metric"] '
        + '[role="radio"]:first-child',
    );
    expect(costOption).not.toBeNull();

    costOption!.click();
    await flushEffects();

    expect(usage.mode).toBe("cost");
    expect(replaceParams).toHaveBeenLastCalledWith(
      expect.objectContaining({
        project: "demo",
        window_days: "90",
      }),
    );
    expect(replaceParams.mock.lastCall?.[0]).not.toHaveProperty("view");
    expect(fetchTopSessions).toHaveBeenCalledOnce();
  });

  it("normalizes the legacy token route while retaining filters", async () => {
    vi.stubGlobal(
      "ResizeObserver",
      class {
        observe() {}
        disconnect() {}
      },
    );
    vi.spyOn(usage, "fetchAll").mockResolvedValue();
    vi.spyOn(sessions, "loadAgents").mockResolvedValue();
    const replace = vi.spyOn(router, "replace");
    window.history.replaceState(
      null,
      "",
      "/token-usage?project=demo&window_days=90",
    );
    router.route = "token-usage";
    router.params = {
      project: "demo",
      window_days: "90",
    };

    component = mount(UsagePage, { target: document.body });
    await flushEffects();

    expect(replace).toHaveBeenCalledWith("usage", {
      project: "demo",
      window_days: "90",
      view: "tokens",
    });
    expect(router.route).toBe("usage");
    expect(usage.mode).toBe("token");
  });

  it("removes an unsupported metric value from the canonical URL", async () => {
    vi.stubGlobal(
      "ResizeObserver",
      class {
        observe() {}
        disconnect() {}
      },
    );
    vi.spyOn(usage, "fetchAll").mockResolvedValue();
    vi.spyOn(sessions, "loadAgents").mockResolvedValue();
    const replaceParams = vi.spyOn(router, "replaceParams");
    router.route = "usage";
    router.params = { view: "unknown", project: "demo" };

    component = mount(UsagePage, { target: document.body });
    await flushEffects();

    expect(usage.mode).toBe("cost");
    expect(replaceParams).toHaveBeenCalled();
    expect(replaceParams.mock.lastCall?.[0]).not.toHaveProperty("view");
  });

  it("materializes rolling bounds before fetching a returned bare page", async () => {
    const fetchStates: Array<{
      isPinned: boolean;
      from: string;
      to: string;
    }> = [];
    vi.useFakeTimers({ toFake: ["Date"] });
    vi.setSystemTime(new Date("2026-07-10T12:00:00"));
    vi.stubGlobal(
      "ResizeObserver",
      class {
        observe() {}
        disconnect() {}
      },
    );
    vi.spyOn(usage, "fetchAll").mockImplementation(() => {
      fetchStates.push({
        isPinned: usage.isPinned,
        from: usage.from,
        to: usage.to,
      });
      return Promise.resolve();
    });
    vi.spyOn(sessions, "loadAgents").mockResolvedValue();
    router.route = "usage";
    router.params = {};
    usage.isPinned = true;
    usage.windowDays = 30;
    usage.from = "2026-01-01";
    usage.to = "2026-01-07";
    yokedDates.setEnabled(false);

    component = mount(UsagePage, { target: document.body });
    await flushEffects();

    expect(usage.isPinned).toBe(false);
    expect(usage.from).toBe("2026-06-11");
    expect(usage.to).toBe("2026-07-10");
    expect(fetchStates[0]).toEqual({
      isPinned: false,
      from: "2026-06-11",
      to: "2026-07-10",
    });
  });

  it("refreshes an unpinned rolling range after midnight", async () => {
    const fetchDates: Array<{ from: string; to: string }> = [];
    vi.useFakeTimers({ toFake: ["Date"] });
    vi.setSystemTime(new Date("2026-07-10T12:00:00"));
    vi.stubGlobal(
      "ResizeObserver",
      class {
        observe() {}
        disconnect() {}
      },
    );
    vi.spyOn(usage, "fetchAll").mockImplementation(() => {
      fetchDates.push({ from: usage.from, to: usage.to });
      return Promise.resolve();
    });
    vi.spyOn(sessions, "loadAgents").mockResolvedValue();
    router.route = "usage";
    router.params = {};
    usage.isPinned = false;
    usage.windowDays = 30;
    usage.from = "2026-06-10";
    usage.to = "2026-07-09";
    yokedDates.setEnabled(false);

    component = mount(UsagePage, { target: document.body });
    await flushEffects();

    expect(usage.isPinned).toBe(false);
    expect(usage.from).toBe("2026-06-11");
    expect(usage.to).toBe("2026-07-10");
    expect(fetchDates[0]).toEqual({
      from: "2026-06-11",
      to: "2026-07-10",
    });
  });

  it("renders the unsupported Copilot note from the summary contract", async () => {
    vi.stubGlobal(
      "ResizeObserver",
      class {
        observe() {}
        disconnect() {}
      },
    );
    vi.spyOn(usage, "fetchAll").mockResolvedValue();

    router.route = "usage";
    router.params = {};
    usage.summary = usageSummaryWithUnsupported("copilot-no-token-data");

    component = mount(UsagePage, { target: document.body });
    await flushEffects();

    expect(document.body.textContent).toContain(
      "Copilot sessions matched this range",
    );
  });

  it("shares full-universe model colors across Usage panels and palette changes", async () => {
    vi.stubGlobal(
      "ResizeObserver",
      class {
        observe() {}
        disconnect() {}
      },
    );
    vi.spyOn(usage, "fetchAll").mockResolvedValue();
    vi.spyOn(sessions, "loadAgents").mockResolvedValue();
    router.route = "usage";
    router.params = {};
    usage.summary = tenModelUsageSummary();
    usage.toggles.timeSeries.groupBy = "model";
    usage.toggles.attribution.groupBy = "model";
    usage.toggles.attribution.view = "list";
    settings.chartPalette = "agentsview";

    component = mount(UsagePage, { target: document.body });
    await flushEffects();

    const firstPath = () => document.querySelector<SVGPathElement>(
      "path[opacity='0.7']",
    );
    const firstDot = () => document.querySelector<HTMLElement>(".list-dot");
    expect(firstPath()?.getAttribute("fill")).toBe("var(--accent-sky)");
    expect(firstDot()?.style.background).toBe("var(--accent-sky)");

    settings.chartPalette = "matplotlib";
    await tick();

    expect(firstPath()?.getAttribute("fill")).toBe("#c5b0d5");
    expect(firstDot()?.style.background).toBe("rgb(197, 176, 213)");
  });

  it("loads agent metadata on mount for the Agent dropdown", async () => {
    vi.stubGlobal(
      "ResizeObserver",
      class {
        observe() {}
        disconnect() {}
      },
    );
    vi.spyOn(usage, "fetchAll").mockResolvedValue();
    const loadAgents = vi.spyOn(sessions, "loadAgents")
      .mockResolvedValue();

    router.route = "usage";
    router.params = {};

    component = mount(UsagePage, { target: document.body });
    await flushEffects();

    expect(loadAgents).toHaveBeenCalled();
  });

  it("ignores response-scoped project keys restored from a URL", async () => {
    vi.stubGlobal(
      "ResizeObserver",
      class {
        observe() {}
        disconnect() {}
      },
    );
    vi.spyOn(usage, "fetchAll").mockResolvedValue();

    router.route = "usage";
    router.params = {
      model: "fixture-model",
      exclude_project_key: "pl1:sha256:stale",
    };
    usage.excludedProjectKeys = "";
    usage.selectedModels = "";

    component = mount(UsagePage, { target: document.body });
    await flushEffects();

    expect(usage.excludedProjectKeys).toBe("");
    expect(usage.selectedModels).toBe("fixture-model");
  });

  it("seeds bare Usage from an enabled fixed range", async () => {
    const fetchStates: Array<{
      isPinned: boolean;
      from: string;
      to: string;
    }> = [];
    vi.stubGlobal(
      "ResizeObserver",
      class {
        observe() {}
        disconnect() {}
      },
    );
    vi.spyOn(usage, "fetchAll").mockImplementation(() => {
      fetchStates.push({
        isPinned: usage.isPinned,
        from: usage.from,
        to: usage.to,
      });
      return Promise.resolve();
    });
    vi.spyOn(sessions, "loadAgents").mockResolvedValue();
    router.route = "usage";
    router.params = {};
    usage.isPinned = false;
    usage.windowDays = 30;
    usage.from = "2026-05-22";
    usage.to = "2026-06-20";
    yokedDates.setEnabled(true);
    yokedDates.updateFromPanel({
      from: "2026-06-01",
      to: "2026-06-07",
      mode: "fixed",
    });

    component = mount(UsagePage, { target: document.body });
    await flushEffects();

    expect(usage.isPinned).toBe(true);
    expect(usage.from).toBe("2026-06-01");
    expect(usage.to).toBe("2026-06-07");
    expect(fetchStates[0]).toEqual({
      isPinned: true,
      from: "2026-06-01",
      to: "2026-06-07",
    });
  });

  it("keeps the note hidden without an unsupported usage signal", async () => {
    vi.stubGlobal(
      "ResizeObserver",
      class {
        observe() {}
        disconnect() {}
      },
    );
    vi.spyOn(usage, "fetchAll").mockResolvedValue();

    router.route = "usage";
    router.params = {};
    usage.summary = usageSummaryWithUnsupported();

    component = mount(UsagePage, { target: document.body });
    await flushEffects();

    expect(document.body.textContent).not.toContain(
      "Copilot sessions matched this range",
    );
  });

  it("renders a generic unsupported usage note for unknown kinds", async () => {
    vi.stubGlobal(
      "ResizeObserver",
      class {
        observe() {}
        disconnect() {}
      },
    );
    vi.spyOn(usage, "fetchAll").mockResolvedValue();

    router.route = "usage";
    router.params = {};
    usage.summary = usageSummaryWithUnsupported("future-no-token-data");

    component = mount(UsagePage, { target: document.body });
    await flushEffects();

    expect(document.body.textContent).toContain(
      "Matching sessions do not expose token usage data",
    );
    expect(document.body.textContent).not.toContain(
      "Copilot sessions matched this range",
    );
  });

  it("does not auto-refresh usage scans from SSE updates", () => {
    expect(source).not.toContain("subscribeDebounced");
    expect(source).not.toContain("REFRESH_MS");
    // SSE only flags new data; the periodic refetch lives in RefreshControl.
    expect(source).toContain("usage.markNewData");
    expect(source).toContain("events.subscribe");
  });

  it("delegates the refresh affordance and scheduler to RefreshControl", () => {
    expect(source).toContain("<RefreshControl");
    expect(source).toContain("usage.lastUpdatedAt");
    expect(source).toContain("label={m.usage_refresh()}");
    expect(source).toContain("title={m.shared_refresh()}");
    // The scheduler, label tick, and icon now live in the shared component.
    expect(source).not.toContain("REFRESH_LABEL_INTERVAL_MS");
    expect(source).not.toContain("formatRefreshAge");
    expect(source).not.toContain("RefreshCwIcon");
    expect(source).not.toContain("setInterval");
  });

  it("shows relative last-updated status without ambiguous badges", () => {
    expect(source).not.toContain("formatUpdatedAt");
    expect(source).not.toContain("usage.hasNewData");
    expect(source).not.toContain("New data");
    expect(source).not.toContain(".new-data");
  });

  it("treats termination as a usage URL session filter", () => {
    expect(source).toContain('"termination",');
    expect(source).toContain("filtersToParams(sessions.filters)");
  });

  it("keeps refresh progress out of content layout flow", () => {
    const queryProgress =
      source.match(/\.query-progress\s*{[^}]+}/)?.[0] ?? "";

    expect(queryProgress).toContain("position: absolute");
    expect(queryProgress).toContain("left: 0;");
    expect(queryProgress).toContain("right: 0;");
    expect(queryProgress).not.toContain("position: sticky");
    expect(queryProgress).not.toContain("margin:");
  });

  it("updates shared yoke state from usage range selections", () => {
    expect(source).toContain("<RangePicker");
    expect(source).toContain("function applyRange");
    expect(source).toContain("updateYokeFromUsage");
    expect(source).toContain("yokedDates.updateFromPanel");
  });

  it("seeds bare usage URLs from shared yoked dates", () => {
    expect(source).toContain("const seed = yokedDates.seedForPanel()");
    expect(source).toContain("applyUsagePanelDate(state)");
    expect(source).toContain("usage.applyRollingWindow");
    expect(source).toContain("usage.applyDateRange");
  });

  it("hydrates supported termination filters from usage URLs", () => {
    const filterKeysIndex = source.indexOf("const SESSION_FILTER_KEYS");
    const urlInitIndex = source.indexOf("let urlInitRan", filterKeysIndex);
    const filterKeysBlock = source.slice(filterKeysIndex, urlInitIndex);

    expect(filterKeysBlock).toContain('"termination"');
  });

  it("does not hydrate session-only date filters from usage URLs", () => {
    const filterKeysIndex = source.indexOf("const SESSION_FILTER_KEYS");
    const urlInitIndex = source.indexOf("let urlInitRan", filterKeysIndex);
    const filterKeysBlock = source.slice(filterKeysIndex, urlInitIndex);

    expect(filterKeysBlock).not.toContain('"date"');
    expect(filterKeysBlock).not.toContain('"date_from"');
    expect(filterKeysBlock).not.toContain('"date_to"');
  });

  it("sanitizes mixed usage URL session params before hydrating", () => {
    const initStart = source.indexOf("if (hasSessionFilterKeys)");
    const initEnd = source.indexOf("if (hasDateParam)", initStart);
    const initBlock = source.slice(initStart, initEnd);

    expect(source).toContain("function usageSupportedSessionParams");
    expect(initBlock).toContain(
      "parseFiltersFromParams(supportedSessionParams)",
    );
    expect(initBlock).toContain(
      "sessions.initFromParams(supportedSessionParams)",
    );
    expect(initBlock).not.toContain("parseFiltersFromParams(params)");
    expect(initBlock).not.toContain("sessions.initFromParams(params)");
  });

  it("mounts the pairwise comparison panel additively", () => {
    expect(source).toContain("UsagePairwiseComparisonPanel");
    expect(source).toContain("<UsagePairwiseComparisonPanel />");
  });

  it("keeps pairwise comparison below bounded secondary usage panels", () => {
    const topSessionsIndex = source.indexOf("<TopSessionsTable />");
    const cacheEfficiencyIndex = source.indexOf("<CacheEfficiencyPanel />");
    const pairwiseIndex = source.indexOf("<UsagePairwiseComparisonPanel />");

    expect(topSessionsIndex).toBeGreaterThan(-1);
    expect(cacheEfficiencyIndex).toBeGreaterThan(-1);
    expect(pairwiseIndex).toBeGreaterThan(cacheEfficiencyIndex);
    expect(pairwiseIndex).toBeGreaterThan(topSessionsIndex);
    expect(source).toContain('class="chart-panel bounded"');
    expect(source).toContain("max-height:");
    expect(source).toContain("overflow: auto;");
  });
});
