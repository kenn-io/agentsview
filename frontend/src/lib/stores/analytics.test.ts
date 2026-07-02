import {
  describe,
  it,
  expect,
  vi,
  beforeEach,
  afterEach,
} from "vite-plus/test";
import { analytics } from "./analytics.svelte.js";
import { AnalyticsService } from "../api/generated/index";
import { callGenerated } from "../api/runtime.js";
import type {
  AnalyticsSummary,
  ActivityResponse,
  HeatmapResponse,
  ProjectsAnalyticsResponse,
  HourOfWeekResponse,
  SessionShapeResponse,
  VelocityResponse,
  ToolsAnalyticsResponse,
  SkillsAnalyticsResponse,
  TopSessionsResponse,
  SignalsAnalyticsResponse,
} from "../api/types.js";

vi.mock("../api/runtime.js", () => ({
  configureGeneratedClient: vi.fn(),
  callGenerated: vi.fn((request: () => Promise<unknown>) => request()),
  isAbortError: vi.fn(() => false),
}));

vi.mock("../api/generated/index", () => ({
  AnalyticsService: {
    getApiV1AnalyticsSummary: vi.fn(),
    getApiV1AnalyticsActivity: vi.fn(),
    getApiV1AnalyticsHeatmap: vi.fn(),
    getApiV1AnalyticsProjects: vi.fn(),
    getApiV1AnalyticsHourOfWeek: vi.fn(),
    getApiV1AnalyticsSessions: vi.fn(),
    getApiV1AnalyticsVelocity: vi.fn(),
    getApiV1AnalyticsTools: vi.fn(),
    getApiV1AnalyticsSkills: vi.fn(),
    getApiV1AnalyticsTopSessions: vi.fn(),
    getApiV1AnalyticsSignals: vi.fn(),
  },
}));

type MockFn = ReturnType<typeof vi.fn>;

const analyticsService = AnalyticsService as unknown as {
  getApiV1AnalyticsSummary: MockFn;
  getApiV1AnalyticsActivity: MockFn;
  getApiV1AnalyticsHeatmap: MockFn;
  getApiV1AnalyticsProjects: MockFn;
  getApiV1AnalyticsHourOfWeek: MockFn;
  getApiV1AnalyticsSessions: MockFn;
  getApiV1AnalyticsVelocity: MockFn;
  getApiV1AnalyticsTools: MockFn;
  getApiV1AnalyticsSkills: MockFn;
  getApiV1AnalyticsTopSessions: MockFn;
  getApiV1AnalyticsSignals: MockFn;
};

