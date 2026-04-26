import {
  describe,
  it,
  expect,
  vi,
  beforeEach,
  afterEach,
} from "vitest";
import { analytics } from "./analytics.svelte.js";
import * as api from "../api/client.js";
import type {
  AnalyticsSummary,
  ActivityResponse,
  HeatmapResponse,
  ProjectsAnalyticsResponse,
  HourOfWeekResponse,
  SessionShapeResponse,
  VelocityResponse,
  ToolsAnalyticsResponse,
  TopSessionsResponse,
} from "../api/types.js";

vi.mock("../api/client.js", () => ({
  getAnalyticsSummary: vi.fn(),
  getAnalyticsActivity: vi.fn(),
  getAnalyticsHeatmap: vi.fn(),
  getAnalyticsProjects: vi.fn(),
  getAnalyticsHourOfWeek: vi.fn(),
  getAnalyticsSessionShape: vi.fn(),
  getAnalyticsVelocity: vi.fn(),
  getAnalyticsTools: vi.fn(),
  getAnalyticsTopSessions: vi.fn(),
  getAnalyticsSignals: vi.fn(),
}));


function makeSummary(): AnalyticsSummary {
  return {
    total_sessions: 10,
    total_messages: 100,
    total_output_tokens: 42000,
    token_reporting_sessions: 8,
    active_projects: 3,
    active_days: 5,
    avg_messages: 10,
    median_messages: 8,
    p90_messages: 20,
    most_active_project: "proj",
    concentration: 0.5,
    agents: {},
  };
}

function makeActivity(): ActivityResponse {
  return { granularity: "day", series: [] };
}

function makeHeatmap(): HeatmapResponse {
  return {
    metric: "messages",
    entries: [],
    levels: { l1: 1, l2: 5, l3: 10, l4: 20 },
    entries_from: "2024-01-01",
  };
}

function makeProjects(): ProjectsAnalyticsResponse {
  return { projects: [] };
}

function makeHourOfWeek(): HourOfWeekResponse {
  return { cells: [] };
}

function makeSessionShape(): SessionShapeResponse {
  return {
    count: 0,
    length_distribution: [],
    duration_distribution: [],
    autonomy_distribution: [],
  };
}

function makeVelocity(): VelocityResponse {
  return {
    overall: {
      turn_cycle_sec: { p50: 0, p90: 0 },
      first_response_sec: { p50: 0, p90: 0 },
      msgs_per_active_min: 0,
      chars_per_active_min: 0,
      tool_calls_per_active_min: 0,
    },
    by_agent: [],
    by_complexity: [],
  };
}

function makeTools(): ToolsAnalyticsResponse {
  return {
    total_calls: 0,
    by_category: [],
    by_agent: [],
    trend: [],
  };
}

function makeTopSessions(): TopSessionsResponse {
  return { metric: "messages", sessions: [] };
}

function mockAllAPIs() {
  vi.mocked(api.getAnalyticsSummary).mockResolvedValue(
    makeSummary(),
  );
  vi.mocked(api.getAnalyticsActivity).mockResolvedValue(
    makeActivity(),
  );
  vi.mocked(api.getAnalyticsHeatmap).mockResolvedValue(
    makeHeatmap(),
  );
  vi.mocked(api.getAnalyticsProjects).mockResolvedValue(
    makeProjects(),
  );
  vi.mocked(api.getAnalyticsHourOfWeek).mockResolvedValue(
    makeHourOfWeek(),
  );
  vi.mocked(api.getAnalyticsSessionShape).mockResolvedValue(
    makeSessionShape(),
  );
  vi.mocked(api.getAnalyticsVelocity).mockResolvedValue(
    makeVelocity(),
  );
  vi.mocked(api.getAnalyticsTools).mockResolvedValue(
    makeTools(),
  );
  vi.mocked(api.getAnalyticsTopSessions).mockResolvedValue(
    makeTopSessions(),
  );
  vi.mocked(api.getAnalyticsSignals).mockResolvedValue({
    scored_sessions: 0,
    unscored_sessions: 0,
    grade_distribution: {},
    avg_health_score: null,
    outcome_distribution: {},
    outcome_confidence_distribution: {},
    tool_health: {
      total_failure_signals: 0,
      total_retries: 0,
      total_edit_churn: 0,
      sessions_with_failures: 0,
      failure_rate: 0,
    },
    context_health: {
      avg_compaction_count: 0,
      sessions_with_compaction: 0,
      mid_task_compaction_count: 0,
      sessions_with_mid_task_compaction: 0,
      sessions_with_context_data: 0,
      avg_context_pressure: null,
      high_pressure_sessions: 0,
    },
    trend: [],
    by_agent: [],
    by_project: [],
  });
}

