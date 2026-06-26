import {
  beforeEach,
  afterEach,
  describe,
  expect,
  it,
  vi,
} from "vite-plus/test";
import type {
  UsageComparison,
  UsageSummaryResponse,
} from "../api/types/usage.js";

const usageServiceMocks = vi.hoisted(() => ({
  getApiV1UsageSummary: vi.fn().mockResolvedValue({
    from: "2024-01-01",
    to: "2024-01-31",
    totals: {
      inputTokens: 0,
      outputTokens: 0,
      cacheCreationTokens: 0,
      cacheReadTokens: 0,
      totalCost: 0,
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
      savingsVsUncached: 0,
    },
  }),
  getApiV1UsageComparison: vi.fn().mockResolvedValue({
    priorFrom: "2023-12-01",
    priorTo: "2023-12-31",
    priorTotalCost: 1,
    deltaPct: 0.5,
  }),
  getApiV1UsageTopSessions: vi.fn().mockResolvedValue([]),
}));

const apiRuntimeMocks = vi.hoisted(() => ({
  configureGeneratedClient: vi.fn(),
  callGenerated: vi.fn((request: () => Promise<unknown>) => request()),
  isAbortError: vi.fn(() => false),
}));

vi.mock("../api/runtime.js", () => apiRuntimeMocks);

vi.mock("../api/generated/index", () => ({
  UsageService: {
    getApiV1UsageSummary: usageServiceMocks.getApiV1UsageSummary,
    getApiV1UsageComparison:
      usageServiceMocks.getApiV1UsageComparison,
    getApiV1UsageTopSessions: usageServiceMocks.getApiV1UsageTopSessions,
  },
}));

const TOGGLES_KEY = "usage-toggles";

function installStorage(initial: Record<string, string> = {}) {
  const data = new Map(Object.entries(initial));
  const storage = {
    getItem: vi.fn((key: string) => data.get(key) ?? null),
    setItem: vi.fn((key: string, value: string) => {
      data.set(key, value);
    }),
    removeItem: vi.fn((key: string) => {
      data.delete(key);
    }),
    clear: vi.fn(() => {
      data.clear();
    }),
  };
  Object.defineProperty(globalThis, "localStorage", {
    value: storage,
    configurable: true,
    writable: true,
  });
  return storage;
}

async function loadStore() {
  vi.resetModules();
  return import("./usage.svelte.js");
}

function usageSummary(totalCost = 0): UsageSummaryResponse {
  return {
    from: "2024-01-01",
    to: "2024-01-31",
    totals: {
      inputTokens: 0,
      outputTokens: 0,
      cacheCreationTokens: 0,
      cacheReadTokens: 0,
      totalCost,
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
      savingsVsUncached: 0,
    },
  };
}

function usageComparison(): UsageComparison {
  return {
    priorFrom: "2023-12-01",
    priorTo: "2023-12-31",
    priorTotalCost: 1,
    deltaPct: 0.5,
  };
}

afterEach(() => {
  apiRuntimeMocks.callGenerated.mockImplementation(
    (request: () => Promise<unknown>) => request(),
  );
});

describe("UsageStore filter persistence", () => {
  beforeEach(() => {
    installStorage();
    localStorage.removeItem(TOGGLES_KEY);
    localStorage.removeItem("usage-filters");
    vi.clearAllMocks();
  });

  it("saves exclude filters to localStorage on fetchAll", async () => {
    const { usage } = await loadStore();
    usage.excludedProjects = "proj-a";
    usage.excludedAgents = "claude";
    await usage.fetchAll();

    const saved = JSON.parse(
      localStorage.getItem("usage-filters") ?? "{}",
    );
    expect(saved.excludedProjects).toBe("proj-a");
    expect(saved.excludedAgents).toBe("claude");
  });

  it("restores usage filters from localStorage on load", async () => {
    localStorage.setItem(
      "usage-filters",
      JSON.stringify({
        excludedProjects: "saved-proj",
        excludedModels: "opus",
        selectedModels: "sonnet",
      }),
    );
    const { usage } = await loadStore();
    expect(usage.excludedProjects).toBe("saved-proj");
    expect(usage.excludedModels).toBe("");
    expect(usage.selectedModels).toBe("sonnet");
    expect(usage.excludedAgents).toBe("");
  });

  it("falls back to defaults on corrupted localStorage", async () => {
    localStorage.setItem("usage-filters", "not json");
    const { usage } = await loadStore();
    expect(usage.excludedProjects).toBe("");
    expect(usage.excludedAgents).toBe("");
  });
});

