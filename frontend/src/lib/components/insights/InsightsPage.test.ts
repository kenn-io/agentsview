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
// @ts-ignore
import InsightsPage from "./InsightsPage.svelte";
import source from "./InsightsPage.svelte?raw";

describe("InsightsPage sidebar filter sync", () => {
  it("syncs the automated-session scope from the sidebar", () => {
    // Insight scope derives from analytics.includeAutomated, so the
    // sidebar->insights sync must mirror the analytics page: read the
    // sidebar toggle, map it to all/human, and write both fields.
    const normalized = source.replace(/\s+/g, " ");
    expect(source).toContain("sessions.filters.includeAutomated");
    expect(normalized).toContain('headerIncludeAutomated ? "all" : "human"');
    expect(source).toContain(
      "analytics.includeAutomated = headerIncludeAutomated",
    );
    expect(source).toContain("analytics.automatedScope = headerAutomatedScope");
  });

  it("refetches when the automated scope changes", () => {
    // includeAutomated and automatedScope must take part in the change
    // detection that triggers the refetch, not just be assigned.
    const normalized = source.replace(/\s+/g, " ");
    expect(normalized).toContain(
      "untrack(() => analytics.includeAutomated) !== headerIncludeAutomated",
    );
    expect(normalized).toContain(
      "untrack(() => analytics.automatedScope) !== headerAutomatedScope",
    );
    expect(source).toContain("fetchInsightSignals()");
  });
});

describe("InsightsPage date yoke controls", () => {
  it("updates and seeds shared yoke state from the unified range picker", () => {
    expect(source).toContain("<RangePicker");
    expect(source).toContain("updateYokeFromInsights");
    expect(source).toContain("seedInsightsYoke");
    expect(source).toContain("rangeToPanelDate(seed)");
  });

  it("lets insight URL dates override stored yoke dates", () => {
    expect(source).toContain("insightParamsToPanelDate(router.params)");
    expect(source).toContain("hasInsightDateParams(router.params)");
    expect(source).toContain("paramsWithInsightDate");
    expect(source).toContain("rangeToInsightParams(range)");
  });

  it("preserves relative range selections as rolling yoke state", () => {
    const applyIndex = source.indexOf("function applyRange");
    const parseIndex = source.indexOf(
      "function parseInsightWindowDays",
      applyIndex,
    );
    const applyBlock = source.slice(applyIndex, parseIndex);

    expect(source).toContain('mode: "rolling"');
    expect(source).toContain("windowDays: sel.days");
    expect(applyBlock).toContain("analytics.setRollingWindow(sel.days)");
    expect(applyBlock).toContain("updateYokeFromInsights(state)");
  });

  it("preserves rolling window intent in insight URLs", () => {
    expect(source).toContain('const INSIGHTS_WINDOW_PARAM = "window_days"');
    expect(source).toContain("parseInsightWindowDays");
    expect(source).toContain("rollingRange(windowDays)");
    expect(source).toContain("delete nextParams[key]");
    expect(source).toContain("paramsWithInsightDate");
  });

  it("routes automated scope changes through the insight refresh wrapper", () => {
    const handlerIndex = source.indexOf("function handleAutomatedScopeChange");
    const nextHandlerIndex = source.indexOf(
      "\n\n  function handlePromptChange",
      handlerIndex,
    );
    const handlerBlock = source.slice(handlerIndex, nextHandlerIndex);

    expect(handlerBlock).toContain("fetchInsightSignals()");
    expect(handlerBlock).not.toContain("analytics.setAutomatedScope");
  });
});