async function loadAnalyticsStore() {
  vi.resetModules();
  vi.clearAllMocks();
  mockAllAPIs();
  return import("./analytics.svelte.js");
}

function resetStore() {
  analytics.selectedDate = null;
  analytics.project = "";
  analytics.from = "2024-01-01";
  analytics.to = "2024-01-31";
  analytics.isPinned = false;
  analytics.windowDays = 365;
  // Clear cached data fields so each test starts from a clean
  // "no data" state. Prior tests leave the singleton populated,
  // which breaks assertions like `loading === true during fetch`
  // now that loading is only flipped on first-load (no existing
  // data) rather than every refetch.
  analytics.summary = null;
  analytics.activity = null;
  analytics.heatmap = null;
  analytics.projects = null;
  analytics.hourOfWeek = null;
  analytics.sessionShape = null;
  analytics.velocity = null;
  analytics.tools = null;
  analytics.topSessions = null;
  analytics.signals = null;
}

// Note: selectDate and setDateRange invoke API mocks
// synchronously (the mock call is recorded before the first
// await inside fetchSummary/etc.), so no async flushing is
// needed for call-count or call-param assertions.

beforeEach(() => {
  resetStore();
  vi.clearAllMocks();
  mockAllAPIs();
});

describe("AnalyticsStore.selectDate", () => {
  it("should set selectedDate on first click", () => {
    analytics.selectDate("2024-01-15");
    expect(analytics.selectedDate).toBe("2024-01-15");
  });

  it("should deselect when clicking the same date", () => {
    analytics.selectDate("2024-01-15");
    analytics.selectDate("2024-01-15");
    expect(analytics.selectedDate).toBeNull();
  });

  it("should switch to a different date", () => {
    analytics.selectDate("2024-01-15");
    analytics.selectDate("2024-01-20");
    expect(analytics.selectedDate).toBe("2024-01-20");
  });

  it("should fetch filtered panels but not activity/heatmap/hourOfWeek", () => {
    analytics.selectDate("2024-01-15");

    expect(api.getAnalyticsSummary).toHaveBeenCalledTimes(1);
    expect(api.getAnalyticsProjects).toHaveBeenCalledTimes(1);
    expect(api.getAnalyticsSessionShape).toHaveBeenCalledTimes(1);
    expect(api.getAnalyticsVelocity).toHaveBeenCalledTimes(1);
    expect(api.getAnalyticsTools).toHaveBeenCalledTimes(1);
    expect(api.getAnalyticsActivity).not.toHaveBeenCalled();
    expect(api.getAnalyticsHeatmap).not.toHaveBeenCalled();
    expect(api.getAnalyticsHourOfWeek).not.toHaveBeenCalled();
  });

  it("should pass selected date as from/to for filtered panels", () => {
    analytics.selectDate("2024-01-15");

    expect(api.getAnalyticsSummary).toHaveBeenLastCalledWith(
      expect.objectContaining({ from: "2024-01-15", to: "2024-01-15" }),
    );
    expect(api.getAnalyticsActivity).not.toHaveBeenCalled();
    expect(api.getAnalyticsProjects).toHaveBeenLastCalledWith(
      expect.objectContaining({ from: "2024-01-15", to: "2024-01-15" }),
    );
  });

  it("should use full range after deselecting", () => {
    analytics.selectDate("2024-01-15");
    vi.clearAllMocks();

    analytics.selectDate("2024-01-15"); // deselect

    const expected = expect.objectContaining({
      from: "2024-01-01", to: "2024-01-31",
    });
    expect(api.getAnalyticsSummary).toHaveBeenCalled();
    expect(api.getAnalyticsSummary).toHaveBeenLastCalledWith(expected);
    expect(api.getAnalyticsActivity).not.toHaveBeenCalled();
    expect(api.getAnalyticsProjects).toHaveBeenCalled();
    expect(api.getAnalyticsProjects).toHaveBeenLastCalledWith(expected);
  });
});

