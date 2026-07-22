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
import { yokedDates } from "../../stores/yokedDates.svelte.js";
import { testMoney } from "../../test/money.js";
import source from "./UsagePage.svelte?raw";
import UsagePage from "./UsagePage.svelte";

async function flushEffects() {
  await tick();
  await Promise.resolve();
  await tick();
}

let component: ReturnType<typeof mount> | undefined;

function usageSummaryWithUnsupported(kind?: string) {
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
  usage.summary = null;
  usage.topSessions = null;
  usage.errors.summary = null;
  usage.isPinned = false;
  usage.windowDays = 30;
  usage.from = "";
  usage.to = "";
  sessions.projects = [];
  yokedDates.setEnabled(false);
  localStorage.clear();
});

describe("UsagePage refresh behavior", () => {
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