function makeSummary(): AnalyticsSummary {
  return {
    total_sessions: 10,
    total_messages: 100,
    total_output_tokens: 42000,
    token_reporting_sessions: 8,
    models: [],
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

function makeSkills(): SkillsAnalyticsResponse {
  return {
    total_skill_calls: 0,
    distinct_skills: 0,
    by_skill: [],
    trend: [],
  };
}

function makeTopSessions(): TopSessionsResponse {
  return { metric: "messages", sessions: [] };
}

function makeSignals(): SignalsAnalyticsResponse {
  return {
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
    quality_health: {
      computed_sessions: 0,
      totals: {
        short_prompt_count: 0,
        unstructured_start: 0,
        missing_success_criteria_count: 0,
        missing_verification_count: 0,
        duplicate_prompt_count: 0,
        no_code_context_count: 0,
        runaway_tool_loop_count: 0,
        frustration_marker_count: 0,
      },
      sessions_with_signal: {
        short_prompt_count: 0,
        unstructured_start: 0,
        missing_success_criteria_count: 0,
        missing_verification_count: 0,
        duplicate_prompt_count: 0,
        no_code_context_count: 0,
        runaway_tool_loop_count: 0,
        frustration_marker_count: 0,
      },
    },
    trend: [],
    by_agent: [],
    by_project: [],
    calibration: {},
  };
}

function mockAllAPIs() {
  vi.mocked(analyticsService.getApiV1AnalyticsSummary).mockResolvedValue(
    makeSummary(),
  );
  vi.mocked(analyticsService.getApiV1AnalyticsActivity).mockResolvedValue(
    makeActivity(),
  );
  vi.mocked(analyticsService.getApiV1AnalyticsHeatmap).mockResolvedValue(
    makeHeatmap(),
  );
  vi.mocked(analyticsService.getApiV1AnalyticsProjects).mockResolvedValue(
    makeProjects(),
  );
  vi.mocked(analyticsService.getApiV1AnalyticsHourOfWeek).mockResolvedValue(
    makeHourOfWeek(),
  );
  vi.mocked(analyticsService.getApiV1AnalyticsSessions).mockResolvedValue(
    makeSessionShape(),
  );
  vi.mocked(analyticsService.getApiV1AnalyticsVelocity).mockResolvedValue(
    makeVelocity(),
  );
  vi.mocked(analyticsService.getApiV1AnalyticsTools).mockResolvedValue(
    makeTools(),
  );
  vi.mocked(analyticsService.getApiV1AnalyticsSkills).mockResolvedValue(
    makeSkills(),
  );
  vi.mocked(analyticsService.getApiV1AnalyticsTopSessions).mockResolvedValue(
    makeTopSessions(),
  );
  vi.mocked(analyticsService.getApiV1AnalyticsSignals).mockResolvedValue(
    makeSignals(),
  );
}

async function loadAnalyticsStore() {
  vi.resetModules();
  vi.clearAllMocks();
  mockAllAPIs();
  return import("./analytics.svelte.js");
}

function resetStore() {
  analytics.selectedDate = null;
  analytics.selectedDow = null;
  analytics.selectedHour = null;
  analytics.project = "";
  analytics.machine = "";
  analytics.agent = "";
  analytics.includeAutomated = false;
  analytics.automatedScope = "human";
  analytics.model = "";
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
  analytics.skills = null;
  analytics.topSessions = null;
  analytics.signals = null;
  analytics.lastUpdatedAt = null;
  analytics.hasNewData = false;
  analytics.querying = {
    summary: false,
    activity: false,
    heatmap: false,
    projects: false,
    hourOfWeek: false,
    sessionShape: false,
    velocity: false,
    tools: false,
    skills: false,
    topSessions: false,
    signals: false,
  };
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

    expect(analyticsService.getApiV1AnalyticsSummary).toHaveBeenCalledTimes(1);
    expect(analyticsService.getApiV1AnalyticsProjects).toHaveBeenCalledTimes(1);
    expect(analyticsService.getApiV1AnalyticsSessions).toHaveBeenCalledTimes(1);
    expect(analyticsService.getApiV1AnalyticsVelocity).toHaveBeenCalledTimes(1);
    expect(analyticsService.getApiV1AnalyticsTools).toHaveBeenCalledTimes(1);
    expect(analyticsService.getApiV1AnalyticsSkills).toHaveBeenCalledTimes(1);
    expect(analyticsService.getApiV1AnalyticsActivity).not.toHaveBeenCalled();
    expect(analyticsService.getApiV1AnalyticsHeatmap).not.toHaveBeenCalled();
    expect(analyticsService.getApiV1AnalyticsHourOfWeek).not.toHaveBeenCalled();
  });

  it("should pass selected date as from/to for filtered panels", () => {
    analytics.selectDate("2024-01-15");

    expect(analyticsService.getApiV1AnalyticsSummary).toHaveBeenLastCalledWith(
      expect.objectContaining({ from: "2024-01-15", to: "2024-01-15" }),
    );
    expect(analyticsService.getApiV1AnalyticsActivity).not.toHaveBeenCalled();
    expect(analyticsService.getApiV1AnalyticsProjects).toHaveBeenLastCalledWith(
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
    expect(analyticsService.getApiV1AnalyticsSummary).toHaveBeenCalled();
    expect(analyticsService.getApiV1AnalyticsSummary).toHaveBeenLastCalledWith(expected);
    expect(analyticsService.getApiV1AnalyticsActivity).not.toHaveBeenCalled();
    expect(analyticsService.getApiV1AnalyticsProjects).toHaveBeenCalled();
    expect(analyticsService.getApiV1AnalyticsProjects).toHaveBeenLastCalledWith(expected);
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

    expect(analyticsService.getApiV1AnalyticsSummary).toHaveBeenCalledTimes(1);
    expect(analyticsService.getApiV1AnalyticsActivity).toHaveBeenCalledTimes(1);
    expect(analyticsService.getApiV1AnalyticsHeatmap).toHaveBeenCalledTimes(1);
    expect(analyticsService.getApiV1AnalyticsProjects).toHaveBeenCalledTimes(1);
    expect(analyticsService.getApiV1AnalyticsHourOfWeek).toHaveBeenCalledTimes(1);
    expect(analyticsService.getApiV1AnalyticsSessions).toHaveBeenCalledTimes(1);
    expect(analyticsService.getApiV1AnalyticsVelocity).toHaveBeenCalledTimes(1);
    expect(analyticsService.getApiV1AnalyticsTools).toHaveBeenCalledTimes(1);
    expect(analyticsService.getApiV1AnalyticsSkills).toHaveBeenCalledTimes(1);

    const expected = expect.objectContaining({
      from: "2024-02-01", to: "2024-02-28",
    });
    expect(analyticsService.getApiV1AnalyticsSummary).toHaveBeenLastCalledWith(expected);
    expect(analyticsService.getApiV1AnalyticsActivity).toHaveBeenLastCalledWith(expected);
    expect(analyticsService.getApiV1AnalyticsHeatmap).toHaveBeenLastCalledWith(expected);
    expect(analyticsService.getApiV1AnalyticsProjects).toHaveBeenLastCalledWith(expected);
    expect(analyticsService.getApiV1AnalyticsHourOfWeek).toHaveBeenLastCalledWith(expected);
    expect(analyticsService.getApiV1AnalyticsSessions).toHaveBeenLastCalledWith(expected);
    expect(analyticsService.getApiV1AnalyticsVelocity).toHaveBeenLastCalledWith(expected);
    expect(analyticsService.getApiV1AnalyticsTools).toHaveBeenLastCalledWith(expected);
    expect(analyticsService.getApiV1AnalyticsSkills).toHaveBeenLastCalledWith(expected);
  });
});