describe("UsageStore group-by linking", () => {
  beforeEach(() => {
    installStorage();
    localStorage.removeItem(TOGGLES_KEY);
    vi.clearAllMocks();
  });

  it("normalizes legacy split groupBy values onto shared state", async () => {
    localStorage.setItem(
      TOGGLES_KEY,
      JSON.stringify({
        timeSeries: { groupBy: "agent", view: "lines" },
        attribution: { groupBy: "model", view: "list" },
      }),
    );

    const { usage } = await loadStore();

    expect(usage.toggles.timeSeries.groupBy).toBe("agent");
    expect(usage.toggles.attribution.groupBy).toBe("agent");
    expect(usage.toggles.timeSeries.view).toBe("lines");
    expect(usage.toggles.attribution.view).toBe("list");
  });

  it("syncs attribution selector when time-series selector changes", async () => {
    const { usage } = await loadStore();

    usage.setTimeSeriesGroupBy("model");

    expect(usage.toggles.timeSeries.groupBy).toBe("model");
    expect(usage.toggles.attribution.groupBy).toBe("model");
    expect(JSON.parse(localStorage.getItem(TOGGLES_KEY) || "{}")).toMatchObject({
      timeSeries: { groupBy: "model" },
      attribution: { groupBy: "model" },
    });
  });

  it("syncs time-series selector when attribution selector changes", async () => {
    const { usage } = await loadStore();

    usage.setAttributionGroupBy("agent");

    expect(usage.toggles.timeSeries.groupBy).toBe("agent");
    expect(usage.toggles.attribution.groupBy).toBe("agent");
    expect(JSON.parse(localStorage.getItem(TOGGLES_KEY) || "{}")).toMatchObject({
      timeSeries: { groupBy: "agent" },
      attribution: { groupBy: "agent" },
    });
  });
});