describe("AnalyticsStore.setDateRange", () => {
  it("should clear selectedDate", () => {
    analytics.selectDate("2024-01-15");
    analytics.setDateRange("2024-02-01", "2024-02-28");
    expect(analytics.selectedDate).toBeNull();
  });

  it("should fetch all panels with new range params", () => {
    analytics.setDateRange("2024-02-01", "2024-02-28");

    expect(api.getAnalyticsSummary).toHaveBeenCalledTimes(1);
    expect(api.getAnalyticsActivity).toHaveBeenCalledTimes(1);
    expect(api.getAnalyticsHeatmap).toHaveBeenCalledTimes(1);
    expect(api.getAnalyticsProjects).toHaveBeenCalledTimes(1);
    expect(api.getAnalyticsHourOfWeek).toHaveBeenCalledTimes(1);
    expect(api.getAnalyticsSessionShape).toHaveBeenCalledTimes(1);
    expect(api.getAnalyticsVelocity).toHaveBeenCalledTimes(1);
    expect(api.getAnalyticsTools).toHaveBeenCalledTimes(1);

    const expected = expect.objectContaining({
      from: "2024-02-01", to: "2024-02-28",
    });
    expect(api.getAnalyticsSummary).toHaveBeenLastCalledWith(expected);
    expect(api.getAnalyticsActivity).toHaveBeenLastCalledWith(expected);
    expect(api.getAnalyticsHeatmap).toHaveBeenLastCalledWith(expected);
    expect(api.getAnalyticsProjects).toHaveBeenLastCalledWith(expected);
    expect(api.getAnalyticsHourOfWeek).toHaveBeenLastCalledWith(expected);
    expect(api.getAnalyticsSessionShape).toHaveBeenLastCalledWith(expected);
    expect(api.getAnalyticsVelocity).toHaveBeenLastCalledWith(expected);
    expect(api.getAnalyticsTools).toHaveBeenLastCalledWith(expected);
  });
});

describe("AnalyticsStore heatmap uses full range", () => {
  it("should use base from/to for heatmap even with selectedDate", async () => {
    analytics.selectDate("2024-01-15");
    vi.clearAllMocks();

    await analytics.fetchHeatmap();

    expect(api.getAnalyticsHeatmap).toHaveBeenLastCalledWith(
      expect.objectContaining({ from: "2024-01-01", to: "2024-01-31" }),
    );
  });
});

describe("AnalyticsStore token metrics", () => {
  it("passes output_tokens heatmap metric through to the API", () => {
    analytics.setMetric("output_tokens");

    expect(api.getAnalyticsHeatmap).toHaveBeenLastCalledWith(
      expect.objectContaining({ metric: "output_tokens" }),
    );
  });

  it("passes output_tokens top-session metric through to the API", () => {
    analytics.setTopMetric("output_tokens");

    expect(api.getAnalyticsTopSessions).toHaveBeenLastCalledWith(
      expect.objectContaining({ metric: "output_tokens" }),
    );
  });
});

describe("AnalyticsStore activity uses full range", () => {
  it("should use base from/to for activity even with selectedDate", async () => {
    analytics.selectDate("2024-01-15");
    vi.clearAllMocks();

    await analytics.fetchActivity();

    expect(api.getAnalyticsActivity).toHaveBeenLastCalledWith(
      expect.objectContaining({ from: "2024-01-01", to: "2024-01-31" }),
    );
  });
});

