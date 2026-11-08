import { describe, it, expect, vi, beforeEach, afterEach } from "vite-plus/test";
import { ApiError, attachResponseTiming } from "../api/runtime.js";
import { analytics } from "./analytics.svelte.js";
import {
  makeSummary,
  makeActivity,
  makeHeatmap,
  makeProjects,
  makeHourOfWeek,
  makeSessionShape,
  makeVelocity,
  makeTools,
  makeSkills,
  makeTopSessions,
  makeSignals,
} from "../test-support/analytics.js";
import { sessions } from "./sessions.svelte.js";
import { AnalyticsService } from "../api/generated/index";

import type {
  AnalyticsSummaryResponse as AnalyticsSummary,
  DbToolsAnalyticsResponse as ToolsAnalyticsResponse,
  DbSkillsAnalyticsResponse as SkillsAnalyticsResponse,
  DbSignalsAnalyticsResponse as SignalsAnalyticsResponse,
} from "../api/generated/index.js";

vi.mock("../api/runtime.js", async (importOriginal) => ({
  ...(await importOriginal<typeof import("../api/runtime.js")>()),
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

function mockAllAPIs() {
  vi.mocked(analyticsService.getApiV1AnalyticsSummary).mockResolvedValue(makeSummary());
  vi.mocked(analyticsService.getApiV1AnalyticsActivity).mockResolvedValue(makeActivity());
  vi.mocked(analyticsService.getApiV1AnalyticsHeatmap).mockResolvedValue(makeHeatmap());
  vi.mocked(analyticsService.getApiV1AnalyticsProjects).mockResolvedValue(makeProjects());
  vi.mocked(analyticsService.getApiV1AnalyticsHourOfWeek).mockResolvedValue(makeHourOfWeek());
  vi.mocked(analyticsService.getApiV1AnalyticsSessions).mockResolvedValue(makeSessionShape());
  vi.mocked(analyticsService.getApiV1AnalyticsVelocity).mockResolvedValue(makeVelocity());
  vi.mocked(analyticsService.getApiV1AnalyticsTools).mockResolvedValue(makeTools());
  vi.mocked(analyticsService.getApiV1AnalyticsSkills).mockResolvedValue(makeSkills());
  vi.mocked(analyticsService.getApiV1AnalyticsTopSessions).mockResolvedValue(makeTopSessions());
  vi.mocked(analyticsService.getApiV1AnalyticsSignals).mockResolvedValue(makeSignals());
}

async function loadAnalyticsStore() {
  vi.resetModules();
  vi.clearAllMocks();
  mockAllAPIs();
  return import("./analytics.svelte.js");
}

function resetStore() {
  analytics.cancelInFlightReads();
  Object.assign(analytics, {
    skillsRequestGranularity: "week",
    scopes: {},
    loadedModelAliases: {},
  });
  for (const panel of Object.keys(analytics.errors) as (keyof typeof analytics.errors)[]) {
    analytics.errors[panel] = null;
    analytics.loading[panel] = false;
  }
  analytics.selectedDate = null;
  analytics.selectedActivityRange = null;
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
  analytics.skillsGranularity = "week";
  // Clear cached data fields so each test starts from a clean
  // "no data" state. Prior tests leave the singleton populated,
  // which breaks assertions like `loading === true during fetch`
  // now that loading is only flipped on first-load (no existing
  // data) rather than every refetch.
  analytics.summary = null;
  analytics.summaryFilterParams = null;
  analytics.activity = null;
  analytics.heatmap = null;
  analytics.projects = null;
  analytics.hourOfWeek = null;
  analytics.sessionShape = null;
  analytics.velocity = null;
  analytics.tools = null;
  analytics.toolsFilterParams = null;
  analytics.skills = null;
  analytics.topSessions = null;
  analytics.signals = null;
  analytics.lastUpdatedAt = null;
  analytics.qualityLastUpdatedAt = null;
  analytics.lastQueryDurationMs = null;
  analytics.qualityLastQueryDurationMs = null;
  analytics.lastQuerySteps = [];
  analytics.qualityLastQuerySteps = [];
  analytics.hasNewData = false;
  sessions.filters.date = "";
  sessions.filters.dateFrom = "";
  sessions.filters.dateTo = "";
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

  it("keeps parent hour-of-week data through selecting and clearing a date", async () => {
    await analytics.fetchHourOfWeek();
    const parent = analytics.hourOfWeek;

    analytics.selectDate("2024-01-15");

    expect(analytics.hourOfWeek).toEqual(parent);
    analytics.clearDate();
    expect(analytics.hourOfWeek).toEqual(parent);
  });

  it("should pass selected date as from/to for filtered panels", () => {
    analytics.selectDate("2024-01-15");

    expect(analyticsService.getApiV1AnalyticsSummary.mock.lastCall?.[0]).toEqual(
      expect.objectContaining({ from: "2024-01-15", to: "2024-01-15" }),
    );
    expect(analyticsService.getApiV1AnalyticsActivity).not.toHaveBeenCalled();
    expect(analyticsService.getApiV1AnalyticsProjects.mock.lastCall?.[0]).toEqual(
      expect.objectContaining({ from: "2024-01-15", to: "2024-01-15" }),
    );
  });

  it("should use full range after deselecting", () => {
    analytics.selectDate("2024-01-15");
    vi.clearAllMocks();

    analytics.selectDate("2024-01-15"); // deselect

    const expected = expect.objectContaining({
      from: "2024-01-01",
      to: "2024-01-31",
    });
    expect(analyticsService.getApiV1AnalyticsSummary).toHaveBeenCalled();
    expect(analyticsService.getApiV1AnalyticsSummary.mock.lastCall?.[0]).toEqual(expected);
    expect(analyticsService.getApiV1AnalyticsActivity).not.toHaveBeenCalled();
    expect(analyticsService.getApiV1AnalyticsProjects).toHaveBeenCalled();
    expect(analyticsService.getApiV1AnalyticsProjects.mock.lastCall?.[0]).toEqual(expected);
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
      from: "2024-02-01",
      to: "2024-02-28",
    });
    expect(analyticsService.getApiV1AnalyticsSummary.mock.lastCall?.[0]).toEqual(expected);
    expect(analyticsService.getApiV1AnalyticsActivity.mock.lastCall?.[0]).toEqual(expected);
    expect(analyticsService.getApiV1AnalyticsHeatmap.mock.lastCall?.[0]).toEqual(expected);
    expect(analyticsService.getApiV1AnalyticsProjects.mock.lastCall?.[0]).toEqual(expected);
    expect(analyticsService.getApiV1AnalyticsHourOfWeek.mock.lastCall?.[0]).toEqual(expected);
    expect(analyticsService.getApiV1AnalyticsSessions.mock.lastCall?.[0]).toEqual(expected);
    expect(analyticsService.getApiV1AnalyticsVelocity.mock.lastCall?.[0]).toEqual(expected);
    expect(analyticsService.getApiV1AnalyticsTools.mock.lastCall?.[0]).toEqual(expected);
    expect(analyticsService.getApiV1AnalyticsSkills.mock.lastCall?.[0]).toEqual(expected);
  });
});

