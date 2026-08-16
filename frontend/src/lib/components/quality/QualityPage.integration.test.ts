// @vitest-environment jsdom
import {
  afterEach,
  beforeEach,
  describe,
  expect,
  it,
  vi,
} from "vite-plus/test";
import { mount, tick, unmount } from "svelte";
import { analytics } from "../../stores/analytics.svelte.js";
import { analyticsPageDates } from "../../stores/analyticsPageDates.js";
import { router } from "../../stores/router.svelte.js";
import { ui } from "../../stores/ui.svelte.js";
import { yokedDates } from "../../stores/yokedDates.svelte.js";
import {
  AnalyticsService,
  CancelablePromise,
} from "../../api/generated/index.js";
import type { SignalsAnalyticsResponse } from "../../api/types.js";
// @ts-ignore
import QualityPage from "./QualityPage.svelte";

const mocks = vi.hoisted(() => ({
  copyToClipboard: vi.fn().mockResolvedValue(true),
  downloadInsightExport: vi.fn().mockResolvedValue(undefined),
  deleteItem: vi.fn(),
  loadAgents: vi.fn(),
  loadInsights: vi.fn(),
  loadProjects: vi.fn(),
  navigateToSession: vi.fn(),
  watchEvents: vi.fn(() => ({ close() {} })),
}));

const state = vi.hoisted(() => {
  const selectedInsight = {
    id: 42,
    type: "daily_activity",
    date_from: "2026-06-24",
    date_to: "2026-06-24",
    project: "agentsview",
    agent: "claude",
    model: "sonnet",
    content: "# Insight\n\n- Shipped change",
    created_at: "2026-06-24T12:00:00Z",
  };

  return {
    selectedInsight,
    insightsStore: {
      type: "daily_activity",
      dateFrom: "2026-06-24",
      dateTo: "2026-06-24",
      project: "",
      agent: "claude",
      promptText: "",
      tasks: [],
      items: [selectedInsight],
      selectedId: 42,
      selectedTaskId: null,
      selectedTask: undefined,
      selectedItem: selectedInsight,
      loading: false,
      generatingCount: 0,
      load: mocks.loadInsights,
      setType: vi.fn(),
      setDateFrom: vi.fn(),
      setDateTo: vi.fn(),
      setProject: vi.fn(),
      setAgent: vi.fn(),
      generate: vi.fn(),
      select: vi.fn(),
      selectTask: vi.fn(),
      cancelAll: vi.fn(),
      cancelInFlightReads: vi.fn(),
      cancelTask: vi.fn(),
      dismissTask: vi.fn(),
      deleteItem: mocks.deleteItem,
    },
  };
});

const syncState = vi.hoisted(() => ({
  serverVersion: {
    read_only: false,
  } as {
    read_only: boolean;
    insight_generation_available?: boolean;
  },
}));

vi.mock("../../api/client.js", () => ({
  downloadInsightExport: mocks.downloadInsightExport,
  watchEvents: mocks.watchEvents,
}));

vi.mock("../../utils/clipboard.js", () => ({
  copyToClipboard: mocks.copyToClipboard,
}));

vi.mock("../../stores/insights.svelte.js", () => ({
  insights: state.insightsStore,
}));

vi.mock("../../stores/sessions.svelte.js", () => ({
  sessions: {
    agents: [],
    filters: {
      project: "",
      machine: "",
      agent: "",
      termination: "",
      recentlyActive: false,
      minUserMessages: 0,
      includeOneShot: false,
      includeAutomated: true,
    },
    projects: [],
    loadAgents: mocks.loadAgents,
    loadProjects: mocks.loadProjects,
    navigateToSession: mocks.navigateToSession,
  },
}));

vi.mock("../../stores/sync.svelte.js", () => ({
  sync: {
    get serverVersion() {
      return syncState.serverVersion;
    },
  },
}));

vi.mock("../../paraglide/messages.js", () => {
  const stub = new Proxy(
    {},
    {
      get(_target, prop) {
        if (prop === "m") return stub;
        return () => String(prop);
      },
    },
  );
  return stub;
});

vi.mock("../../utils/markdown.js", () => ({
  renderMarkdown: (content: string) => content,
}));

vi.mock("../../utils/highlight-fences.js", () => ({
  highlightCodeFences: () => ({
    destroy() {},
  }),
}));