describe("AnalyticsStore.clearDate", () => {
  it("should clear selectedDate and fetch filtered panels", () => {
    analytics.selectDate("2024-01-15");
    vi.clearAllMocks();

    analytics.clearDate();

    expect(analytics.selectedDate).toBeNull();
    expect(api.getAnalyticsSummary).toHaveBeenCalledTimes(1);
    expect(api.getAnalyticsProjects).toHaveBeenCalledTimes(1);
    expect(api.getAnalyticsSessionShape).toHaveBeenCalledTimes(1);
    expect(api.getAnalyticsVelocity).toHaveBeenCalledTimes(1);
    expect(api.getAnalyticsTools).toHaveBeenCalledTimes(1);
    expect(api.getAnalyticsTopSessions).toHaveBeenCalledTimes(1);
    expect(api.getAnalyticsActivity).not.toHaveBeenCalled();
    expect(api.getAnalyticsHeatmap).not.toHaveBeenCalled();
  });

  it("should use full range after clearing date", () => {
    analytics.selectDate("2024-01-15");
    vi.clearAllMocks();

    analytics.clearDate();

    const expected = expect.objectContaining({
      from: "2024-01-01", to: "2024-01-31",
    });
    expect(api.getAnalyticsSummary).toHaveBeenLastCalledWith(expected);
    expect(api.getAnalyticsProjects).toHaveBeenLastCalledWith(expected);
  });
});

describe("AnalyticsStore.setProject", () => {
  it("should toggle project on first click", () => {
    analytics.setProject("alpha");
    expect(analytics.project).toBe("alpha");
  });

  it("should clear project when clicking same project", () => {
    analytics.setProject("alpha");
    analytics.setProject("alpha");
    expect(analytics.project).toBe("");
  });

  it("should switch to different project", () => {
    analytics.setProject("alpha");
    analytics.setProject("beta");
    expect(analytics.project).toBe("beta");
  });

  it.each([
    { name: "summary", fn: () => api.getAnalyticsSummary },
    { name: "activity", fn: () => api.getAnalyticsActivity },
    { name: "sessionShape", fn: () => api.getAnalyticsSessionShape },
    { name: "velocity", fn: () => api.getAnalyticsVelocity },
    { name: "tools", fn: () => api.getAnalyticsTools },
    { name: "topSessions", fn: () => api.getAnalyticsTopSessions },
  ])(
    "should include project in $name params",
    ({ fn }) => {
      analytics.setProject("alpha");
      const params = vi.mocked(fn()).mock.lastCall?.[0];
      expect(params?.project).toBe("alpha");
    },
  );

  it.each([
    { name: "heatmap", fn: () => api.getAnalyticsHeatmap },
    { name: "hourOfWeek", fn: () => api.getAnalyticsHourOfWeek },
  ])(
    "should include project in $name base params",
    ({ fn }) => {
      analytics.setProject("alpha");
      const params = vi.mocked(fn()).mock.lastCall?.[0];
      expect(params?.project).toBe("alpha");
    },
  );

  it("should exclude project from fetchProjects params", () => {
    analytics.setProject("alpha");

    const projectsParams =
      vi.mocked(api.getAnalyticsProjects).mock.lastCall?.[0];
    expect(projectsParams?.project).toBeUndefined();
  });

  it("should exclude project from fetchProjects even with selectedDate", () => {
    analytics.selectDate("2024-01-15");
    vi.clearAllMocks();

    analytics.setProject("alpha");

    const projectsParams =
      vi.mocked(api.getAnalyticsProjects).mock.lastCall?.[0];
    expect(projectsParams?.project).toBeUndefined();
    expect(projectsParams?.from).toBe("2024-01-15");
  });

  it.each([
    { name: "summary", fn: () => api.getAnalyticsSummary },
    { name: "activity", fn: () => api.getAnalyticsActivity },
    { name: "sessionShape", fn: () => api.getAnalyticsSessionShape },
    { name: "velocity", fn: () => api.getAnalyticsVelocity },
    { name: "tools", fn: () => api.getAnalyticsTools },
    { name: "topSessions", fn: () => api.getAnalyticsTopSessions },
    { name: "heatmap", fn: () => api.getAnalyticsHeatmap },
    { name: "hourOfWeek", fn: () => api.getAnalyticsHourOfWeek },
  ])(
    "should clear project from $name params after deselecting",
    ({ fn }) => {
      analytics.setProject("alpha");
      vi.clearAllMocks();

      analytics.setProject("alpha"); // deselect

      const mock = vi.mocked(fn());
      expect(mock).toHaveBeenCalled();
      const params = mock.mock.lastCall?.[0];
      expect(params?.project).toBeUndefined();
    },
  );
});