describe("AnalyticsStore.setSkillsGranularity", () => {
  it("applies the new granularity only after its response arrives", async () => {
    await analytics.fetchSkills("week");
    let resolve!: (value: SkillsAnalyticsResponse) => void;
    vi.mocked(analyticsService.getApiV1AnalyticsSkills).mockReturnValueOnce(
      new Promise((r) => {
        resolve = r;
      }),
    );

    const pending = analytics.setSkillsGranularity("month");

    expect(analytics.skillsGranularity).toBe("week");
    expect(analytics.querying.skills).toBe(true);
    expect(analyticsService.getApiV1AnalyticsSkills.mock.lastCall?.[0]).toEqual(
      expect.objectContaining({ granularity: "month" }),
    );

    resolve(makeSkills());
    await pending;

    expect(analytics.skillsGranularity).toBe("month");
    expect(analytics.querying.skills).toBe(false);
  });

  it("keeps the applied grouping and permits retrying or returning after failure", async () => {
    await analytics.fetchSkills("week");
    vi.mocked(analyticsService.getApiV1AnalyticsSkills).mockRejectedValueOnce(
      new Error("network down"),
    );

    const result = await analytics.setSkillsGranularity("month");

    expect(result).toBe("error");
    expect(analytics.skillsGranularity).toBe("week");
    expect(analytics.requestedSkillsGranularity).toBe("month");
    vi.mocked(analyticsService.getApiV1AnalyticsSkills).mockRejectedValueOnce(
      new Error("still unavailable"),
    );
    await analytics.fetchSkills();
    expect(analyticsService.getApiV1AnalyticsSkills.mock.lastCall?.[0]?.granularity).toBe("month");
    await analytics.setSkillsGranularity("week");
    expect(analyticsService.getApiV1AnalyticsSkills.mock.lastCall?.[0]?.granularity).toBe("week");
    expect(analytics.requestedSkillsGranularity).toBe("week");
  });
});