describe("AnalyticsStore freshness state", () => {
  it("records full refresh time and clears new-data hints", async () => {
    vi.useFakeTimers({ toFake: ["Date"] });
    try {
      vi.setSystemTime(new Date("2026-06-15T15:00:00Z"));

      await analytics.fetchAll();

      expect(analytics.lastUpdatedAt).toBe(
        new Date("2026-06-15T15:00:00Z").getTime(),
      );

      analytics.markNewData();
      expect(analytics.hasNewData).toBe(true);

      vi.setSystemTime(new Date("2026-06-15T15:05:00Z"));
      await analytics.fetchAll();

      expect(analytics.lastUpdatedAt).toBe(
        new Date("2026-06-15T15:05:00Z").getTime(),
      );
      expect(analytics.hasNewData).toBe(false);
    } finally {
      vi.useRealTimers();
    }
  });

  it("does not mark cached partial refresh failures as current", async () => {
    vi.useFakeTimers({ toFake: ["Date"] });
    const warn = vi.spyOn(console, "warn").mockImplementation(() => {});
    try {
      vi.setSystemTime(new Date("2026-06-15T15:00:00Z"));
      await analytics.fetchAll();
      const previousUpdatedAt = analytics.lastUpdatedAt;

      analytics.markNewData();
      vi.mocked(analyticsService.getApiV1AnalyticsVelocity)
        .mockRejectedValueOnce(new Error("velocity failed"));

      vi.setSystemTime(new Date("2026-06-15T15:05:00Z"));
      await analytics.fetchAll();

      expect(analytics.lastUpdatedAt).toBe(previousUpdatedAt);
      expect(analytics.hasNewData).toBe(true);
    } finally {
      warn.mockRestore();
      vi.useRealTimers();
    }
  });
});

