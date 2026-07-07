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
  UsagePairwiseComparisonResponse,
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
    projectTotals: [
      {
        project_key: "pl1:sha256:alpha",
        project: "alpha",
        inputTokens: 0,
        outputTokens: 0,
        cacheCreationTokens: 0,
        cacheReadTokens: 0,
        cost: 0,
      },
      {
        project_key: "pl1:sha256:beta",
        project: "beta",
        inputTokens: 0,
        outputTokens: 0,
        cacheCreationTokens: 0,
        cacheReadTokens: 0,
        cost: 0,
      },
    ],
    modelTotals: [
      {
        model: "claude-sonnet-4-20250514",
        inputTokens: 0,
        outputTokens: 0,
        cacheCreationTokens: 0,
        cacheReadTokens: 0,
        cost: 0,
      },
      {
        model: "gpt-4o",
        inputTokens: 0,
        outputTokens: 0,
        cacheCreationTokens: 0,
        cacheReadTokens: 0,
        cost: 0,
      },
    ],
    agentTotals: [],
    branchTotals: [],
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
  getApiV1UsagePairwiseComparison: vi.fn().mockResolvedValue({
    left: {
      totalCost: 1,
      inputTokens: 10,
      outputTokens: 5,
      cacheCreationTokens: 0,
      cacheReadTokens: 0,
      totalTokens: 15,
      sessionCount: 1,
      costPerSession: 1,
      tokensPerSession: 15,
    },
    right: {
      totalCost: 2,
      inputTokens: 20,
      outputTokens: 10,
      cacheCreationTokens: 0,
      cacheReadTokens: 0,
      totalTokens: 30,
      sessionCount: 2,
      costPerSession: 1,
      tokensPerSession: 15,
    },
    deltas: {
      totalCostDelta: 1,
      totalCostDeltaRatio: 1,
      inputTokensDelta: 10,
      inputTokensDeltaRatio: 1,
      outputTokensDelta: 5,
      outputTokensDeltaRatio: 1,
      cacheCreationDelta: 0,
      cacheCreationDeltaRatio: null,
      cacheReadDelta: 0,
      cacheReadDeltaRatio: null,
      totalTokensDelta: 15,
      totalTokensDeltaRatio: 1,
      sessionCountDelta: 1,
      sessionCountDeltaRatio: 1,
      costPerSessionDelta: 0,
      costPerSessionRatio: 0,
      tokensPerSessionDelta: 0,
      tokensPerSessionRatio: 0,
    },
  }),
  getApiV1UsageTopSessions: vi.fn().mockResolvedValue([]),
}));

const metadataServiceMocks = vi.hoisted(() => ({
  getApiV1Branches: vi.fn().mockResolvedValue({
    branches: [
      { project: "alpha", branch: "main", session_count: 3 },
    ],
  }),
}));

const apiRuntimeMocks = vi.hoisted(() => {
  class ApiError extends Error {
    constructor(
      public readonly status: number,
      message: string,
      public readonly code?: string,
    ) {
      super(message);
      this.name = "ApiError";
    }
  }
  return {
    ApiError,
    configureGeneratedClient: vi.fn(),
    callGenerated: vi.fn((request: () => Promise<unknown>) => request()),
    isAbortError: vi.fn(() => false),
  };
});

vi.mock("../api/runtime.js", () => apiRuntimeMocks);