describe("AnalyticsStore freshness state", () => {
  it("records Quality freshness without changing dashboard freshness", async () => {
    vi.useFakeTimers({ toFake: ["Date"] });
    try {
      analytics.lastUpdatedAt = new Date("2026-06-15T14:55:00Z").getTime();
      analytics.hasNewData = true;
      vi.setSystemTime(new Date("2026-06-15T15:00:00Z"));

      await analytics.fetchSignalsForQuality();

      expect(analytics.qualityLastUpdatedAt).toBe(new Date("2026-06-15T15:00:00Z").getTime());
      expect(analytics.lastUpdatedAt).toBe(new Date("2026-06-15T14:55:00Z").getTime());
      expect(analytics.hasNewData).toBe(true);
    } finally {
      vi.useRealTimers();
    }
  });

  it("records full refresh time and clears new-data hints", async () => {
    vi.useFakeTimers({ toFake: ["Date"] });
    try {
      vi.setSystemTime(new Date("2026-06-15T15:00:00Z"));

      await analytics.fetchAll();

      expect(analytics.lastUpdatedAt).toBe(new Date("2026-06-15T15:00:00Z").getTime());

      analytics.markNewData();
      expect(analytics.hasNewData).toBe(true);

      vi.setSystemTime(new Date("2026-06-15T15:05:00Z"));
      await analytics.fetchAll();

      expect(analytics.lastUpdatedAt).toBe(new Date("2026-06-15T15:05:00Z").getTime());
      expect(analytics.hasNewData).toBe(false);
    } finally {
      vi.useRealTimers();
    }
  });

  it("records dashboard and Quality query durations separately", async () => {
    vi.useFakeTimers({ toFake: ["Date", "performance"] });
    try {
      expect(analytics.lastQueryDurationMs).toBeNull();
      expect(analytics.qualityLastQueryDurationMs).toBeNull();

      vi.mocked(analyticsService.getApiV1AnalyticsVelocity).mockImplementationOnce(async () => {
        const sentAt = performance.now();
        vi.advanceTimersByTime(600);
        const headersAt = performance.now();
        vi.advanceTimersByTime(100);
        const data = makeVelocity();
        attachResponseTiming(data, { sentAt, headersAt, bodyAt: performance.now() });
        return data;
      });
      await analytics.fetchAll();
      expect(analytics.lastQueryDurationMs).toBe(700);
      expect(analytics.qualityLastQueryDurationMs).toBeNull();
      // One step per panel, in execution order, each with its own timing.
      expect(analytics.lastQuerySteps.map((step) => step.name)).toEqual([
        "summary",
        "activity",
        "heatmap",
        "projects",
        "hourOfWeek",
        "sessionShape",
        "velocity",
        "tools",
        "skills",
        "topSessions",
        "signals",
      ]);
      // The velocity request carried phase timings: 600 ms waiting on the
      // server, 100 ms downloading, applied at once.
      expect(analytics.lastQuerySteps).toContainEqual({
        name: "velocity",
        startMs: 0,
        durationMs: 700,
        segments: [
          { phase: "wait", startMs: 0, durationMs: 600 },
          { phase: "download", startMs: 600, durationMs: 100 },
          { phase: "apply", startMs: 700, durationMs: 0 },
        ],
      });

      vi.mocked(analyticsService.getApiV1AnalyticsSignals).mockImplementationOnce(async () => {
        vi.advanceTimersByTime(90);
        return makeSignals();
      });
      await analytics.fetchSignalsForQuality();
      expect(analytics.qualityLastQueryDurationMs).toBe(90);
      expect(analytics.qualityLastQuerySteps).toEqual([
        { name: "signals", startMs: 0, durationMs: 90 },
      ]);
      expect(analytics.lastQueryDurationMs).toBe(700);
    } finally {
      vi.useRealTimers();
    }
  });

  it("lists each panel request live while a refresh runs", async () => {
    let releaseVelocity!: () => void;
    vi.mocked(analyticsService.getApiV1AnalyticsVelocity).mockImplementationOnce(
      () =>
        new Promise((resolve) => {
          releaseVelocity = () => resolve(makeVelocity());
        }),
    );
    const refresh = analytics.fetchAll();
    await vi.waitFor(() => {
      expect(analytics.liveQuery.steps.filter((step) => step.running)).toHaveLength(1);
    });

    // Every other panel has settled; the held one is still running.
    expect(analytics.liveQuery.startedAt).not.toBeNull();
    expect(analytics.liveQuery.steps.find((step) => step.running)?.name).toBe("velocity");
    expect(analytics.liveQuery.steps).toHaveLength(11);

    releaseVelocity();
    await refresh;
    expect(analytics.liveQuery.startedAt).toBeNull();
    expect(analytics.lastQuerySteps.some((step) => step.running)).toBe(false);
  });

  it("does not mark cached partial refresh failures as current", async () => {
    vi.useFakeTimers({ toFake: ["Date"] });
    const warn = vi.spyOn(console, "warn").mockImplementation(() => {});
    try {
      vi.setSystemTime(new Date("2026-06-15T15:00:00Z"));
      await analytics.fetchAll();
      const previousUpdatedAt = analytics.lastUpdatedAt;

      analytics.markNewData();
      vi.mocked(analyticsService.getApiV1AnalyticsVelocity).mockRejectedValueOnce(
        new Error("velocity failed"),
      );

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

    expect(analyticsService.getApiV1AnalyticsHeatmap.mock.lastCall?.[0]).toEqual(
      expect.objectContaining({ from: "2024-01-01", to: "2024-01-31" }),
    );
  });
});

describe("AnalyticsStore hour-of-week range context", () => {
  it("keeps the parent range during a full refresh with a selected date", async () => {
    analytics.applyDateRange("2024-01-01", "2024-01-31");
    analytics.selectDate("2024-01-15");
    vi.clearAllMocks();
    mockAllAPIs();

    await analytics.fetchAll();

    expect(analyticsService.getApiV1AnalyticsHourOfWeek.mock.lastCall?.[0]).toEqual(
      expect.objectContaining({ from: "2024-01-01", to: "2024-01-31" }),
    );
  });
});

describe("AnalyticsStore token metrics", () => {
  it("passes output_tokens heatmap metric through to the API", () => {
    analytics.setMetric("output_tokens");

    expect(analyticsService.getApiV1AnalyticsHeatmap.mock.lastCall?.[0]).toEqual(
      expect.objectContaining({ metric: "output_tokens" }),
    );
  });

  it("passes output_tokens top-session metric through to the API", () => {
    analytics.setTopMetric("output_tokens");

    expect(analyticsService.getApiV1AnalyticsTopSessions.mock.lastCall?.[0]).toEqual(
      expect.objectContaining({ metric: "output_tokens" }),
    );
  });
});

describe("AnalyticsStore activity uses full range", () => {
  it("should use base from/to for activity even with selectedDate", async () => {
    analytics.selectDate("2024-01-15");
    vi.clearAllMocks();

    await analytics.fetchActivity();

    expect(analyticsService.getApiV1AnalyticsActivity.mock.lastCall?.[0]).toEqual(
      expect.objectContaining({ from: "2024-01-01", to: "2024-01-31" }),
    );
  });

  it("does not retain activity data when a new granularity request fails", async () => {
    analytics.activity = makeActivity();
    await analytics.fetchActivity();
    analytics.granularity = "week";
    vi.mocked(analyticsService.getApiV1AnalyticsActivity).mockRejectedValueOnce(
      new Error("activity failed"),
    );

    await analytics.fetchActivity();

    expect(analytics.activity).toBeNull();
    expect(analytics.errors.activity).toBe("activity failed");
  });
});

describe("AnalyticsStore fetch start handler", () => {
  it("runs only for full refreshes, not panel fetches", async () => {
    const handler = vi.fn();
    analytics.setFetchStartHandler(handler);

    try {
      await analytics.fetchActivity();
      expect(handler).not.toHaveBeenCalled();

      await analytics.fetchAll();
      expect(handler).toHaveBeenCalledOnce();
    } finally {
      analytics.setFetchStartHandler(undefined);
    }
  });
});

describe("AnalyticsStore activity range selection", () => {
  it("filters dependent analytics and Sessions without changing the chart window", () => {
    const loadSessions = vi.spyOn(sessions, "load").mockResolvedValue();

    analytics.setActivitySelection("2024-01-10", "2024-01-12");

    expect(analytics.from).toBe("2024-01-01");
    expect(analytics.to).toBe("2024-01-31");
    expect(sessions.filters.dateFrom).toBe("2024-01-10");
    expect(sessions.filters.dateTo).toBe("2024-01-12");
    expect(loadSessions).toHaveBeenCalledOnce();
    expect(analyticsService.getApiV1AnalyticsSummary.mock.lastCall?.[0]).toEqual(
      expect.objectContaining({ from: "2024-01-10", to: "2024-01-12" }),
    );
    expect(analyticsService.getApiV1AnalyticsActivity).not.toHaveBeenCalled();
    expect(analyticsService.getApiV1AnalyticsHeatmap).not.toHaveBeenCalled();
  });

  it("clears the selection and restores the Sessions parent window", () => {
    vi.spyOn(sessions, "load").mockResolvedValue();
    analytics.setActivitySelection("2024-01-10", "2024-01-12");
    vi.clearAllMocks();
    mockAllAPIs();

    analytics.clearActivitySelection();

    expect(analytics.selectedActivityRange).toBeNull();
    expect(sessions.filters.dateFrom).toBe("2024-01-01");
    expect(sessions.filters.dateTo).toBe("2024-01-31");
    expect(analyticsService.getApiV1AnalyticsSummary.mock.lastCall?.[0]).toEqual(
      expect.objectContaining({ from: "2024-01-01", to: "2024-01-31" }),
    );
  });

  it("replaces a selected heatmap date with the brushed range", () => {
    vi.spyOn(sessions, "load").mockResolvedValue();
    analytics.selectDate("2024-01-15");
    vi.clearAllMocks();
    mockAllAPIs();

    analytics.setActivitySelection("2024-01-10", "2024-01-12");

    expect(analytics.selectedDate).toBeNull();
    expect(analytics.selectedActivityRange).toEqual({
      from: "2024-01-10",
      to: "2024-01-12",
    });
    expect(analyticsService.getApiV1AnalyticsSummary.mock.lastCall?.[0]).toEqual(
      expect.objectContaining({ from: "2024-01-10", to: "2024-01-12" }),
    );
  });

  it("replaces a brushed range with a heatmap date and restores parent context", () => {
    const loadSessions = vi.spyOn(sessions, "load").mockResolvedValue();
    analytics.setActivitySelection("2024-01-10", "2024-01-12");
    vi.clearAllMocks();
    mockAllAPIs();

    analytics.selectDate("2024-01-15");

    expect(analytics.selectedActivityRange).toBeNull();
    expect(analytics.selectedDate).toBe("2024-01-15");
    expect(sessions.filters.dateFrom).toBe("2024-01-01");
    expect(sessions.filters.dateTo).toBe("2024-01-31");
    expect(loadSessions).toHaveBeenCalledOnce();
    expect(analyticsService.getApiV1AnalyticsHourOfWeek).toHaveBeenCalledOnce();
    expect(analyticsService.getApiV1AnalyticsHourOfWeek.mock.lastCall?.[0]).toEqual(
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
      from: "2024-01-01",
      to: "2024-01-31",
    });
    expect(analyticsService.getApiV1AnalyticsSummary.mock.lastCall?.[0]).toEqual(expected);
    expect(analyticsService.getApiV1AnalyticsProjects.mock.lastCall?.[0]).toEqual(expected);
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
  ])("should include project in $name params", ({ fn }) => {
    analytics.setProject("alpha");
    const params = vi.mocked(fn()).mock.lastCall?.[0];
    expect(params?.project).toBe("alpha");
  });

  it.each([
    { name: "heatmap", fn: () => analyticsService.getApiV1AnalyticsHeatmap },
    { name: "hourOfWeek", fn: () => analyticsService.getApiV1AnalyticsHourOfWeek },
  ])("should include project in $name base params", ({ fn }) => {
    analytics.setProject("alpha");
    const params = vi.mocked(fn()).mock.lastCall?.[0];
    expect(params?.project).toBe("alpha");
  });

  it("should exclude project from fetchProjects params", () => {
    analytics.setProject("alpha");

    const projectsParams = vi.mocked(analyticsService.getApiV1AnalyticsProjects).mock.lastCall?.[0];
    expect(projectsParams?.project).toBeUndefined();
  });

  it("should exclude project from fetchProjects even with selectedDate", () => {
    analytics.selectDate("2024-01-15");
    vi.clearAllMocks();

    analytics.setProject("alpha");

    const projectsParams = vi.mocked(analyticsService.getApiV1AnalyticsProjects).mock.lastCall?.[0];
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
  ])("should clear project from $name params after deselecting", ({ fn }) => {
    analytics.setProject("alpha");
    vi.clearAllMocks();

    analytics.setProject("alpha"); // deselect

    const mock = vi.mocked(fn());
    expect(mock).toHaveBeenCalled();
    const params = mock.mock.lastCall?.[0];
    expect(params?.project).toBeUndefined();
  });
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
  ])("should include machine in $name params", async ({ fn }) => {
    analytics.machine = "host-a,host-b";

    await analytics.fetchAll();

    const mock = vi.mocked(fn());
    expect(mock).toHaveBeenCalled();
    const params = mock.mock.lastCall?.[0];
    expect(params?.machine).toBe("host-a,host-b");
  });
});