describe("executeFetch concurrency and error handling", () => {
  it("should set loading true during fetch", async () => {
    let resolve!: (v: AnalyticsSummary) => void;
    vi.mocked(api.getAnalyticsSummary).mockReturnValue(
      new Promise((r) => { resolve = r; }),
    );

    const p = analytics.fetchSummary();
    expect(analytics.loading.summary).toBe(true);

    resolve(makeSummary());
    await p;
    expect(analytics.loading.summary).toBe(false);
  });

  it("should clear error on new request", async () => {
    vi.mocked(api.getAnalyticsSummary)
      .mockRejectedValueOnce(new Error("fail"));
    await analytics.fetchSummary();
    expect(analytics.errors.summary).toBe("fail");

    vi.mocked(api.getAnalyticsSummary)
      .mockResolvedValueOnce(makeSummary());
    await analytics.fetchSummary();
    expect(analytics.errors.summary).toBeNull();
  });

  it("should set error message on failure", async () => {
    vi.mocked(api.getAnalyticsSummary)
      .mockRejectedValueOnce(new Error("network down"));

    await analytics.fetchSummary();

    expect(analytics.errors.summary).toBe("network down");
    expect(analytics.loading.summary).toBe(false);
  });

  it("should use fallback message for non-Error throws", async () => {
    vi.mocked(api.getAnalyticsSummary)
      .mockRejectedValueOnce("string error");

    await analytics.fetchSummary();

    expect(analytics.errors.summary).toBe("Failed to load");
  });

  it("should ignore stale success from superseded request", async () => {
    let resolveFirst!: (v: AnalyticsSummary) => void;
    vi.mocked(api.getAnalyticsSummary)
      .mockReturnValueOnce(
        new Promise((r) => { resolveFirst = r; }),
      );

    const firstFetch = analytics.fetchSummary();

    const secondData = makeSummary();
    secondData.total_sessions = 99;
    vi.mocked(api.getAnalyticsSummary)
      .mockResolvedValueOnce(secondData);
    const secondFetch = analytics.fetchSummary();

    await secondFetch;
    expect(analytics.summary?.total_sessions).toBe(99);

    const staleData = makeSummary();
    staleData.total_sessions = 1;
    resolveFirst(staleData);
    await firstFetch;

    expect(analytics.summary?.total_sessions).toBe(99);
  });

  it("should ignore stale error from superseded request", async () => {
    let rejectFirst!: (e: Error) => void;
    vi.mocked(api.getAnalyticsSummary)
      .mockReturnValueOnce(
        new Promise((_r, rej) => { rejectFirst = rej; }),
      );

    const firstFetch = analytics.fetchSummary();

    const data = makeSummary();
    vi.mocked(api.getAnalyticsSummary)
      .mockResolvedValueOnce(data);
    const secondFetch = analytics.fetchSummary();
    await secondFetch;

    expect(analytics.errors.summary).toBeNull();
    expect(analytics.summary).toStrictEqual(data);

    rejectFirst(new Error("stale error"));
    await firstFetch;

    expect(analytics.errors.summary).toBeNull();
    expect(analytics.summary).toStrictEqual(data);
  });

  it("should not clear loading for superseded request", async () => {
    let resolveFirst!: (v: AnalyticsSummary) => void;
    vi.mocked(api.getAnalyticsSummary)
      .mockReturnValueOnce(
        new Promise((r) => { resolveFirst = r; }),
      );

    const firstFetch = analytics.fetchSummary();

    let resolveSecond!: (v: AnalyticsSummary) => void;
    vi.mocked(api.getAnalyticsSummary)
      .mockReturnValueOnce(
        new Promise((r) => { resolveSecond = r; }),
      );
    const secondFetch = analytics.fetchSummary();

    expect(analytics.loading.summary).toBe(true);

    resolveFirst(makeSummary());
    await firstFetch;

    // Loading should still be true because second is pending
    expect(analytics.loading.summary).toBe(true);

    resolveSecond(makeSummary());
    await secondFetch;
    expect(analytics.loading.summary).toBe(false);
  });
});