describe("UsageStore session filter params", () => {
  beforeEach(() => {
    installStorage();
    vi.clearAllMocks();
  });

  it("passes shared session filters to usage endpoints", async () => {
    const { usage } = await loadStore();
    const { sessions } = await import("./sessions.svelte.js");

    sessions.filters.project = "proj-a";
    sessions.filters.machine = "host-a,host-b";
    sessions.filters.agent = "claude,codex";
    sessions.filters.termination = "abandoned";
    sessions.filters.minUserMessages = 5;
    sessions.filters.includeOneShot = false;
    sessions.filters.includeAutomated = true;
    sessions.filters.recentlyActive = true;

    await usage.fetchAll();

    expect(usageServiceMocks.getApiV1UsageSummary).toHaveBeenLastCalledWith(
      expect.objectContaining({
        project: "proj-a",
        machine: "host-a,host-b",
        agent: "claude,codex",
        termination: "abandoned",
        minUserMessages: 5,
        includeOneShot: false,
        includeAutomated: true,
      }),
    );
    const params = usageServiceMocks.getApiV1UsageSummary.mock.lastCall?.[0];
    expect(params?.activeSince).toEqual(expect.any(String));

    expect(usageServiceMocks.getApiV1UsageTopSessions).toHaveBeenLastCalledWith(
      expect.objectContaining({
        project: "proj-a",
        machine: "host-a,host-b",
        agent: "claude,codex",
        termination: "abandoned",
        minUserMessages: 5,
        includeOneShot: false,
        includeAutomated: true,
      }),
    );
  });

  it("records full refresh time and clears new-data hints", async () => {
    vi.useFakeTimers({ toFake: ["Date"] });
    try {
      vi.setSystemTime(new Date("2026-06-15T16:00:00Z"));
      const { usage } = await loadStore();

      await usage.fetchAll();

      expect(usage.lastUpdatedAt).toBe(
        new Date("2026-06-15T16:00:00Z").getTime(),
      );

      usage.markNewData();
      expect(usage.hasNewData).toBe(true);

      vi.setSystemTime(new Date("2026-06-15T16:03:00Z"));
      await usage.fetchAll();

      expect(usage.lastUpdatedAt).toBe(
        new Date("2026-06-15T16:03:00Z").getTime(),
      );
      expect(usage.hasNewData).toBe(false);
    } finally {
      vi.useRealTimers();
    }
  });

  it("does not mark cached partial refresh failures as current", async () => {
    vi.useFakeTimers({ toFake: ["Date"] });
    const warn = vi.spyOn(console, "warn").mockImplementation(() => {});
    try {
      vi.setSystemTime(new Date("2026-06-15T16:00:00Z"));
      const { usage } = await loadStore();

      await usage.fetchAll();
      const previousUpdatedAt = usage.lastUpdatedAt;

      usage.markNewData();
      usageServiceMocks.getApiV1UsageTopSessions
        .mockRejectedValueOnce(new Error("top sessions failed"));

      vi.setSystemTime(new Date("2026-06-15T16:05:00Z"));
      await usage.fetchAll();

      expect(usage.lastUpdatedAt).toBe(previousUpdatedAt);
      expect(usage.hasNewData).toBe(true);
    } finally {
      warn.mockRestore();
      vi.useRealTimers();
    }
  });

  it("starts summary and top sessions together during full refresh", async () => {
    const calls: string[] = [];
    let resolveSummary:
      | ((value: unknown) => void)
      | undefined;
    const summaryPromise = new Promise((resolve) => {
      resolveSummary = resolve;
    });
    usageServiceMocks.getApiV1UsageSummary.mockImplementationOnce(
      () => {
        calls.push("summary");
        return summaryPromise;
      },
    );
    usageServiceMocks.getApiV1UsageTopSessions.mockImplementationOnce(
      () => {
        calls.push("topSessions");
        return Promise.resolve([]);
      },
    );
    usageServiceMocks.getApiV1UsageComparison.mockImplementationOnce(
      () => {
        calls.push("comparison");
        return Promise.resolve({
          priorFrom: "2023-12-01",
          priorTo: "2023-12-31",
          priorTotalCost: 1,
          deltaPct: 0.5,
        });
      },
    );

    const { usage } = await loadStore();
    const fetch = usage.fetchAll();
    await Promise.resolve();

    expect(calls).toEqual(["summary", "topSessions"]);
    expect(usage.summary).toBeNull();

    resolveSummary?.({
      from: "2024-01-01",
      to: "2024-01-31",
      totals: {
        inputTokens: 0,
        outputTokens: 0,
        cacheCreationTokens: 0,
        cacheReadTokens: 0,
        totalCost: 0,
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
        savingsVsUncached: 0,
      },
    });
    await fetch;
    await Promise.resolve();

    expect(calls).toEqual(["summary", "topSessions", "comparison"]);
    expect(usage.summary).not.toBeNull();
    expect(usage.summary?.comparison).toEqual({
      priorFrom: "2023-12-01",
      priorTo: "2023-12-31",
      priorTotalCost: 1,
      deltaPct: 0.5,
    });
    expect(
      usageServiceMocks.getApiV1UsageComparison,
    ).toHaveBeenCalledWith(
      expect.objectContaining({ currentCost: 0 }),
    );
  });

  it("tracks cached usage refetches as querying without first-load skeletons", async () => {
    const { usage } = await loadStore();

    await usage.fetchAll();
    expect(usage.summary).not.toBeNull();
    expect(usage.loading.summary).toBe(false);
    await vi.waitFor(() => expect(usage.isQuerying).toBe(false));

    let resolveSummary:
      | ((value: UsageSummaryResponse) => void)
      | undefined;
    usageServiceMocks.getApiV1UsageSummary.mockImplementationOnce(
      () =>
        new Promise((resolve) => {
          resolveSummary = resolve;
        }),
    );

    const refetch = usage.fetchAll();
    await Promise.resolve();

    expect(usage.loading.summary).toBe(false);
    expect(usage.querying.summary).toBe(true);
    expect(usage.isQuerying).toBe(true);

    resolveSummary?.(usageSummary(2));
    await refetch;

    expect(usage.querying.summary).toBe(false);
    await vi.waitFor(() => expect(usage.isQuerying).toBe(false));
    expect(usage.summary?.totals.totalCost).toBe(2);
  });

  it("aborts stale top sessions when a new full refresh starts", async () => {
    const signals: (AbortSignal | undefined)[] = [];
    apiRuntimeMocks.callGenerated.mockImplementation(
      (request: () => Promise<unknown>, signal?: AbortSignal) => {
        signals.push(signal);
        return request();
      },
    );
    usageServiceMocks.getApiV1UsageTopSessions.mockImplementationOnce(
      () => new Promise(() => {}),
    );
    usageServiceMocks.getApiV1UsageSummary.mockImplementationOnce(
      () => new Promise(() => {}),
    );

    const { usage } = await loadStore();

    void usage.fetchTopSessions();
    await Promise.resolve();
    expect(signals[0]?.aborted).toBe(false);

    void usage.fetchAll();
    await Promise.resolve();

    expect(signals[0]?.aborted).toBe(true);
  });

  it("reuses summary params for top sessions during full refresh", async () => {
    vi.useFakeTimers({ toFake: ["Date"] });
    try {
      vi.setSystemTime(new Date("2026-04-25T12:00:00"));
      let resolveSummary:
        | ((value: UsageSummaryResponse) => void)
        | undefined;
      usageServiceMocks.getApiV1UsageSummary.mockImplementationOnce(
        () =>
          new Promise((resolve) => {
            resolveSummary = resolve;
          }),
      );

      const { usage } = await loadStore();
      const { sessions } = await import("./sessions.svelte.js");
      sessions.filters.recentlyActive = true;

      const fetch = usage.fetchAll();
      await Promise.resolve();
      const summaryParams =
        usageServiceMocks.getApiV1UsageSummary.mock.lastCall?.[0];

      vi.setSystemTime(new Date("2026-04-26T12:00:00"));
      resolveSummary?.(usageSummary());
      await fetch;

      const topSessionParams =
        usageServiceMocks.getApiV1UsageTopSessions.mock.lastCall?.[0];
      expect(topSessionParams?.activeSince).toBe(summaryParams?.activeSince);
    } finally {
      vi.useRealTimers();
    }
  });

  it("does not let stale comparison abort the current comparison", async () => {
    const signals: (AbortSignal | undefined)[] = [];
    apiRuntimeMocks.callGenerated.mockImplementation(
      (request: () => Promise<unknown>, signal?: AbortSignal) => {
        signals.push(signal);
        return request();
      },
    );

    const { usage } = await loadStore();
    const loaded = await usage.fetchSummary({ loadComparison: false });
    expect(loaded).not.toBeNull();
    if (!loaded) return;
    const loadedSummary = loaded;

    let resolveComparison:
      | ((value: UsageComparison) => void)
      | undefined;
    usageServiceMocks.getApiV1UsageComparison.mockImplementationOnce(
      () =>
        new Promise((resolve) => {
          resolveComparison = resolve;
        }),
    );
    const compare = usage as unknown as {
      fetchComparison: (
        summaryVersion: number,
        summary: UsageSummaryResponse,
        params: typeof loadedSummary.params,
      ) => Promise<void>;
    };

    const currentComparison = compare.fetchComparison(
      loadedSummary.version,
      loadedSummary.summary,
      loadedSummary.params,
    );
    await Promise.resolve();
    const currentSignal = signals[1];
    expect(currentSignal).toBeDefined();
    expect(currentSignal?.aborted).toBe(false);

    await compare.fetchComparison(
      loadedSummary.version - 1,
      loadedSummary.summary,
      loadedSummary.params,
    );

    expect(currentSignal?.aborted).toBe(false);
    expect(
      usageServiceMocks.getApiV1UsageComparison,
    ).toHaveBeenCalledTimes(1);

    resolveComparison?.(usageComparison());
    await currentComparison;
  });

  it("aborts active comparison when a newer summary starts", async () => {
    const signals: (AbortSignal | undefined)[] = [];
    apiRuntimeMocks.callGenerated.mockImplementation(
      (request: () => Promise<unknown>, signal?: AbortSignal) => {
        signals.push(signal);
        return request();
      },
    );

    const { usage } = await loadStore();
    const loaded = await usage.fetchSummary({ loadComparison: false });
    expect(loaded).not.toBeNull();
    if (!loaded) return;
    const loadedSummary = loaded;

    usageServiceMocks.getApiV1UsageComparison.mockImplementationOnce(
      () => new Promise(() => {}),
    );
    const compare = usage as unknown as {
      fetchComparison: (
        summaryVersion: number,
        summary: UsageSummaryResponse,
        params: typeof loadedSummary.params,
      ) => Promise<void>;
    };
    void compare.fetchComparison(
      loadedSummary.version,
      loadedSummary.summary,
      loadedSummary.params,
    );
    await Promise.resolve();
    const comparisonSignal = signals[1];
    expect(comparisonSignal).toBeDefined();
    expect(comparisonSignal?.aborted).toBe(false);

    usageServiceMocks.getApiV1UsageSummary.mockImplementationOnce(
      () => new Promise(() => {}),
    );
    void usage.fetchSummary({ loadComparison: false });
    await Promise.resolve();

    expect(comparisonSignal?.aborted).toBe(true);
  });

  it("refreshes comparison when summary is refreshed directly", async () => {
    const { usage } = await loadStore();

    await usage.fetchSummary();
    await Promise.resolve();

    expect(
      usageServiceMocks.getApiV1UsageComparison,
    ).toHaveBeenCalledTimes(1);
    expect(
      usageServiceMocks.getApiV1UsageTopSessions,
    ).not.toHaveBeenCalled();
    expect(usage.summary?.comparison).toEqual({
      priorFrom: "2023-12-01",
      priorTo: "2023-12-31",
      priorTotalCost: 1,
      deltaPct: 0.5,
    });
  });

  it("aborts stale summary requests when a newer fetch starts", async () => {
    const signals: (AbortSignal | undefined)[] = [];
    apiRuntimeMocks.callGenerated.mockImplementation(
      (request: () => Promise<unknown>, signal?: AbortSignal) => {
        signals.push(signal);
        return request();
      },
    );
    usageServiceMocks.getApiV1UsageSummary
      .mockImplementationOnce(() => new Promise(() => {}))
      .mockResolvedValueOnce({
        from: "2024-01-01",
        to: "2024-01-31",
        totals: {
          inputTokens: 0,
          outputTokens: 0,
          cacheCreationTokens: 0,
          cacheReadTokens: 0,
          totalCost: 0,
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
          savingsVsUncached: 0,
        },
      });

    const { usage } = await loadStore();

    void usage.fetchSummary();
    await Promise.resolve();
    void usage.fetchSummary();
    await Promise.resolve();

    expect(signals[0]).toBeDefined();
    expect(signals[0]?.aborted).toBe(true);
  });
});

