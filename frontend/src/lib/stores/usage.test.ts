import {
  beforeEach,
  afterEach,
  describe,
  expect,
  it,
  vi,
} from "vitest";

vi.mock("../api/client.js", () => ({
  getUsageSummary: vi.fn().mockResolvedValue({
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
  getUsageTopSessions: vi.fn().mockResolvedValue([]),
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

  it("restores exclude filters from localStorage on load", async () => {
    localStorage.setItem(
      "usage-filters",
      JSON.stringify({
        excludedProjects: "saved-proj",
        excludedModels: "opus",
      }),
    );
    const { usage } = await loadStore();
    expect(usage.excludedProjects).toBe("saved-proj");
    expect(usage.excludedModels).toBe("opus");
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
  it("omits from/to when isPinned is false with default window, includes excludes", async () => {
    const { buildUsageUrlParams } = await loadStore();
    const params = buildUsageUrlParams({
      from: "2026-03-26",
      to: "2026-04-25",
      isPinned: false,
      windowDays: 30,
      excludedProjects: "p1",
      excludedAgents: "a1",
      excludedModels: "m1",
    });
    expect(params).toEqual({
      exclude_project: "p1",
      exclude_agent: "a1",
      exclude_model: "m1",
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
    });
    expect(params).toEqual({
      from: "2026-01-01",
      to: "2026-01-15",
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