async function flushEffects() {
  await tick();
  await Promise.resolve();
  await tick();
}

async function selectCustomRange(fromLabel: string, toLabel: string) {
  const trigger = document.querySelector<HTMLButtonElement>(
    ".kit-date-range-picker__trigger",
  );
  expect(trigger).not.toBeNull();
  trigger!.click();
  await flushEffects();

  const customTab = [
    ...document.querySelectorAll<HTMLElement>('[role="radio"]'),
  ][2];
  expect(customTab).not.toBeUndefined();
  customTab!.click();
  await flushEffects();

  const from = document.querySelector<HTMLButtonElement>(
    `.kit-calendar button[aria-label="${fromLabel}"]`,
  );
  expect(from).not.toBeNull();
  from!.click();
  await flushEffects();

  const to = document.querySelector<HTMLButtonElement>(
    `.kit-calendar button[aria-label="${toLabel}"]`,
  );
  expect(to).not.toBeNull();
  to!.click();
  await flushEffects();
}

const signalsFixture: SignalsAnalyticsResponse = {
  scored_sessions: 2,
  unscored_sessions: 0,
  grade_distribution: { A: 1, B: 1 },
  avg_health_score: 85,
  outcome_distribution: { completed: 2 },
  outcome_confidence_distribution: { high: 2 },
  tool_health: {
    total_failure_signals: 1,
    total_retries: 0,
    total_edit_churn: 0,
    sessions_with_failures: 1,
    failure_rate: 50,
  },
  context_health: {
    avg_compaction_count: 0,
    sessions_with_compaction: 0,
    mid_task_compaction_count: 0,
    sessions_with_mid_task_compaction: 0,
    sessions_with_context_data: 2,
    avg_context_pressure: 0.2,
    high_pressure_sessions: 0,
  },
  quality_health: {
    computed_sessions: 2,
    totals: {
      short_prompt_count: 2,
      unstructured_start: 0,
      missing_success_criteria_count: 0,
      missing_verification_count: 0,
      duplicate_prompt_count: 0,
      no_code_context_count: 0,
      runaway_tool_loop_count: 0,
      frustration_marker_count: 0,
    },
    sessions_with_signal: {
      short_prompt_count: 2,
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

describe("QualityPage date yoke integration", () => {
  let component: ReturnType<typeof mount> | undefined;

  beforeEach(() => {
    vi.clearAllMocks();
    window.history.replaceState(null, "", "/quality");
    router.route = "quality";
    router.params = {};
    analytics.isPinned = false;
    analytics.windowDays = 365;
    analytics.from = "";
    analytics.to = "";
    yokedDates.setEnabled(false);
    analyticsPageDates.clear();
    localStorage.clear();
    vi.useRealTimers();
  });

  afterEach(() => {
    if (component) {
      unmount(component);
      component = undefined;
    }
    vi.restoreAllMocks();
    document.body.innerHTML = "";
    window.history.replaceState(null, "", "/");
    router.route = "sessions";
    router.params = {};
    analytics.isPinned = false;
    analytics.windowDays = 365;
    analytics.from = "";
    analytics.to = "";
    yokedDates.setEnabled(false);
    analyticsPageDates.clear();
    localStorage.clear();
    vi.useRealTimers();
  });

  it("keeps an enabled empty yoke empty during bare rolling refreshes", async () => {
    vi.useFakeTimers({ toFake: ["Date"] });
    vi.setSystemTime(new Date("2026-07-10T12:00:00"));
    const fetchStates: Array<{
      isPinned: boolean;
      windowDays: number;
      from: string;
      to: string;
    }> = [];
    vi.spyOn(analytics, "fetchSignalsForQuality").mockImplementation(
      () => {
        fetchStates.push({
          isPinned: analytics.isPinned,
          windowDays: analytics.windowDays,
          from: analytics.from,
          to: analytics.to,
        });
        return Promise.resolve();
      },
    );
    analytics.isPinned = false;
    analytics.windowDays = 365;
    analytics.from = "2025-07-11";
    analytics.to = "2026-07-10";
    yokedDates.setEnabled(true);

    component = mount(QualityPage, { target: document.body });
    await flushEffects();

    expect(fetchStates[0]).toEqual({
      isPinned: false,
      windowDays: 365,
      from: "2025-07-11",
      to: "2026-07-10",
    });
    expect(router.params.window_days).toBeUndefined();
    expect(window.location.search).not.toContain("window_days");
    expect(yokedDates.range).toBeNull();

    const refresh = document.querySelector<HTMLButtonElement>(
      'button[aria-label="Refresh quality"]',
    );
    expect(refresh).not.toBeNull();
    const callsBeforeRefresh = fetchStates.length;
    refresh!.click();
    await flushEffects();

    expect(fetchStates.length).toBeGreaterThan(callsBeforeRefresh);
    expect(fetchStates.at(-1)).toEqual({
      isPinned: false,
      windowDays: 365,
      from: "2025-07-11",
      to: "2026-07-10",
    });
    expect(router.params.window_days).toBeUndefined();
    expect(yokedDates.range).toBeNull();
  });

  it("aborts its pending signals transport when unmounted", async () => {
    const cancelTransport = vi.fn();
    vi.spyOn(
      AnalyticsService,
      "getApiV1AnalyticsSignals",
    ).mockImplementation(
      () =>
        new CancelablePromise((_resolve, _reject, onCancel) => {
          onCancel(cancelTransport);
        }),
    );

    component = mount(QualityPage, { target: document.body });
    await flushEffects();
    expect(
      AnalyticsService.getApiV1AnalyticsSignals,
    ).toHaveBeenCalled();

    unmount(component);
    component = undefined;

    expect(cancelTransport).toHaveBeenCalledOnce();
  });

  it("does not turn a bare Quality reload into explicit date intent", async () => {
    vi.spyOn(analytics, "fetchSignalsForQuality").mockResolvedValue();
    analytics.applyRollingWindow(365);
    yokedDates.setEnabled(true);

    component = mount(QualityPage, { target: document.body });
    await flushEffects();
    expect(window.location.search).not.toContain("window_days");

    unmount(component);
    component = undefined;
    component = mount(QualityPage, { target: document.body });
    await flushEffects();

    expect(router.params.window_days).toBeUndefined();
    expect(yokedDates.range).toBeNull();
  });

  it("restores a bare Quality history entry without publishing default dates", async () => {
    vi.spyOn(analytics, "fetchSignalsForQuality").mockResolvedValue();
    analytics.applyRollingWindow(365);
    yokedDates.setEnabled(true);

    component = mount(QualityPage, { target: document.body });
    await flushEffects();
    unmount(component);
    component = undefined;

    router.navigate("usage");
    window.history.replaceState(null, "", "/quality");
    window.dispatchEvent(new PopStateEvent("popstate"));
    expect(router.route).toBe("quality");
    expect(router.params).toEqual({});

    component = mount(QualityPage, { target: document.body });
    await flushEffects();

    expect(window.location.search).toBe("");
    expect(yokedDates.range).toBeNull();
  });

  it("restores fixed picker dates to the URL after navigation", async () => {
    vi.useFakeTimers({ toFake: ["Date"] });
    vi.setSystemTime(new Date("2026-07-10T12:00:00"));
    vi.stubGlobal(
      "ResizeObserver",
      class {
        observe() {}
        unobserve() {}
        disconnect() {}
      },
    );
    vi.spyOn(analytics, "fetchAll").mockResolvedValue();
    vi.spyOn(analytics, "fetchSignalsForQuality").mockResolvedValue();
    yokedDates.setEnabled(false);

    component = mount(QualityPage, { target: document.body });
    await flushEffects();
    await selectCustomRange("Jul 1, 2026", "Jul 7, 2026");

    router.navigate("usage");
    unmount(component);
    component = undefined;
    router.navigate("quality");
    component = mount(QualityPage, { target: document.body });
    await flushEffects();

    expect(analytics.isPinned).toBe(true);
    expect(router.params.window_days).toBeUndefined();
    expect(router.params.date_from).toBe("2026-07-01");
    expect(router.params.date_to).toBe("2026-07-07");
  });

  it("establishes an enabled empty yoke from explicit URL dates", async () => {
    const fetchStates: Array<{
      isPinned: boolean;
      from: string;
      to: string;
    }> = [];
    vi.spyOn(analytics, "fetchSignalsForQuality").mockImplementation(
      () => {
        fetchStates.push({
          isPinned: analytics.isPinned,
          from: analytics.from,
          to: analytics.to,
        });
        return Promise.resolve();
      },
    );
    window.history.replaceState(
      null,
      "",
      "/quality?date_from=2026-06-01&date_to=2026-06-07",
    );
    router.params = {
      date_from: "2026-06-01",
      date_to: "2026-06-07",
    };
    yokedDates.setEnabled(true);

    component = mount(QualityPage, { target: document.body });
    await flushEffects();

    expect(fetchStates[0]).toEqual({
      isPinned: true,
      from: "2026-06-01",
      to: "2026-06-07",
    });
    expect(yokedDates.range).toMatchObject({
      from: "2026-06-01",
      to: "2026-06-07",
      mode: "fixed",
    });
  });

  it("seeds bare Quality from an enabled fixed range", async () => {
    const fetchStates: Array<{
      isPinned: boolean;
      from: string;
      to: string;
    }> = [];
    vi.spyOn(analytics, "fetchSignalsForQuality").mockImplementation(
      () => {
        fetchStates.push({
          isPinned: analytics.isPinned,
          from: analytics.from,
          to: analytics.to,
        });
        return Promise.resolve();
      },
    );
    yokedDates.setEnabled(true);
    yokedDates.updateFromPanel({
      from: "2026-06-01",
      to: "2026-06-07",
      mode: "fixed",
    });

    component = mount(QualityPage, { target: document.body });
    await flushEffects();

    expect(analytics.isPinned).toBe(true);
    expect(analytics.from).toBe("2026-06-01");
    expect(analytics.to).toBe("2026-06-07");
    expect(fetchStates[0]).toEqual({
      isPinned: true,
      from: "2026-06-01",
      to: "2026-06-07",
    });
  });
});

describe("QualityPage evidence navigation", () => {
  let component: ReturnType<typeof mount> | undefined;

  beforeEach(() => {
    vi.clearAllMocks();
    window.history.replaceState(null, "", "/quality");
    router.route = "quality";
    router.params = {};
    analytics.signals = signalsFixture;
    analytics.loading.signals = false;
    analytics.errors.signals = null;
    vi.spyOn(analytics, "fetchSignalsForQuality").mockResolvedValue();
  });

  afterEach(() => {
    if (component) {
      unmount(component);
      component = undefined;
    }
    vi.restoreAllMocks();
    document.body.innerHTML = "";
    analytics.signals = null;
    analytics.loading.signals = false;
    analytics.errors.signals = null;
    window.history.replaceState(null, "", "/");
    router.route = "sessions";
    router.params = {};
  });

  it("navigates with the clicked example's session and message ordinal", async () => {
    vi.spyOn(
      AnalyticsService,
      "getApiV1AnalyticsSignalSessions",
    ).mockResolvedValue({
      signal: "short_prompt_count",
      sessions: [
        {
          session_id: "first-session",
          project: "alpha",
          agent: "codex",
          date: "2026-07-10",
          is_automated: false,
          outcome: "completed",
          health_score: 90,
          health_grade: "A",
          signal_total: 1,
          reason_code: "short_prompt",
          excerpt: "First example",
          message_ordinal: 7,
          failure_signals: 0,
          retries: 0,
          edit_churn: 0,
        },
        {
          session_id: "second-session",
          project: "beta",
          agent: "claude",
          date: "2026-07-09",
          is_automated: false,
          outcome: "errored",
          health_score: 55,
          health_grade: "D",
          signal_total: 2,
          reason_code: "short_prompt",
          excerpt: "Second example",
          message_ordinal: 23,
          failure_signals: 2,
          retries: 1,
          edit_churn: 1,
        },
      ],
    });
    const scrollToOrdinal = vi.spyOn(ui, "scrollToOrdinal");
    const routeToSession = vi
      .spyOn(router, "navigateToSession")
      .mockImplementation(() => {});

    component = mount(QualityPage, { target: document.body });
    await flushEffects();
    document.querySelector<HTMLButtonElement>(".driver-row")!.click();
    await flushEffects();

    const evidenceLinks = document.querySelectorAll<HTMLAnchorElement>(
      "a.evidence-row",
    );
    expect(evidenceLinks).toHaveLength(2);
    evidenceLinks[1]!.click();
    await flushEffects();

    expect(scrollToOrdinal).toHaveBeenCalledWith(23, "second-session");
    expect(routeToSession).toHaveBeenCalledWith("second-session", {
      msg: "23",
    });
  });
});