describe("UsageStore rolling default date range", () => {
  beforeEach(() => {
    installStorage();
    localStorage.removeItem("usage-toggles");
    localStorage.removeItem("usage-filters");
    vi.clearAllMocks();
    vi.useFakeTimers({ toFake: ["Date"] });
    vi.setSystemTime(new Date("2026-04-25T12:00:00"));
  });

  afterEach(() => {
    vi.useRealTimers();
  });

  it("constructor produces isPinned=false and windowDays=30 with rolling defaults", async () => {
    const { usage } = await loadStore();
    expect(usage.isPinned).toBe(false);
    expect(usage.windowDays).toBe(30);
    expect(usage.from).toBe("2026-03-26");
    expect(usage.to).toBe("2026-04-25");
  });

  it("fetchAll re-derives from/to against the current clock while unpinned", async () => {
    const { usage } = await loadStore();

    expect(usage.from).toBe("2026-03-26");
    expect(usage.to).toBe("2026-04-25");

    vi.setSystemTime(new Date("2026-04-26T12:00:00"));
    await usage.fetchAll();

    expect(usage.from).toBe("2026-03-27");
    expect(usage.to).toBe("2026-04-26");
  });

  it("setDateRange pins and subsequent fetchAll does not roll", async () => {
    const { usage } = await loadStore();
    usage.setDateRange("2026-01-01", "2026-01-15");
    expect(usage.isPinned).toBe(true);
    expect(usage.from).toBe("2026-01-01");
    expect(usage.to).toBe("2026-01-15");

    vi.setSystemTime(new Date("2026-04-26T12:00:00"));
    await usage.fetchAll();

    expect(usage.isPinned).toBe(true);
    expect(usage.from).toBe("2026-01-01");
    expect(usage.to).toBe("2026-01-15");
  });

  it("setRollingWindow sets windowDays, clears the pin, and re-derives dates", async () => {
    const { usage } = await loadStore();
    usage.setDateRange("2026-01-01", "2026-01-15");
    expect(usage.isPinned).toBe(true);

    usage.setRollingWindow(7);

    expect(usage.isPinned).toBe(false);
    expect(usage.windowDays).toBe(7);
    expect(usage.from).toBe("2026-04-18");
    expect(usage.to).toBe("2026-04-25");
  });

  it("after setRollingWindow, fetchAll keeps rolling", async () => {
    const { usage } = await loadStore();
    usage.setRollingWindow(7);
    expect(usage.from).toBe("2026-04-18");

    vi.setSystemTime(new Date("2026-04-26T12:00:00"));
    await usage.fetchAll();

    expect(usage.from).toBe("2026-04-19");
    expect(usage.to).toBe("2026-04-26");
  });
});