describe("AnalyticsStore heatmap uses full range", () => {
  it("should use base from/to for heatmap even with selectedDate", async () => {
    analytics.selectDate("2024-01-15");
    vi.clearAllMocks();

    await analytics.fetchHeatmap();

    expect(analyticsService.getApiV1AnalyticsHeatmap).toHaveBeenLastCalledWith(
      expect.objectContaining({ from: "2024-01-01", to: "2024-01-31" }),
    );
  });
});

describe("AnalyticsStore token metrics", () => {
  it("passes output_tokens heatmap metric through to the API", () => {
    analytics.setMetric("output_tokens");

    expect(analyticsService.getApiV1AnalyticsHeatmap).toHaveBeenLastCalledWith(
      expect.objectContaining({ metric: "output_tokens" }),
    );
  });

  it("passes output_tokens top-session metric through to the API", () => {
    analytics.setTopMetric("output_tokens");

    expect(analyticsService.getApiV1AnalyticsTopSessions).toHaveBeenLastCalledWith(
      expect.objectContaining({ metric: "output_tokens" }),
    );
  });
});

describe("AnalyticsStore activity uses full range", () => {
  it("should use base from/to for activity even with selectedDate", async () => {
    analytics.selectDate("2024-01-15");
    vi.clearAllMocks();

    await analytics.fetchActivity();

    expect(analyticsService.getApiV1AnalyticsActivity).toHaveBeenLastCalledWith(
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
    expect(analyticsService.getApiV1AnalyticsSummary).toHaveBeenCalledTimes(1);
    expect(analyticsService.getApiV1AnalyticsProjects).toHaveBeenCalledTimes(1);
    expect(analyticsService.getApiV1AnalyticsSessions).toHaveBeenCalledTimes(1);
    expect(analyticsService.getApiV1AnalyticsVelocity).toHaveBeenCalledTimes(1);
    expect(analyticsService.getApiV1AnalyticsTools).toHaveBeenCalledTimes(1);
    expect(analyticsService.getApiV1AnalyticsSkills).toHaveBeenCalledTimes(1);
    expect(analyticsService.getApiV1AnalyticsTopSessions).toHaveBeenCalledTimes(1);
    expect(analyticsService.getApiV1AnalyticsActivity).not.toHaveBeenCalled();
    expect(analyticsService.getApiV1AnalyticsHeatmap).not.toHaveBeenCalled();
  });

  it("should use full range after clearing date", () => {
    analytics.selectDate("2024-01-15");
    vi.clearAllMocks();

    analytics.clearDate();

    const expected = expect.objectContaining({
      from: "2024-01-01", to: "2024-01-31",
    });
    expect(analyticsService.getApiV1AnalyticsSummary).toHaveBeenLastCalledWith(expected);
    expect(analyticsService.getApiV1AnalyticsProjects).toHaveBeenLastCalledWith(expected);
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
    { name: "summary", fn: () => analyticsService.getApiV1AnalyticsSummary },
    { name: "activity", fn: () => analyticsService.getApiV1AnalyticsActivity },
    { name: "sessionShape", fn: () => analyticsService.getApiV1AnalyticsSessions },
    { name: "velocity", fn: () => analyticsService.getApiV1AnalyticsVelocity },
    { name: "tools", fn: () => analyticsService.getApiV1AnalyticsTools },
    { name: "skills", fn: () => analyticsService.getApiV1AnalyticsSkills },
    { name: "topSessions", fn: () => analyticsService.getApiV1AnalyticsTopSessions },
  ])(
    "should include project in $name params",
    ({ fn }) => {
      analytics.setProject("alpha");
      const params = vi.mocked(fn()).mock.lastCall?.[0];
      expect(params?.project).toBe("alpha");
    },
  );

  it.each([
    { name: "heatmap", fn: () => analyticsService.getApiV1AnalyticsHeatmap },
    { name: "hourOfWeek", fn: () => analyticsService.getApiV1AnalyticsHourOfWeek },
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
      vi.mocked(analyticsService.getApiV1AnalyticsProjects).mock.lastCall?.[0];
    expect(projectsParams?.project).toBeUndefined();
  });

  it("should exclude project from fetchProjects even with selectedDate", () => {
    analytics.selectDate("2024-01-15");
    vi.clearAllMocks();

    analytics.setProject("alpha");

    const projectsParams =
      vi.mocked(analyticsService.getApiV1AnalyticsProjects).mock.lastCall?.[0];
    expect(projectsParams?.project).toBeUndefined();
    expect(projectsParams?.from).toBe("2024-01-15");
  });

  it.each([
    { name: "summary", fn: () => analyticsService.getApiV1AnalyticsSummary },
    { name: "activity", fn: () => analyticsService.getApiV1AnalyticsActivity },
    { name: "sessionShape", fn: () => analyticsService.getApiV1AnalyticsSessions },
    { name: "velocity", fn: () => analyticsService.getApiV1AnalyticsVelocity },
    { name: "tools", fn: () => analyticsService.getApiV1AnalyticsTools },
    { name: "skills", fn: () => analyticsService.getApiV1AnalyticsSkills },
    { name: "topSessions", fn: () => analyticsService.getApiV1AnalyticsTopSessions },
    { name: "heatmap", fn: () => analyticsService.getApiV1AnalyticsHeatmap },
    { name: "hourOfWeek", fn: () => analyticsService.getApiV1AnalyticsHourOfWeek },
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

describe("AnalyticsStore machine filter", () => {
  it.each([
    { name: "summary", fn: () => analyticsService.getApiV1AnalyticsSummary },
    { name: "activity", fn: () => analyticsService.getApiV1AnalyticsActivity },
    { name: "heatmap", fn: () => analyticsService.getApiV1AnalyticsHeatmap },
    { name: "projects", fn: () => analyticsService.getApiV1AnalyticsProjects },
    { name: "hourOfWeek", fn: () => analyticsService.getApiV1AnalyticsHourOfWeek },
    { name: "sessionShape", fn: () => analyticsService.getApiV1AnalyticsSessions },
    { name: "velocity", fn: () => analyticsService.getApiV1AnalyticsVelocity },
    { name: "tools", fn: () => analyticsService.getApiV1AnalyticsTools },
    { name: "skills", fn: () => analyticsService.getApiV1AnalyticsSkills },
    { name: "topSessions", fn: () => analyticsService.getApiV1AnalyticsTopSessions },
    { name: "signals", fn: () => analyticsService.getApiV1AnalyticsSignals },
  ])("should include machine in $name params", ({ fn }) => {
    analytics.machine = "host-a,host-b";

    analytics.fetchAll();

    const mock = vi.mocked(fn());
    expect(mock).toHaveBeenCalled();
    const params = mock.mock.lastCall?.[0];
    expect(params?.machine).toBe("host-a,host-b");
  });
});

describe("AnalyticsStore automated scope params", () => {
  it("derives all scope from legacy includeAutomated updates", () => {
    analytics.includeAutomated = true;

    analytics.fetchSummary();

    expect(analyticsService.getApiV1AnalyticsSummary).toHaveBeenLastCalledWith(
      expect.objectContaining({ automatedScope: "all" }),
    );
  });

  it("keeps automated-only scope when selected explicitly", () => {
    analytics.setAutomatedScope("automated");

    analytics.fetchSummary();

    expect(analyticsService.getApiV1AnalyticsSummary).toHaveBeenLastCalledWith(
      expect.objectContaining({ automatedScope: "automated" }),
    );
  });
});

describe("AnalyticsStore model filter", () => {
  it.each([
    { name: "summary", fn: () => analyticsService.getApiV1AnalyticsSummary },
    { name: "activity", fn: () => analyticsService.getApiV1AnalyticsActivity },
    { name: "heatmap", fn: () => analyticsService.getApiV1AnalyticsHeatmap },
    { name: "projects", fn: () => analyticsService.getApiV1AnalyticsProjects },
    { name: "hourOfWeek", fn: () => analyticsService.getApiV1AnalyticsHourOfWeek },
    { name: "sessionShape", fn: () => analyticsService.getApiV1AnalyticsSessions },
    { name: "velocity", fn: () => analyticsService.getApiV1AnalyticsVelocity },
    { name: "tools", fn: () => analyticsService.getApiV1AnalyticsTools },
    { name: "skills", fn: () => analyticsService.getApiV1AnalyticsSkills },
    { name: "topSessions", fn: () => analyticsService.getApiV1AnalyticsTopSessions },
    { name: "signals", fn: () => analyticsService.getApiV1AnalyticsSignals },
  ])("should include model in $name params", ({ fn }) => {
    analytics.toggleModel("gpt-4o");

    const mock = vi.mocked(fn());
    expect(mock).toHaveBeenCalled();
    const params = mock.mock.lastCall?.[0];
    expect(params?.model).toBe("gpt-4o");
  });

  it("should clear model from subsequent requests", () => {
    analytics.toggleModel("gpt-4o");
    vi.clearAllMocks();

    analytics.clearModel();

    expect(analytics.model).toBe("");
    const params =
      vi.mocked(analyticsService.getApiV1AnalyticsSummary).mock.lastCall?.[0];
    expect(params?.model).toBeUndefined();
  });

  it("fetchSignalsForInsights omits the model filter", async () => {
    analytics.model = "gpt-4o";
    vi.clearAllMocks();

    await analytics.fetchSignalsForInsights();

    expect(analyticsService.getApiV1AnalyticsSignals).toHaveBeenCalledWith(
      expect.not.objectContaining({ model: expect.anything() }),
    );
    // The Insights page has no model control; the selected model stays put for
    // the Analytics page rather than being cleared by viewing Insights.
    expect(analytics.model).toBe("gpt-4o");
  });

  it("signalEvidenceParams omits the model filter", () => {
    analytics.model = "gpt-4o";
    expect(analytics.signalEvidenceParams().model).toBeUndefined();
  });

  it("drops stale model-scoped signals while an Insights fetch is pending", async () => {
    // Analytics loads model-scoped signals into the shared cache.
    analytics.model = "gpt-4o";
    await analytics.fetchSignals();
    expect(analytics.signals).not.toBeNull();

    // The unmodelled Insights fetch is held in flight.
    let resolve!: (v: SignalsAnalyticsResponse) => void;
    vi.mocked(analyticsService.getApiV1AnalyticsSignals).mockReturnValue(
      new Promise((r) => {
        resolve = r;
      }),
    );

    const pending = analytics.fetchSignalsForInsights();

    // The model-scoped cache is dropped up front, so Insights shows a loading
    // skeleton instead of another scope's signals during the fetch.
    expect(analytics.signals).toBeNull();
    expect(analytics.loading.signals).toBe(true);

    resolve(makeSignals());
    await pending;
    expect(analytics.signals).not.toBeNull();
  });

  it("does not retain stale model-scoped signals when an Insights fetch fails", async () => {
    // Analytics loads model-scoped signals into the shared cache.
    analytics.model = "gpt-4o";
    await analytics.fetchSignals();
    expect(analytics.signals).not.toBeNull();

    // The unmodelled Insights fetch fails.
    vi.mocked(analyticsService.getApiV1AnalyticsSignals).mockRejectedValueOnce(
      new Error("signals failed"),
    );

    await analytics.fetchSignalsForInsights();

    // The wrong-scope cache is cleared and the failure surfaces rather than
    // being swallowed as a cached refetch that keeps stale data.
    expect(analytics.signals).toBeNull();
    expect(analytics.errors.signals).toBe("signals failed");
  });

  it("drops stale drill-down-scoped signals when entering Insights without a model", async () => {
    // Analytics loaded signals under a heatmap drill-down (hour) and no model,
    // so the cache scope differs from Insights only by the drill-down filter.
    analytics.selectedHour = 9;
    await analytics.fetchSignals();
    expect(analytics.signals).not.toBeNull();

    // Insights clears the drill-down; hold the unmodelled fetch in flight.
    let resolve!: (v: SignalsAnalyticsResponse) => void;
    vi.mocked(analyticsService.getApiV1AnalyticsSignals).mockReturnValue(
      new Promise((r) => {
        resolve = r;
      }),
    );

    const pending = analytics.fetchSignalsForInsights();

    // The drill-down-scoped cache is dropped even though no model was set, so
    // Insights does not show another scope's signals while the fetch runs.
    expect(analytics.signals).toBeNull();
    expect(analytics.loading.signals).toBe(true);

    resolve(makeSignals());
    await pending;
    expect(analytics.signals).not.toBeNull();
  });
});

describe("executeFetch concurrency and error handling", () => {
  it("should set loading true during fetch", async () => {
    let resolve!: (v: AnalyticsSummary) => void;
    vi.mocked(analyticsService.getApiV1AnalyticsSummary).mockReturnValue(
      new Promise((r) => { resolve = r; }),
    );

    const p = analytics.fetchSummary();
    expect(analytics.loading.summary).toBe(true);

    resolve(makeSummary());
    await p;
    expect(analytics.loading.summary).toBe(false);
  });

  it("should expose query progress during cached refetches", async () => {
    analytics.summary = makeSummary();
    let resolve!: (v: AnalyticsSummary) => void;
    vi.mocked(analyticsService.getApiV1AnalyticsSummary).mockReturnValue(
      new Promise((r) => { resolve = r; }),
    );

    const p = analytics.fetchSummary();

    expect(analytics.loading.summary).toBe(false);
    expect(analytics.querying.summary).toBe(true);
    expect(analytics.isQuerying).toBe(true);

    resolve(makeSummary());
    await p;

    expect(analytics.querying.summary).toBe(false);
    expect(analytics.isQuerying).toBe(false);
  });

  it("should clear error on new request", async () => {
    vi.mocked(analyticsService.getApiV1AnalyticsSummary)
      .mockRejectedValueOnce(new Error("fail"));
    await analytics.fetchSummary();
    expect(analytics.errors.summary).toBe("fail");

    vi.mocked(analyticsService.getApiV1AnalyticsSummary)
      .mockResolvedValueOnce(makeSummary());
    await analytics.fetchSummary();
    expect(analytics.errors.summary).toBeNull();
  });

  it("should set error message on failure", async () => {
    vi.mocked(analyticsService.getApiV1AnalyticsSummary)
      .mockRejectedValueOnce(new Error("network down"));

    await analytics.fetchSummary();

    expect(analytics.errors.summary).toBe("network down");
    expect(analytics.loading.summary).toBe(false);
  });

  it("should use fallback message for non-Error throws", async () => {
    vi.mocked(analyticsService.getApiV1AnalyticsSummary)
      .mockRejectedValueOnce("string error");

    await analytics.fetchSummary();

    expect(analytics.errors.summary).toBe("Failed to load");
  });

  it("should ignore stale success from superseded request", async () => {
    let resolveFirst!: (v: AnalyticsSummary) => void;
    vi.mocked(analyticsService.getApiV1AnalyticsSummary)
      .mockReturnValueOnce(
        new Promise((r) => { resolveFirst = r; }),
      );

    const firstFetch = analytics.fetchSummary();

    const secondData = makeSummary();
    secondData.total_sessions = 99;
    vi.mocked(analyticsService.getApiV1AnalyticsSummary)
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
    vi.mocked(analyticsService.getApiV1AnalyticsSummary)
      .mockReturnValueOnce(
        new Promise((_r, rej) => { rejectFirst = rej; }),
      );

    const firstFetch = analytics.fetchSummary();

    const data = makeSummary();
    vi.mocked(analyticsService.getApiV1AnalyticsSummary)
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
    vi.mocked(analyticsService.getApiV1AnalyticsSummary)
      .mockReturnValueOnce(
        new Promise((r) => { resolveFirst = r; }),
      );

    const firstFetch = analytics.fetchSummary();

    let resolveSecond!: (v: AnalyticsSummary) => void;
    vi.mocked(analyticsService.getApiV1AnalyticsSummary)
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

  it("aborts stale panel requests when a newer fetch starts", async () => {
    const signals: (AbortSignal | undefined)[] = [];
    vi.mocked(callGenerated).mockImplementation(
      (request: () => Promise<unknown>, signal?: AbortSignal) => {
        signals.push(signal);
        return request();
      },
    );
    vi.mocked(analyticsService.getApiV1AnalyticsSummary)
      .mockImplementationOnce(() => new Promise(() => {}))
      .mockResolvedValueOnce(makeSummary());

    void analytics.fetchSummary();
    await Promise.resolve();
    void analytics.fetchSummary();
    await Promise.resolve();

    expect(signals[0]).toBeDefined();
    expect(signals[0]?.aborted).toBe(true);
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
    expect(analytics.from).toBe("2025-04-26");
    expect(analytics.to).toBe("2026-04-25");
  });

  it("fetchAll re-derives from/to against the current clock while unpinned", async () => {
    const { analytics } = await loadAnalyticsStore();

    expect(analytics.from).toBe("2025-04-26");
    expect(analytics.to).toBe("2026-04-25");

    vi.setSystemTime(new Date("2026-04-26T12:00:00"));
    await analytics.fetchAll();

    expect(analytics.from).toBe("2025-04-27");
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
    expect(analytics.from).toBe("2026-04-19");
    expect(analytics.to).toBe("2026-04-25");
  });

  it("after setRollingWindow, fetchAll keeps rolling", async () => {
    const { analytics } = await loadAnalyticsStore();
    analytics.setRollingWindow(7);
    expect(analytics.from).toBe("2026-04-19");

    vi.setSystemTime(new Date("2026-04-26T12:00:00"));
    await analytics.fetchAll();

    expect(analytics.from).toBe("2026-04-20");
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

  it("fetchSignalsForInsights clears hidden drill-down filters", async () => {
    const { analytics } = await loadAnalyticsStore();
    analytics.from = "2026-04-01";
    analytics.to = "2026-04-30";
    analytics.isPinned = true;
    analytics.selectedDate = "2026-04-12";
    analytics.selectedDow = 2;
    analytics.selectedHour = 16;

    await analytics.fetchSignalsForInsights();

    expect(analytics.selectedDate).toBeNull();
    expect(analytics.selectedDow).toBeNull();
    expect(analytics.selectedHour).toBeNull();
    expect(analyticsService.getApiV1AnalyticsSignals).toHaveBeenCalledWith(
      expect.objectContaining({
        from: "2026-04-01",
        to: "2026-04-30",
      }),
    );
    expect(analyticsService.getApiV1AnalyticsSignals).toHaveBeenCalledWith(
      expect.not.objectContaining({
        dow: expect.anything(),
        hour: expect.anything(),
      }),
    );
  });

  it("fetchSignalsForInsights re-derives rolling dates before fetching", async () => {
    const { analytics } = await loadAnalyticsStore();
    analytics.setRollingWindow(7);
    vi.clearAllMocks();

    vi.setSystemTime(new Date("2026-04-26T12:00:00"));
    await analytics.fetchSignalsForInsights();

    expect(analytics.from).toBe("2026-04-20");
    expect(analytics.to).toBe("2026-04-26");
    expect(analyticsService.getApiV1AnalyticsSignals).toHaveBeenLastCalledWith(
      expect.objectContaining({
        from: "2026-04-20",
        to: "2026-04-26",
      }),
    );
  });
});