const mocks = vi.hoisted(() => ({
  copyToClipboard: vi.fn().mockResolvedValue(true),
  downloadInsightExport: vi.fn().mockResolvedValue(undefined),
  deleteItem: vi.fn(),
  loadAgents: vi.fn(),
  loadInsights: vi.fn(),
  loadProjects: vi.fn(),
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

async function selectRelativeRange(days: number) {
  const trigger = document.querySelector<HTMLButtonElement>(
    ".kit-date-range-picker__trigger",
  );
  expect(trigger).not.toBeNull();
  trigger!.click();
  await flushEffects();

  const preset = [
    ...document.querySelectorAll<HTMLButtonElement>("button"),
  ].find((button) => button.textContent?.trim() === `${days}d`);
  expect(preset).not.toBeUndefined();
  preset!.click();
  await flushEffects();
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

describe("InsightsPage date yoke integration", () => {
  let component: ReturnType<typeof mount> | undefined;

  beforeEach(() => {
    vi.clearAllMocks();
    window.history.replaceState(null, "", "/insights");
    router.route = "insights";
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
    vi.spyOn(analytics, "fetchSignalsForInsights").mockImplementation(
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

    component = mount(InsightsPage, { target: document.body });
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
      'button[aria-label="Refresh insights"]',
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

  it("does not turn a bare Insights reload into explicit date intent", async () => {
    vi.spyOn(analytics, "fetchSignalsForInsights").mockResolvedValue();
    analytics.applyRollingWindow(365);
    yokedDates.setEnabled(true);

    component = mount(InsightsPage, { target: document.body });
    await flushEffects();
    expect(window.location.search).not.toContain("window_days");

    unmount(component);
    component = undefined;
    component = mount(InsightsPage, { target: document.body });
    await flushEffects();

    expect(router.params.window_days).toBeUndefined();
    expect(yokedDates.range).toBeNull();
  });

  it("restores a bare Insights history entry without publishing default dates", async () => {
    vi.spyOn(analytics, "fetchSignalsForInsights").mockResolvedValue();
    analytics.applyRollingWindow(365);
    yokedDates.setEnabled(true);

    component = mount(InsightsPage, { target: document.body });
    await flushEffects();
    unmount(component);
    component = undefined;

    router.navigate("usage");
    window.history.replaceState(null, "", "/insights");
    window.dispatchEvent(new PopStateEvent("popstate"));
    expect(router.route).toBe("insights");
    expect(router.params).toEqual({});

    component = mount(InsightsPage, { target: document.body });
    await flushEffects();

    expect(window.location.search).toBe("");
    expect(yokedDates.range).toBeNull();
  });

  it("keeps picker date intent in copied links after navigation", async () => {
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
    vi.spyOn(analytics, "fetchSignalsForInsights").mockResolvedValue();
    yokedDates.setEnabled(false);

    component = mount(InsightsPage, { target: document.body });
    await flushEffects();
    await selectRelativeRange(30);

    router.navigate("usage");
    unmount(component);
    component = undefined;
    router.navigate("insights");
    component = mount(InsightsPage, { target: document.body });
    await flushEffects();

    const copyButton = document.querySelector<HTMLButtonElement>(
      ".insight-link-copy",
    );
    expect(copyButton).not.toBeNull();
    copyButton!.click();
    await flushEffects();

    expect(mocks.copyToClipboard).toHaveBeenCalledTimes(1);
    const copiedUrl = new URL(
      mocks.copyToClipboard.mock.calls[0]![0],
    );
    expect(copiedUrl.searchParams.get("window_days")).toBe("30");
    expect(copiedUrl.searchParams.get("date_from")).toBe("2026-06-11");
    expect(copiedUrl.searchParams.get("date_to")).toBe("2026-07-10");
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
    vi.spyOn(analytics, "fetchSignalsForInsights").mockResolvedValue();
    yokedDates.setEnabled(false);

    component = mount(InsightsPage, { target: document.body });
    await flushEffects();
    await selectCustomRange("Jul 1, 2026", "Jul 7, 2026");

    router.navigate("usage");
    unmount(component);
    component = undefined;
    router.navigate("insights");
    component = mount(InsightsPage, { target: document.body });
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
    vi.spyOn(analytics, "fetchSignalsForInsights").mockImplementation(
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
      "/insights?date_from=2026-06-01&date_to=2026-06-07",
    );
    router.params = {
      date_from: "2026-06-01",
      date_to: "2026-06-07",
    };
    yokedDates.setEnabled(true);

    component = mount(InsightsPage, { target: document.body });
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

  it("seeds bare Insights from an enabled fixed range", async () => {
    const fetchStates: Array<{
      isPinned: boolean;
      from: string;
      to: string;
    }> = [];
    vi.spyOn(analytics, "fetchSignalsForInsights").mockImplementation(
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

    component = mount(InsightsPage, { target: document.body });
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

describe("InsightsPage selected insight actions", () => {
  let component: ReturnType<typeof mount> | undefined;

  beforeEach(() => {
    vi.clearAllMocks();
    ui.activeModal = null;
    ui.publishSecret = false;
    ui.clearPublishTarget();
    syncState.serverVersion = { read_only: false };
    state.insightsStore.selectedItem = state.selectedInsight;
    state.insightsStore.selectedId = state.selectedInsight.id;
    state.insightsStore.items = [state.selectedInsight];
  });

  afterEach(() => {
    if (component) {
      unmount(component);
      component = undefined;
    }
    document.body.innerHTML = "";
  });

  it("renders the deterministic-vs-generated insights help affordance", async () => {
    component = mount(InsightsPage, { target: document.body });
    await tick();

    const helpBlock = document.querySelector("p.insights-help");
    expect(helpBlock).not.toBeNull();
    const helpText = helpBlock?.textContent ?? "";
    expect(
      helpText.includes("insights_page_insights_help_intro") ||
        helpText.includes("Deterministic sections are computed"),
    ).toBe(true);

    const docsLink = document.querySelector<HTMLAnchorElement>(
      'a[href="https://www.agentsview.io/insights/"]',
    );
    expect(docsLink).not.toBeNull();
    expect(
      (docsLink!.textContent?.includes("insights_page_insights_help_docs") ||
        docsLink!.textContent?.includes("Read Insights docs")),
    ).toBe(true);
    expect(docsLink!.getAttribute("target")).toBe("_blank");
    expect(docsLink!.getAttribute("rel")).toContain("noopener");
    expect(docsLink!.getAttribute("rel")).toContain("noreferrer");
  });

  it("exports the selected insight", async () => {
    component = mount(InsightsPage, { target: document.body });
    await tick();

    const exportButton = Array.from(
      document.querySelectorAll<HTMLButtonElement>("button"),
    ).find((button) => button.textContent?.trim() === "Export");
    expect(exportButton).toBeDefined();

    exportButton!.click();
    await tick();

    expect(mocks.downloadInsightExport).toHaveBeenCalledWith(42);
  });

  it("opens the shared publish modal for the selected insight", async () => {
    component = mount(InsightsPage, { target: document.body });
    await tick();

    const publishButton = Array.from(
      document.querySelectorAll<HTMLButtonElement>("button"),
    ).find((button) => button.textContent?.trim() === "Publish");
    expect(publishButton).toBeDefined();

    publishButton!.click();
    await tick();

    expect(ui.activeModal).toBe("publish");
    expect(ui.publishSecret).toBe(false);
    expect(ui.publishTarget).toEqual({
      kind: "insight",
      id: 42,
    });
  });

  it("can target a secret insight publish", async () => {
    component = mount(InsightsPage, { target: document.body });
    await tick();

    const secretButton = Array.from(
      document.querySelectorAll<HTMLButtonElement>("button"),
    ).find((button) => button.textContent?.trim() === "Secret");
    expect(secretButton).toBeDefined();

    secretButton!.click();
    await tick();

    expect(ui.activeModal).toBe("publish");
    expect(ui.publishSecret).toBe(true);
    expect(ui.publishTarget).toEqual({
      kind: "insight",
      id: 42,
    });
  });

  it("keeps Generate enabled for pg serve when version advertises insight writes", async () => {
    syncState.serverVersion = {
      read_only: true,
      insight_generation_available: true,
    };
    component = mount(InsightsPage, { target: document.body });
    await tick();

    const generateButton = document.querySelector<HTMLButtonElement>(
      "button.generate-action",
    );
    expect(generateButton).toBeDefined();
    expect(generateButton!.disabled).toBe(false);
  });

  it("selects a generated insight without promoting default date intent", async () => {
    window.history.replaceState(null, "", "/insights");
    router.route = "insights";
    router.params = {};
    analytics.applyRollingWindow(365);
    yokedDates.setEnabled(true);
    vi.spyOn(analytics, "fetchSignalsForInsights").mockResolvedValue();

    component = mount(InsightsPage, { target: document.body });
    await flushEffects();

    const insightButton = document.querySelector<HTMLButtonElement>(
      ".generated-list button",
    );
    expect(insightButton).not.toBeNull();
    insightButton!.click();
    await flushEffects();

    expect(state.insightsStore.select).toHaveBeenCalledWith(42);
    expect(router.params.insight).toBe("42");
    expect(router.params.window_days).toBeUndefined();
    expect(yokedDates.range).toBeNull();
  });
});