describe("buildUsageUrlParams", () => {
  it("omits from/to when isPinned is false with default window, includes header filters", async () => {
    const { buildUsageUrlParams } = await loadStore();
    const params = buildUsageUrlParams({
      from: "2026-03-26",
      to: "2026-04-25",
      isPinned: false,
      windowDays: 30,
      excludedProjects: "p1",
      excludedAgents: "a1",
      excludedModels: "m1",
      selectedModels: "m2",
    });
    expect(params).toEqual({
      exclude_project: "p1",
      model: "m2",
    });
  });

  it("includes from/to when isPinned is true", async () => {
    const { buildUsageUrlParams } = await loadStore();
    const params = buildUsageUrlParams({
      from: "2026-01-01",
      to: "2026-01-15",
      isPinned: true,
      windowDays: 30,
      excludedProjects: "",
      excludedAgents: "",
      excludedModels: "",
      selectedModels: "",
    });
    expect(params).toEqual({
      from: "2026-01-01",
      to: "2026-01-15",
    });
  });

  it("returns empty object when nothing is set", async () => {
    const { buildUsageUrlParams } = await loadStore();
    const params = buildUsageUrlParams({
      from: "",
      to: "",
      isPinned: false,
      windowDays: 30,
      excludedProjects: "",
      excludedAgents: "",
      excludedModels: "",
      selectedModels: "",
    });
    expect(params).toEqual({});
  });

  it("omits empty from/to even when pinned", async () => {
    const { buildUsageUrlParams } = await loadStore();
    const params = buildUsageUrlParams({
      from: "",
      to: "",
      isPinned: true,
      windowDays: 30,
      excludedProjects: "",
      excludedAgents: "",
      excludedModels: "",
      selectedModels: "",
    });
    expect(params).toEqual({});
  });

  it("emits window_days for unpinned non-default windows", async () => {
    const { buildUsageUrlParams } = await loadStore();
    const params = buildUsageUrlParams({
      from: "2026-04-19",
      to: "2026-04-25",
      isPinned: false,
      windowDays: 7,
      excludedProjects: "",
      excludedAgents: "",
      excludedModels: "",
      selectedModels: "",
    });
    expect(params).toEqual({ window_days: "7" });
  });

  it("omits window_days when isPinned is true", async () => {
    const { buildUsageUrlParams } = await loadStore();
    const params = buildUsageUrlParams({
      from: "2026-01-01",
      to: "2026-01-15",
      isPinned: true,
      windowDays: 7,
      excludedProjects: "",
      excludedAgents: "",
      excludedModels: "",
      selectedModels: "",
    });
    expect(params).toEqual({
      from: "2026-01-01",
      to: "2026-01-15",
    });
  });
});