describe("AnalyticsStore automated scope params", () => {
  it("derives all scope from legacy includeAutomated updates", async () => {
    analytics.includeAutomated = true;

    await analytics.fetchSummary();

    expect(analyticsService.getApiV1AnalyticsSummary.mock.lastCall?.[0]).toEqual(
      expect.objectContaining({ automated_scope: "all" }),
    );
  });

  it("keeps automated-only scope when selected explicitly", async () => {
    analytics.setAutomatedScope("automated");

    await analytics.fetchSummary();

    expect(analyticsService.getApiV1AnalyticsSummary.mock.lastCall?.[0]).toEqual(
      expect.objectContaining({ automated_scope: "automated" }),
    );
  });
});

describe("AnalyticsStore model filter", () => {
  it("retains a saved model's group when a new date read fails", async () => {
    analytics.model = "claude-opus-4-20250514";
    analyticsService.getApiV1AnalyticsSummary.mockResolvedValueOnce({
      ...makeSummary(),
      model_aliases: { "claude-opus-4-20250514": "claude-4" },
    });
    await analytics.fetchSummary();
    analytics.from = "2024-02-01";
    analyticsService.getApiV1AnalyticsSummary.mockRejectedValueOnce(new Error("Summary timed out"));
    await analytics.fetchSummary();
    expect(analytics.summary).toBeNull();
    expect(analytics.errors.summary).toBe("Summary timed out");
    expect(analytics.displayedModels).toBe("claude-4");
    const refresh = vi.spyOn(analytics, "fetchAll");
    analytics.toggleModel("claude-4");
    expect(analytics.model).toBe("");
    await refresh.mock.results[0]!.value;
    refresh.mockRestore();
  });

  it("requests grouped model rows", async () => {
    await analytics.fetchSummary();
    expect(analyticsService.getApiV1AnalyticsSummary.mock.lastCall?.[0]).toEqual(
      expect.objectContaining({ group_models: true }),
    );
  });

  it("displays a saved member selection through its group", async () => {
    analytics.model = "claude-opus-4-20250514";
    analyticsService.getApiV1AnalyticsSummary.mockResolvedValue({
      ...makeSummary(),
      model_aliases: { "claude-opus-4-20250514": "claude-4" },
    });
    await analytics.fetchAll();
    expect(analyticsService.getApiV1AnalyticsSummary).toHaveBeenCalledTimes(1);
    expect(analytics.model).toBe("claude-opus-4-20250514");
    expect(analytics.displayedModels).toBe("claude-4");
    expect(analyticsService.getApiV1AnalyticsSummary.mock.lastCall?.[0]?.model).toBe(
      "claude-opus-4-20250514",
    );
  });

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
    const params = vi.mocked(analyticsService.getApiV1AnalyticsSummary).mock.lastCall?.[0];
    expect(params?.model).toBeUndefined();
  });

  it("fetchSignalsForQuality omits the model filter", async () => {
    analytics.model = "gpt-4o";
    vi.clearAllMocks();

    await analytics.fetchSignalsForQuality();

    expect(analyticsService.getApiV1AnalyticsSignals.mock.lastCall?.[0]).toEqual(
      expect.not.objectContaining({ model: expect.anything() }),
    );
    // The Quality page has no model control; the selected model stays put for
    // the Analytics page rather than being cleared by viewing Quality.
    expect(analytics.model).toBe("gpt-4o");
  });

  it("signalEvidenceParams omits the model filter", () => {
    analytics.model = "gpt-4o";
    expect(analytics.signalEvidenceParams().model).toBeUndefined();
  });

  it("drops stale model-scoped signals while an Quality fetch is pending", async () => {
    // Analytics loads model-scoped signals into the shared cache.
    analytics.model = "gpt-4o";
    await analytics.fetchSignals();
    expect(analytics.signals).not.toBeNull();

    // The unmodelled Quality fetch is held in flight.
    let resolve!: (v: SignalsAnalyticsResponse) => void;
    vi.mocked(analyticsService.getApiV1AnalyticsSignals).mockReturnValue(
      new Promise((r) => {
        resolve = r;
      }),
    );

    const pending = analytics.fetchSignalsForQuality();

    // The model-scoped cache is dropped up front, so Quality shows a loading
    // skeleton instead of another scope's signals during the fetch.
    expect(analytics.signals).toBeNull();
    expect(analytics.loading.signals).toBe(true);

    resolve(makeSignals());
    await pending;
    expect(analytics.signals).not.toBeNull();
  });

  it("does not retain stale model-scoped signals when an Quality fetch fails", async () => {
    // Analytics loads model-scoped signals into the shared cache.
    analytics.model = "gpt-4o";
    await analytics.fetchSignals();
    expect(analytics.signals).not.toBeNull();

    // The unmodelled Quality fetch fails.
    vi.mocked(analyticsService.getApiV1AnalyticsSignals).mockRejectedValueOnce(
      new Error("signals failed"),
    );

    await analytics.fetchSignalsForQuality();

    // The wrong-scope cache is cleared and the failure surfaces rather than
    // being swallowed as a cached refetch that keeps stale data.
    expect(analytics.signals).toBeNull();
    expect(analytics.errors.signals).toBe("signals failed");
  });

  it("drops stale drill-down-scoped signals when entering Quality without a model", async () => {
    // Analytics loaded signals under a heatmap drill-down (hour) and no model,
    // so the cache scope differs from Quality only by the drill-down filter.
    analytics.selectedHour = 9;
    await analytics.fetchSignals();
    expect(analytics.signals).not.toBeNull();

    // Quality clears the drill-down; hold the unmodelled fetch in flight.
    let resolve!: (v: SignalsAnalyticsResponse) => void;
    vi.mocked(analyticsService.getApiV1AnalyticsSignals).mockReturnValue(
      new Promise((r) => {
        resolve = r;
      }),
    );

    const pending = analytics.fetchSignalsForQuality();

    // The drill-down-scoped cache is dropped even though no model was set, so
    // Quality does not show another scope's signals while the fetch runs.
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
      new Promise((r) => {
        resolve = r;
      }),
    );

    const p = analytics.fetchSummary();
    expect(analytics.loading.summary).toBe(true);

    resolve(makeSummary());
    await p;
    expect(analytics.loading.summary).toBe(false);
  });

  it("should expose query progress during cached refetches", async () => {
    await analytics.fetchSummary();
    let resolve!: (v: AnalyticsSummary) => void;
    vi.mocked(analyticsService.getApiV1AnalyticsSummary).mockReturnValue(
      new Promise((r) => {
        resolve = r;
      }),
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
    vi.mocked(analyticsService.getApiV1AnalyticsSummary).mockRejectedValueOnce(new Error("fail"));
    await analytics.fetchSummary();
    expect(analytics.errors.summary).toBe("fail");

    vi.mocked(analyticsService.getApiV1AnalyticsSummary).mockResolvedValueOnce(makeSummary());
    await analytics.fetchSummary();
    expect(analytics.errors.summary).toBeNull();
  });

  it("should set error message on failure", async () => {
    vi.mocked(analyticsService.getApiV1AnalyticsSummary).mockRejectedValueOnce(
      new Error("network down"),
    );

    await analytics.fetchSummary();

    expect(analytics.errors.summary).toBe("network down");
    expect(analytics.loading.summary).toBe(false);
  });

  it("should use fallback message for non-Error throws", async () => {
    vi.mocked(analyticsService.getApiV1AnalyticsSummary).mockRejectedValueOnce("string error");

    await analytics.fetchSummary();

    expect(analytics.errors.summary).toBe("Failed to load");
  });

  it("ignores stale summary data and keeps accepted params after a failed refetch", async () => {
    let resolveFirst!: (v: AnalyticsSummary) => void;
    vi.mocked(analyticsService.getApiV1AnalyticsSummary).mockReturnValueOnce(
      new Promise((r) => {
        resolveFirst = r;
      }),
    );

    analytics.from = "2025-01-01";
    const firstFetch = analytics.fetchSummary();

    analytics.from = "2026-01-01";
    analytics.to = "2026-01-31";
    const secondData = makeSummary();
    secondData.total_sessions = 99;
    vi.mocked(analyticsService.getApiV1AnalyticsSummary).mockResolvedValueOnce(secondData);
    const secondFetch = analytics.fetchSummary();

    await secondFetch;
    expect(analytics.summary?.total_sessions).toBe(99);

    const staleData = makeSummary();
    staleData.total_sessions = 1;
    resolveFirst(staleData);
    await firstFetch;

    expect(analytics.summary?.total_sessions).toBe(99);
    expect(analytics.summaryFilterParams).toMatchObject({ from: "2026-01-01", to: "2026-01-31" });

    vi.mocked(analyticsService.getApiV1AnalyticsSummary).mockRejectedValueOnce(
      new Error("summary unavailable"),
    );
    await analytics.fetchSummary();
    expect(analytics.summaryFilterParams).toMatchObject({ from: "2026-01-01", to: "2026-01-31" });
  });

  it.each(["summary", "tools"] as const)(
    "clears the %s filter snapshot with its data",
    async (panel) => {
      const fetch =
        panel === "summary" ? () => analytics.fetchSummary() : () => analytics.fetchTools();
      const read =
        panel === "summary"
          ? analyticsService.getApiV1AnalyticsSummary
          : analyticsService.getApiV1AnalyticsTools;
      const snapshot = panel === "summary" ? "summaryFilterParams" : "toolsFilterParams";
      await fetch();
      expect(analytics[snapshot]).not.toBeNull();
      analytics.agent = "codex";
      let release!: (value: unknown) => void;
      read.mockReturnValueOnce(
        new Promise((resolve) => {
          release = resolve;
        }),
      );
      const pending = fetch();
      expect(analytics[panel]).toBeNull();
      expect(analytics[snapshot]).toBeNull();
      release(panel === "summary" ? makeSummary() : makeTools());
      await pending;
    },
  );

  it("should ignore stale error from superseded request", async () => {
    let rejectFirst!: (e: Error) => void;
    vi.mocked(analyticsService.getApiV1AnalyticsSummary).mockReturnValueOnce(
      new Promise((_r, rej) => {
        rejectFirst = rej;
      }),
    );

    const firstFetch = analytics.fetchSummary();

    const data = makeSummary();
    vi.mocked(analyticsService.getApiV1AnalyticsSummary).mockResolvedValueOnce(data);
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
    vi.mocked(analyticsService.getApiV1AnalyticsSummary).mockReturnValueOnce(
      new Promise((r) => {
        resolveFirst = r;
      }),
    );

    const firstFetch = analytics.fetchSummary();

    let resolveSecond!: (v: AnalyticsSummary) => void;
    vi.mocked(analyticsService.getApiV1AnalyticsSummary).mockReturnValueOnce(
      new Promise((r) => {
        resolveSecond = r;
      }),
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
    vi.mocked(analyticsService.getApiV1AnalyticsSummary)
      .mockImplementationOnce(() => new Promise(() => {}))
      .mockResolvedValueOnce(makeSummary());

    void analytics.fetchSummary();
    await Promise.resolve();
    void analytics.fetchSummary();
    await Promise.resolve();

    expect(
      vi.mocked(AnalyticsService.getApiV1AnalyticsSummary).mock.calls[0]?.[1]?.signal,
    ).toBeDefined();
    expect(
      vi.mocked(AnalyticsService.getApiV1AnalyticsSummary).mock.calls[0]?.[1]?.signal?.aborted,
    ).toBe(true);
  });

  it("aborts visible panel requests on teardown", async () => {
    vi.mocked(analyticsService.getApiV1AnalyticsSummary).mockImplementationOnce(
      () => new Promise(() => {}),
    );

    void analytics.fetchSummary();
    await Promise.resolve();
    analytics.cancelInFlightReads();

    expect(
      vi.mocked(AnalyticsService.getApiV1AnalyticsSummary).mock.calls[0]?.[1]?.signal?.aborted,
    ).toBe(true);
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

  it("fetchAll re-derives its rolling request range", async () => {
    const { analytics } = await loadAnalyticsStore();
    vi.setSystemTime(new Date("2026-04-26T12:00:00"));
    await analytics.fetchAll();
    expect(analyticsService.getApiV1AnalyticsSummary.mock.lastCall?.[0]).toMatchObject({
      from: "2025-04-27",
      to: "2026-04-26",
    });
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

  it("fetchSignalsForQuality clears hidden drill-down filters", async () => {
    const { analytics } = await loadAnalyticsStore();
    analytics.from = "2026-04-01";
    analytics.to = "2026-04-30";
    analytics.isPinned = true;
    analytics.selectedDate = "2026-04-12";
    analytics.selectedActivityRange = {
      from: "2026-04-08",
      to: "2026-04-14",
    };
    analytics.selectedDow = 2;
    analytics.selectedHour = 16;

    await analytics.fetchSignalsForQuality();

    expect(analytics.selectedDate).toBeNull();
    expect(analytics.selectedActivityRange).toBeNull();
    expect(analytics.selectedDow).toBeNull();
    expect(analytics.selectedHour).toBeNull();
    expect(analyticsService.getApiV1AnalyticsSignals.mock.lastCall?.[0]).toEqual(
      expect.objectContaining({
        from: "2026-04-01",
        to: "2026-04-30",
      }),
    );
    expect(analyticsService.getApiV1AnalyticsSignals.mock.lastCall?.[0]).toEqual(
      expect.not.objectContaining({
        dow: expect.anything(),
        hour: expect.anything(),
      }),
    );
  });

  it("fetchSignalsForQuality re-derives rolling dates before fetching", async () => {
    const { analytics } = await loadAnalyticsStore();
    analytics.setRollingWindow(7);
    vi.clearAllMocks();

    vi.setSystemTime(new Date("2026-04-26T12:00:00"));
    await analytics.fetchSignalsForQuality();

    expect(analytics.from).toBe("2026-04-20");
    expect(analytics.to).toBe("2026-04-26");
    expect(analyticsService.getApiV1AnalyticsSignals.mock.lastCall?.[0]).toEqual(
      expect.objectContaining({
        from: "2026-04-20",
        to: "2026-04-26",
      }),
    );
  });
});

describe("invalid machine refetches", () => {
  it("clears cached summary and shared quality signals on an input error", async () => {
    analytics.machine = "host-a";
    await analytics.fetchSummary();
    await analytics.fetchSignals();
    expect(analytics.summary?.total_sessions).toBe(10);
    expect(analytics.signals).not.toBeNull();
    analyticsService.getApiV1AnalyticsSummary.mockImplementationOnce((params) => {
      expect(params.machine).toBe("host-a");
      return Promise.reject(new ApiError(400, "unknown machine", "invalid_machine_filter"));
    });
    await analytics.fetchSummary();
    expect(analytics.summary).toBeNull();
    expect(analytics.signals).toBeNull();
    expect(analytics.errors.summary).toBe("unknown machine");
  });
});

describe("AnalyticsStore panel scopes", () => {
  it("keeps the panel error visible while retry is pending", async () => {
    await analytics.fetchTools();

    vi.mocked(analyticsService.getApiV1AnalyticsTools).mockRejectedValueOnce(
      new Error("tools unavailable"),
    );
    await analytics.fetchTools();
    expect(analytics.errors.tools).toBe("tools unavailable");
    let releaseRetry!: (value: ToolsAnalyticsResponse) => void;
    vi.mocked(analyticsService.getApiV1AnalyticsTools).mockReturnValueOnce(
      new Promise((resolve) => {
        releaseRetry = resolve;
      }),
    );
    const retry = analytics.fetchTools();
    expect(analytics.errors.tools).toBe("tools unavailable");
    releaseRetry(makeTools());
    await retry;
  });

  it("passes the selected hour to activity and heatmap requests", async () => {
    await analytics.fetchAll();
    analytics.selectHourOfWeek(1, 9);
    for (const read of [
      analyticsService.getApiV1AnalyticsActivity,
      analyticsService.getApiV1AnalyticsHeatmap,
    ]) {
      expect(read.mock.lastCall?.[0]).toMatchObject({ dow: 1, hour: 9 });
    }
    await vi.waitFor(() =>
      expect(analytics.querying.activity || analytics.querying.heatmap).toBe(false),
    );
  });
});