vi.mock("../api/generated/index", () => ({
  UsageService: {
    getApiV1UsageSummary: usageServiceMocks.getApiV1UsageSummary,
    getApiV1UsageComparison:
      usageServiceMocks.getApiV1UsageComparison,
    getApiV1UsagePairwiseComparison:
      usageServiceMocks.getApiV1UsagePairwiseComparison,
    getApiV1UsageTopSessions: usageServiceMocks.getApiV1UsageTopSessions,
  },
  MetadataService: {
    getApiV1Branches: metadataServiceMocks.getApiV1Branches,
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
    projectTotals: [
      {
        project_key: "pl1:sha256:alpha",
        project: "alpha",
        inputTokens: 0,
        outputTokens: 0,
        cacheCreationTokens: 0,
        cacheReadTokens: 0,
        cost: 0,
      },
      {
        project_key: "pl1:sha256:beta",
        project: "beta",
        inputTokens: 0,
        outputTokens: 0,
        cacheCreationTokens: 0,
        cacheReadTokens: 0,
        cost: 0,
      },
    ],
    modelTotals: [
      {
        model: "claude-sonnet-4-20250514",
        inputTokens: 0,
        outputTokens: 0,
        cacheCreationTokens: 0,
        cacheReadTokens: 0,
        cost: 0,
      },
      {
        model: "gpt-4o",
        inputTokens: 0,
        outputTokens: 0,
        cacheCreationTokens: 0,
        cacheReadTokens: 0,
        cost: 0,
      },
    ],
    agentTotals: [],
    branchTotals: [],
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

function usageSummaryWithOptions(options: {
  totalCost?: number;
  projects?: string[];
  models?: string[];
} = {}): UsageSummaryResponse {
  const totalCost = options.totalCost ?? 0;
  const projects = options.projects ?? ["alpha", "beta"];
  const models = options.models ?? [
    "claude-sonnet-4-20250514",
    "gpt-4o",
  ];
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
    projectTotals: projects.map((project) => ({
      project_key: `pl1:sha256:${project}`,
      project,
      inputTokens: 0,
      outputTokens: 0,
      cacheCreationTokens: 0,
      cacheReadTokens: 0,
      cost: 0,
    })),
    modelTotals: models.map((model) => ({
      model,
      inputTokens: 0,
      outputTokens: 0,
      cacheCreationTokens: 0,
      cacheReadTokens: 0,
      cost: 0,
    })),
    agentTotals: [],
    branchTotals: [],
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

function usagePairwiseComparison(): UsagePairwiseComparisonResponse {
  return {
    left: {
      totalCost: 1,
      inputTokens: 10,
      outputTokens: 5,
      cacheCreationTokens: 0,
      cacheReadTokens: 0,
      totalTokens: 15,
      sessionCount: 1,
      costPerSession: 1,
      tokensPerSession: 15,
    },
    right: {
      totalCost: 2,
      inputTokens: 20,
      outputTokens: 10,
      cacheCreationTokens: 0,
      cacheReadTokens: 0,
      totalTokens: 30,
      sessionCount: 2,
      costPerSession: 1,
      tokensPerSession: 15,
    },
    deltas: {
      totalCostDelta: 1,
      totalCostDeltaRatio: 1,
      inputTokensDelta: 10,
      inputTokensDeltaRatio: 1,
      outputTokensDelta: 5,
      outputTokensDeltaRatio: 1,
      cacheCreationDelta: 0,
      cacheCreationDeltaRatio: null,
      cacheReadDelta: 0,
      cacheReadDeltaRatio: null,
      totalTokensDelta: 15,
      totalTokensDeltaRatio: 1,
      sessionCountDelta: 1,
      sessionCountDeltaRatio: 1,
      costPerSessionDelta: 0,
      costPerSessionRatio: 0,
      tokensPerSessionDelta: 0,
      tokensPerSessionRatio: 0,
    },
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
    usage.excludedProjectKeys = "pl1:sha256:proj-a";
    usage.excludedAgents = "claude";
    usage.excludedGitBranch = "proj-a\u001fmain";
    await usage.fetchAll();

    const saved = JSON.parse(
      localStorage.getItem("usage-filters") ?? "{}",
    );
    expect(saved.excludedProjects).toBe("proj-a");
    expect(saved.excludedProjectKeys).toBeUndefined();
    expect(saved.excludedAgents).toBe("claude");
    expect(saved.excludedGitBranch).toBe("proj-a\u001fmain");
  });

  it("restores usage filters from localStorage on load", async () => {
    localStorage.setItem(
      "usage-filters",
      JSON.stringify({
        excludedProjects: "saved-proj",
        excludedProjectKeys: "pl1:sha256:saved-proj",
        excludedModels: "opus",
        selectedModels: "sonnet",
      }),
    );
    const { usage } = await loadStore();
    expect(usage.excludedProjects).toBe("saved-proj");
    expect(usage.excludedProjectKeys).toBe("");
    expect(usage.excludedModels).toBe("");
    expect(usage.selectedModels).toBe("sonnet");
    expect(usage.excludedAgents).toBe("");
  });

  it("preserves unsupported usage metadata when comparison data is merged", async () => {
    usageServiceMocks.getApiV1UsageSummary.mockResolvedValueOnce({
      ...usageSummary(0),
      unsupportedUsage: { kind: "copilot-no-token-data" },
    });

    const { usage } = await loadStore();
    await usage.fetchSummary();
    await Promise.resolve();

    expect(usage.summary?.unsupportedUsage).toEqual({
      kind: "copilot-no-token-data",
    });
    expect(usage.summary?.comparison).toEqual(usageComparison());
  });

  it("falls back to defaults on corrupted localStorage", async () => {
    localStorage.setItem("usage-filters", "not json");
    const { usage } = await loadStore();
    expect(usage.excludedProjects).toBe("");
    expect(usage.excludedAgents).toBe("");
  });
});

describe("UsageStore branch metadata", () => {
  beforeEach(() => {
    installStorage();
    vi.clearAllMocks();
  });

  it("loads branches once and caches the result", async () => {
    const { usage } = await loadStore();
    await usage.loadBranches();
    await usage.loadBranches();

    expect(metadataServiceMocks.getApiV1Branches)
      .toHaveBeenCalledTimes(1);
    expect(usage.branches).toEqual([
      { project: "alpha", branch: "main", session_count: 3 },
    ]);
  });

  it("refetches branches after a sync invalidates the cache", async () => {
    const { usage } = await loadStore();
    await usage.loadBranches();
    expect(metadataServiceMocks.getApiV1Branches)
      .toHaveBeenCalledTimes(1);

    usage.invalidateBranches();
    await usage.loadBranches();

    expect(metadataServiceMocks.getApiV1Branches)
      .toHaveBeenCalledTimes(2);
  });

  it("drops a branch response superseded by an invalidation", async () => {
    const { usage } = await loadStore();
    let resolveFetch!: (v: unknown) => void;
    metadataServiceMocks.getApiV1Branches.mockImplementationOnce(
      () => new Promise((r) => { resolveFetch = r; }),
    );
    const p = usage.loadBranches();
    usage.invalidateBranches();
    resolveFetch({
      branches: [{ project: "alpha", branch: "stale", session_count: 1 }],
    });
    await p;

    expect(usage.branches).toEqual([]);
    await usage.loadBranches();
    expect(metadataServiceMocks.getApiV1Branches)
      .toHaveBeenCalledTimes(2);
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
    sessions.filters.branch = "proj-a\u001fmain";
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
        gitBranch: "proj-a\u001fmain",
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
        gitBranch: "proj-a\u001fmain",
        agent: "claude,codex",
        termination: "abandoned",
        minUserMessages: 5,
        includeOneShot: false,
        includeAutomated: true,
      }),
    );
  });

  it("passes exclusion filters to usage endpoints", async () => {
    const { usage } = await loadStore();

    usage.excludedProjects = "proj-a,proj-b";
    usage.excludedProjectKeys = "pl1:sha256:project";
    usage.excludedAgents = "codex";

    await usage.fetchAll();

    expect(usageServiceMocks.getApiV1UsageSummary).toHaveBeenLastCalledWith(
      expect.objectContaining({
        excludeProject: "proj-a,proj-b",
        excludeProjectKey: "pl1:sha256:project",
        excludeAgent: "codex",
      }),
    );
    expect(usageServiceMocks.getApiV1UsageTopSessions).toHaveBeenLastCalledWith(
      expect.objectContaining({
        excludeProject: "proj-a,proj-b",
        excludeProjectKey: "pl1:sha256:project",
        excludeAgent: "codex",
      }),
    );
  });

  it("refreshes response-scoped project selections after archive identity changes", async () => {
    usageServiceMocks.getApiV1UsageSummary.mockRejectedValueOnce(
      new apiRuntimeMocks.ApiError(
        400,
        "unknown project key",
        "unknown_project_key",
      ),
    );
    const { usage } = await loadStore();
    usage.excludedProjectKeys = "pl1:sha256:stale";
    usage.pairwiseSelection = {
      left: { dimension: "project", value: "pl1:sha256:stale" },
      right: { dimension: "model", value: "gpt-4o" },
    };

    await usage.fetchAll();

    expect(usage.excludedProjectKeys).toBe("");
    expect(usageServiceMocks.getApiV1UsageSummary).toHaveBeenCalledTimes(2);
    expect(usageServiceMocks.getApiV1UsageSummary).toHaveBeenNthCalledWith(
      1,
      expect.objectContaining({
        excludeProjectKey: "pl1:sha256:stale",
      }),
    );
    expect(usageServiceMocks.getApiV1UsageSummary).toHaveBeenNthCalledWith(
      2,
      expect.not.objectContaining({ excludeProjectKey: expect.anything() }),
    );
    expect(usageServiceMocks.getApiV1UsageTopSessions).toHaveBeenCalledTimes(2);
    expect(usage.pairwiseSelection.left.value).not.toBe(
      "pl1:sha256:stale",
    );
    expect(usage.summary).not.toBeNull();
  });

  it("passes branch exclusions to usage endpoints", async () => {
    const { usage } = await loadStore();

    usage.excludedGitBranch = "proj-a\u001fmain\u001eproj-b\u001fdev";

    await usage.fetchAll();

    expect(usageServiceMocks.getApiV1UsageSummary).toHaveBeenLastCalledWith(
      expect.objectContaining({
        excludeGitBranch: "proj-a\u001fmain\u001eproj-b\u001fdev",
      }),
    );
    expect(usageServiceMocks.getApiV1UsageTopSessions).toHaveBeenLastCalledWith(
      expect.objectContaining({
        excludeGitBranch: "proj-a\u001fmain\u001eproj-b\u001fdev",
      }),
    );
  });

  it("toggles branch exclusions with the list separator", async () => {
    const { usage } = await loadStore();
    const tokenA = "proj-a\u001fmain";
    const tokenB = "proj-b\u001fdev";

    usage.toggleBranch(tokenA);
    usage.toggleBranch(tokenB);
    expect(usage.excludedGitBranch).toBe(`${tokenA}\u001e${tokenB}`);
    expect(usage.isBranchExcluded(tokenA)).toBe(true);
    expect(usage.isBranchExcluded(tokenB)).toBe(true);
    expect(usage.hasActiveFilters).toBe(true);

    usage.toggleBranch(tokenA);
    expect(usage.excludedGitBranch).toBe(tokenB);
    expect(usage.isBranchExcluded(tokenA)).toBe(false);

    usage.selectAllBranches();
    expect(usage.excludedGitBranch).toBe("");

    usage.deselectAllBranches([tokenA, tokenB]);
    expect(usage.excludedGitBranch).toBe(`${tokenA}\u001e${tokenB}`);
  });

  it("stores pairwise comparison data from the generated API", async () => {
    const { usage } = await loadStore();

    await usage.fetchAll();

    expect(
      usageServiceMocks.getApiV1UsagePairwiseComparison,
    ).toHaveBeenCalledTimes(1);
    expect(usage.pairwiseComparison).toEqual(usagePairwiseComparison());
  });

  it("clears stale pairwise results before refetching after a selector change", async () => {
    const { usage } = await loadStore();

    await usage.fetchAll();
    expect(usage.pairwiseComparison).toEqual(usagePairwiseComparison());

    usageServiceMocks.getApiV1UsagePairwiseComparison.mockImplementationOnce(
      () => new Promise(() => {}),
    );

    usage.setPairwiseSide("left", { value: "gpt-4o" });

    expect(usage.pairwiseSelection.left.value).toBe("gpt-4o");
    expect(usage.pairwiseComparison).toBeNull();
  });

  it("clears stale pairwise results when a summary refresh rewrites the selection", async () => {
    const { usage } = await loadStore();

    await usage.fetchAll();
    expect(usage.pairwiseSelection).toEqual({
      left: {
        dimension: "model",
        value: "claude-sonnet-4-20250514",
      },
      right: {
        dimension: "model",
        value: "gpt-4o",
      },
    });
    expect(usage.pairwiseComparison).toEqual(usagePairwiseComparison());

    usageServiceMocks.getApiV1UsageSummary.mockResolvedValueOnce(
      usageSummaryWithOptions({
        projects: ["beta", "gamma"],
        models: ["gpt-4o"],
      }),
    );
    usageServiceMocks.getApiV1UsagePairwiseComparison.mockImplementationOnce(
      () => new Promise(() => {}),
    );

    void usage.fetchAll();
    await Promise.resolve();
    await Promise.resolve();

    expect(usage.pairwiseSelection).toEqual({
      left: { dimension: "project", value: "pl1:sha256:beta" },
      right: { dimension: "project", value: "pl1:sha256:gamma" },
    });
    expect(usage.pairwiseComparison).toBeNull();
  });

  it("clears stale pairwise results when filters change without selector changes", async () => {
    const warn = vi.spyOn(console, "warn").mockImplementation(() => {});
    try {
      const { usage } = await loadStore();

      await usage.fetchAll();
      expect(usage.pairwiseComparison).toEqual(usagePairwiseComparison());

      usageServiceMocks.getApiV1UsagePairwiseComparison.mockRejectedValueOnce(
        new Error("pairwise failed"),
      );

      usage.applyDateRange("2024-02-01", "2024-02-29");
      await usage.fetchAll();

      expect(usage.pairwiseSelection).toEqual({
        left: {
          dimension: "model",
          value: "claude-sonnet-4-20250514",
        },
        right: {
          dimension: "model",
          value: "gpt-4o",
        },
      });
      expect(usage.pairwiseComparison).toBeNull();
      expect(usage.errors.pairwise).toBe("pairwise failed");
    } finally {
      warn.mockRestore();
    }
  });

  it("clears pairwise loading when an aborted first load resolves to no selection", async () => {
    const { usage } = await loadStore();

    usageServiceMocks.getApiV1UsagePairwiseComparison.mockImplementationOnce(
      () => new Promise(() => {}),
    );

    void usage.fetchAll();
    await vi.waitFor(() =>
      expect(
        usageServiceMocks.getApiV1UsagePairwiseComparison,
      ).toHaveBeenCalledTimes(1),
    );

    expect(usage.loading.pairwise).toBe(true);

    usageServiceMocks.getApiV1UsageSummary.mockResolvedValueOnce(
      usageSummaryWithOptions({
        projects: [],
        models: ["gpt-4o"],
      }),
    );

    await usage.fetchAll();

    expect(usage.pairwiseSelection).toEqual({
      left: { dimension: "model", value: "" },
      right: { dimension: "model", value: "" },
    });
    expect(usage.loading.pairwise).toBe(false);
    expect(usage.pairwiseComparison).toBeNull();
  });

  it("clears pairwise loading when a refresh aborts first load and summary fails", async () => {
    const warn = vi.spyOn(console, "warn").mockImplementation(() => {});
    try {
      const { usage } = await loadStore();

      usageServiceMocks.getApiV1UsagePairwiseComparison.mockImplementationOnce(
        () => new Promise(() => {}),
      );

      void usage.fetchAll();
      await vi.waitFor(() =>
        expect(
          usageServiceMocks.getApiV1UsagePairwiseComparison,
        ).toHaveBeenCalledTimes(1),
      );

      expect(usage.loading.pairwise).toBe(true);

      usageServiceMocks.getApiV1UsageSummary.mockRejectedValueOnce(
        new Error("summary failed"),
      );

      await usage.fetchAll();

      expect(usage.loading.pairwise).toBe(false);
      expect(usage.pairwiseComparison).toBeNull();
    } finally {
      warn.mockRestore();
    }
  });

  it("clears pairwise querying when a selector change leaves no comparable values", async () => {
    const { usage } = await loadStore();

    usageServiceMocks.getApiV1UsagePairwiseComparison.mockImplementationOnce(
      () => new Promise(() => {}),
    );

    void usage.fetchAll();
    await vi.waitFor(() =>
      expect(
        usageServiceMocks.getApiV1UsagePairwiseComparison,
      ).toHaveBeenCalledTimes(1),
    );

    usage.summary = usageSummaryWithOptions({
      projects: [],
      models: ["gpt-4o"],
    });

    usage.setPairwiseSide("left", { dimension: "project" });

    expect(usage.pairwiseSelection.left).toEqual({
      dimension: "project",
      value: "",
    });
    expect(usage.loading.pairwise).toBe(false);
    expect(usage.querying.pairwise).toBe(false);
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
    usageServiceMocks.getApiV1UsagePairwiseComparison.mockImplementationOnce(
      () => {
        calls.push("pairwise");
        return Promise.resolve(usagePairwiseComparison());
      },
    );

    const { usage } = await loadStore();
    const fetch = usage.fetchAll();
    await Promise.resolve();

    expect(calls).toEqual(["summary", "topSessions"]);
    expect(usage.summary).toBeNull();

    resolveSummary?.(usageSummary());
    await fetch;
    await Promise.resolve();

    expect(calls).toEqual([
      "summary",
      "topSessions",
      "comparison",
      "pairwise",
    ]);
    expect(usage.summary).not.toBeNull();
    expect(usage.summary?.comparison).toEqual({
      priorFrom: "2023-12-01",
      priorTo: "2023-12-31",
      priorTotalCost: 1,
      deltaPct: 0.5,
    });
    expect(usage.pairwiseComparison).toEqual(usagePairwiseComparison());
    expect(
      usageServiceMocks.getApiV1UsageComparison,
    ).toHaveBeenCalledWith(
      expect.objectContaining({ currentCost: 0 }),
    );
    expect(
      usageServiceMocks.getApiV1UsagePairwiseComparison,
    ).toHaveBeenCalledWith(
      expect.objectContaining({
        leftDimension: expect.any(String),
        leftValue: expect.any(String),
        rightDimension: expect.any(String),
        rightValue: expect.any(String),
      }),
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

  it("aborts visible panel requests on teardown", async () => {
    const signals: (AbortSignal | undefined)[] = [];
    apiRuntimeMocks.callGenerated.mockImplementation(
      (request: () => Promise<unknown>, signal?: AbortSignal) => {
        signals.push(signal);
        return request();
      },
    );
    usageServiceMocks.getApiV1UsageSummary.mockImplementationOnce(
      () => new Promise(() => {}),
    );
    const { usage } = await loadStore();

    void usage.fetchSummary();
    await Promise.resolve();
    usage.cancelInFlightReads();

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
      usageServiceMocks.getApiV1UsagePairwiseComparison,
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
    expect(usage.pairwiseComparison).toEqual(usagePairwiseComparison());
  });

  it("clears stale pairwise data and ignores late selector responses", async () => {
    const { usage } = await loadStore();

    await usage.fetchAll();
    expect(usage.pairwiseComparison).toEqual(usagePairwiseComparison());

    let resolveFirst:
      | ((value: UsagePairwiseComparisonResponse) => void)
      | undefined;
    let resolveSecond:
      | ((value: UsagePairwiseComparisonResponse) => void)
      | undefined;
    usageServiceMocks.getApiV1UsagePairwiseComparison
      .mockImplementationOnce(
        () =>
          new Promise((resolve) => {
            resolveFirst = resolve;
          }),
      )
      .mockImplementationOnce(
        () =>
          new Promise((resolve) => {
            resolveSecond = resolve;
          }),
      );

    usage.setPairwiseSide("left", { dimension: "project" });
    await Promise.resolve();
    expect(usage.pairwiseComparison).toBeNull();
    expect(usage.loading.pairwise).toBe(true);

    usage.setPairwiseSide("right", {
      dimension: "project",
      value: "pl1:sha256:alpha",
    });
    await Promise.resolve();

    resolveFirst?.({
      ...usagePairwiseComparison(),
      left: {
        ...usagePairwiseComparison().left,
        totalCost: 99,
      },
    });
    await Promise.resolve();
    expect(usage.pairwiseComparison).toBeNull();

    const latest = {
      ...usagePairwiseComparison(),
      left: {
        ...usagePairwiseComparison().left,
        totalCost: 3,
      },
      deltas: {
        ...usagePairwiseComparison().deltas,
        totalCostDelta: 2,
        totalCostDeltaRatio: 2,
      },
    };
    resolveSecond?.(latest);
    await vi.waitFor(() => {
      expect(usage.pairwiseComparison).toEqual(latest);
    });
    expect(
      usageServiceMocks.getApiV1UsagePairwiseComparison,
    ).toHaveBeenNthCalledWith(
      2,
      expect.objectContaining({
        leftDimension: "project",
      }),
    );
    expect(
      usageServiceMocks.getApiV1UsagePairwiseComparison,
    ).toHaveBeenNthCalledWith(
      3,
      expect.objectContaining({
        leftDimension: "project",
        rightDimension: "project",
        rightValue: "pl1:sha256:alpha",
      }),
    );
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
    expect(usage.from).toBe("2026-03-27");
    expect(usage.to).toBe("2026-04-25");
  });

  it("fetchAll re-derives from/to against the current clock while unpinned", async () => {
    const { usage } = await loadStore();

    expect(usage.from).toBe("2026-03-27");
    expect(usage.to).toBe("2026-04-25");

    vi.setSystemTime(new Date("2026-04-26T12:00:00"));
    await usage.fetchAll();

    expect(usage.from).toBe("2026-03-28");
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
    expect(usage.from).toBe("2026-04-19");
    expect(usage.to).toBe("2026-04-25");
  });

  it("after setRollingWindow, fetchAll keeps rolling", async () => {
    const { usage } = await loadStore();
    usage.setRollingWindow(7);
    expect(usage.from).toBe("2026-04-19");

    vi.setSystemTime(new Date("2026-04-26T12:00:00"));
    await usage.fetchAll();

    expect(usage.from).toBe("2026-04-20");
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
      excludedProjectKeys: "pk1",
      excludedAgents: "a1",
      excludedGitBranch: "",
      excludedModels: "m1",
      selectedModels: "m2",
    });
    expect(params).toEqual({
      exclude_project: "p1",
      exclude_agent: "a1",
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
      excludedProjectKeys: "",
      excludedAgents: "",
      excludedGitBranch: "",
      excludedModels: "",
      selectedModels: "",
    });
    expect(params).toEqual({
      from: "2026-01-01",
      to: "2026-01-15",
    });
  });

  it("emits exclude_git_branch for excluded branch tokens", async () => {
    const { buildUsageUrlParams } = await loadStore();
    const tokens = "proj-a\u001fmain\u001eproj-b\u001fdev";
    const params = buildUsageUrlParams({
      from: "",
      to: "",
      isPinned: false,
      windowDays: 30,
      excludedProjects: "",
      excludedAgents: "",
      excludedGitBranch: tokens,
      excludedModels: "",
      selectedModels: "",
    });
    expect(params).toEqual({ exclude_git_branch: tokens });
  });

  it("returns empty object when nothing is set", async () => {
    const { buildUsageUrlParams } = await loadStore();
    const params = buildUsageUrlParams({
      from: "",
      to: "",
      isPinned: false,
      windowDays: 30,
      excludedProjects: "",
      excludedProjectKeys: "",
      excludedAgents: "",
      excludedGitBranch: "",
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
      excludedProjectKeys: "",
      excludedAgents: "",
      excludedGitBranch: "",
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
      excludedProjectKeys: "",
      excludedAgents: "",
      excludedGitBranch: "",
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
      excludedProjectKeys: "",
      excludedAgents: "",
      excludedGitBranch: "",
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