describe("AnalyticsStore rolling default date range", () => {
  beforeEach(() => {
    vi.useFakeTimers({ toFake: ["Date"] });
    vi.setSystemTime(new Date("2026-04-25T12:00:00"));
  });

  afterEach(() => {
    vi.useRealTimers();
  });

  it("constructor produces isPinned=false and windowDays=365", async () => {
    const { analytics } = await loadAnalyticsStore();
    expect(analytics.isPinned).toBe(false);
    expect(analytics.windowDays).toBe(365);
    expect(analytics.from).toBe("2025-04-25");
    expect(analytics.to).toBe("2026-04-25");
  });

  it("fetchAll re-derives from/to against the current clock while unpinned", async () => {
    const { analytics } = await loadAnalyticsStore();

    expect(analytics.from).toBe("2025-04-25");
    expect(analytics.to).toBe("2026-04-25");

    vi.setSystemTime(new Date("2026-04-26T12:00:00"));
    await analytics.fetchAll();

    expect(analytics.from).toBe("2025-04-26");
    expect(analytics.to).toBe("2026-04-26");
  });

  it("setDateRange pins and subsequent fetchAll does not roll", async () => {
    const { analytics } = await loadAnalyticsStore();
    analytics.setDateRange("2026-01-01", "2026-01-15");
    expect(analytics.isPinned).toBe(true);
    expect(analytics.from).toBe("2026-01-01");
    expect(analytics.to).toBe("2026-01-15");

    vi.setSystemTime(new Date("2026-04-26T12:00:00"));
    await analytics.fetchAll();

    expect(analytics.isPinned).toBe(true);
    expect(analytics.from).toBe("2026-01-01");
    expect(analytics.to).toBe("2026-01-15");
  });

  it("setRollingWindow sets windowDays, clears the pin, and re-derives dates", async () => {
    const { analytics } = await loadAnalyticsStore();
    analytics.setDateRange("2026-01-01", "2026-01-15");
    expect(analytics.isPinned).toBe(true);

    analytics.setRollingWindow(7);

    expect(analytics.isPinned).toBe(false);
    expect(analytics.windowDays).toBe(7);
    expect(analytics.from).toBe("2026-04-18");
    expect(analytics.to).toBe("2026-04-25");
  });

  it("after setRollingWindow, fetchAll keeps rolling", async () => {
    const { analytics } = await loadAnalyticsStore();
    analytics.setRollingWindow(7);
    expect(analytics.from).toBe("2026-04-18");

    vi.setSystemTime(new Date("2026-04-26T12:00:00"));
    await analytics.fetchAll();

    expect(analytics.from).toBe("2026-04-19");
    expect(analytics.to).toBe("2026-04-26");
  });

  it("setRollingWindow clears any active drill-down (selectedDate/Dow/Hour)", async () => {
    const { analytics } = await loadAnalyticsStore();
    analytics.selectedDate = "2026-04-20";
    analytics.selectedDow = 3;
    analytics.selectedHour = 14;

    analytics.setRollingWindow(7);

    expect(analytics.selectedDate).toBeNull();
    expect(analytics.selectedDow).toBeNull();
    expect(analytics.selectedHour).toBeNull();
  });
});