describe("mergeUsageAndSessionUrlParams", () => {
  it("merges overlapping CSV params instead of overwriting usage filters", async () => {
    const { mergeUsageAndSessionUrlParams } = await loadStore();

    expect(
      mergeUsageAndSessionUrlParams(
        {
          exclude_project: "alpha,beta",
          model: "gpt-5.5",
        },
        {
          exclude_project: "unknown,beta",
          machine: "host-a",
        },
      ),
    ).toEqual({
      exclude_project: "alpha,beta,unknown",
      model: "gpt-5.5",
      machine: "host-a",
    });
  });

  it("omits hidden session date params from usage URLs", async () => {
    const { mergeUsageAndSessionUrlParams } = await loadStore();

    expect(
      mergeUsageAndSessionUrlParams(
        {
          from: "2026-02-01",
          to: "2026-02-07",
        },
        {
          date: "2026-01-15",
          date_from: "2026-01-01",
          date_to: "2026-01-31",
          project: "agentsview",
        },
      ),
    ).toEqual({
      from: "2026-02-01",
      to: "2026-02-07",
      project: "agentsview",
    });
  });

  it("preserves supported termination params in usage URLs", async () => {
    const { mergeUsageAndSessionUrlParams } = await loadStore();

    expect(
      mergeUsageAndSessionUrlParams(
        {
          from: "2026-02-01",
          to: "2026-02-07",
        },
        {
          termination: "unclean",
          project: "agentsview",
        },
      ),
    ).toEqual({
      from: "2026-02-01",
      to: "2026-02-07",
      termination: "unclean",
      project: "agentsview",
    });
  });
});

describe("parseWindowDays", () => {
  it("returns the parsed integer for valid positive integers", async () => {
    const { parseWindowDays } = await loadStore();
    expect(parseWindowDays("7")).toBe(7);
    expect(parseWindowDays("365")).toBe(365);
  });

  it("rejects non-positive, non-integer, and malformed values", async () => {
    const { parseWindowDays } = await loadStore();
    expect(parseWindowDays(undefined)).toBeNull();
    expect(parseWindowDays("")).toBeNull();
    expect(parseWindowDays("0")).toBeNull();
    expect(parseWindowDays("-7")).toBeNull();
    expect(parseWindowDays("7.5")).toBeNull();
    expect(parseWindowDays("7d")).toBeNull();
    expect(parseWindowDays("abc")).toBeNull();
  });

  it("accepts values up to the 100-year cap and rejects beyond", async () => {
    const { parseWindowDays } = await loadStore();
    expect(parseWindowDays("36500")).toBe(36500);
    expect(parseWindowDays("36501")).toBeNull();
    expect(parseWindowDays("1000000000")).toBeNull();
  });
});
