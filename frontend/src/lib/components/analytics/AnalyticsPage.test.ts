// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from "vitest";
import { mount, tick, unmount } from "svelte";
import { analytics } from "../../stores/analytics.svelte.js";
import { analyticsPageDates } from "../../stores/analyticsPageDates.js";
import { insights } from "../../stores/insights.svelte.js";
import { router } from "../../stores/router.svelte.js";
import { sessions } from "../../stores/sessions.svelte.js";
import { ui } from "../../stores/ui.svelte.js";
import { yokedDates } from "../../stores/yokedDates.svelte.js";
import sourceRaw from "./AnalyticsPage.svelte?raw";
// @ts-ignore
import AnalyticsPage from "./AnalyticsPage.svelte";
// @ts-ignore
import QualityPage from "../quality/QualityPage.svelte";

const source = sourceRaw.replace(/\r\n/g, "\n");

async function flushEffects() {
  await tick();
  await Promise.resolve();
  await tick();
}

async function selectRelativeRange(days: number) {
  const trigger = document.querySelector<HTMLButtonElement>(".kit-date-range-picker__trigger");
  expect(trigger).not.toBeNull();
  trigger!.click();
  await flushEffects();

  const preset = [...document.querySelectorAll<HTMLButtonElement>("button")].find(
    (button) => button.textContent?.trim() === `${days}d`,
  );
  expect(preset).not.toBeUndefined();
  preset!.click();
  await flushEffects();
}

let component: ReturnType<typeof mount> | undefined;

afterEach(() => {
  if (component) {
    unmount(component);
    component = undefined;
  }
  vi.restoreAllMocks();
  vi.useRealTimers();
  vi.unstubAllGlobals();
  document.body.innerHTML = "";
  localStorage.clear();
  window.history.replaceState(null, "", "/");
  router.route = "sessions";
  router.params = {};
  router.sessionId = null;
  router.isRootPath = false;
  analytics.isPinned = false;
  analytics.windowDays = 365;
  analytics.from = "";
  analytics.to = "";
  analytics.selectedDate = null;
  analytics.selectedDow = null;
  analytics.selectedHour = null;
  sessions.filters.date = "";
  sessions.filters.dateFrom = "";
  sessions.filters.dateTo = "";
  yokedDates.setEnabled(false);
  analyticsPageDates.clear();
  ui.sidebarOpen = true;
  ui.isMobileViewport = false;
});

describe("AnalyticsPage sidebar controls", () => {
  it("places the desktop expand control to the left of the relocated filter", async () => {
    vi.stubGlobal(
      "ResizeObserver",
      class {
        observe() {}
        disconnect() {}
      },
    );
    vi.spyOn(analytics, "fetchAll").mockResolvedValue();
    vi.spyOn(sessions, "load").mockResolvedValue();
    ui.sidebarOpen = false;
    ui.isMobileViewport = false;

    component = mount(AnalyticsPage, { target: document.body });
    await flushEffects();

    const anchor = document.querySelector<HTMLElement>(".toolbar-filter-anchor");
    const expandButton = document.querySelector<HTMLButtonElement>(
      'button[aria-label="Open sidebar"]',
    );
    const filterButton = anchor?.querySelector<HTMLButtonElement>(".filter-btn");

    expect(anchor).not.toBeNull();
    expect(expandButton).not.toBeNull();
    expect(filterButton).not.toBeNull();
    expect(expandButton?.nextElementSibling).toBe(filterButton);
    expect(expandButton?.title).toBe("Toggle sidebar (b)");

    expandButton!.click();
    await flushEffects();

    expect(ui.sidebarOpen).toBe(true);
  });

  it("leaves collapsed mobile sidebar controls in the title bar", async () => {
    vi.stubGlobal(
      "ResizeObserver",
      class {
        observe() {}
        disconnect() {}
      },
    );
    vi.spyOn(analytics, "fetchAll").mockResolvedValue();
    vi.spyOn(sessions, "load").mockResolvedValue();
    ui.sidebarOpen = false;
    ui.isMobileViewport = true;

    component = mount(AnalyticsPage, { target: document.body });
    await flushEffects();

    expect(document.querySelector(".toolbar-filter-anchor")).toBeNull();
    expect(document.querySelector('button[aria-label="Open sidebar"]')).toBeNull();
  });
});

describe("AnalyticsPage refresh behavior", () => {
  it("does not rematerialize rolling dates after cleared session date params", async () => {
    vi.useFakeTimers({ toFake: ["Date"] });
    vi.setSystemTime(new Date("2026-06-20T12:00:00"));
    vi.stubGlobal(
      "ResizeObserver",
      class {
        observe() {}
        disconnect() {}
      },
    );
    vi.spyOn(analytics, "fetchAll").mockResolvedValue();
    vi.spyOn(sessions, "load").mockResolvedValue();

    window.history.replaceState(
      null,
      "",
      "/sessions?window_days=30&date_from=2026-05-21&date_to=2026-06-20",
    );
    router.route = "sessions";
    router.sessionId = null;
    router.params = {
      window_days: "30",
      date_from: "2026-05-21",
      date_to: "2026-06-20",
    };
    analytics.windowDays = 30;
    analytics.isPinned = false;
    analytics.from = "2026-05-21";
    analytics.to = "2026-06-20";
    sessions.filters.date = "";
    sessions.filters.dateFrom = "2026-05-21";
    sessions.filters.dateTo = "2026-06-20";
    yokedDates.setEnabled(true);
    yokedDates.updateFromPanel({
      from: "2026-05-21",
      to: "2026-06-20",
      mode: "rolling",
      windowDays: 30,
    });

    component = mount(AnalyticsPage, { target: document.body });
    await flushEffects();

    sessions.filters.date = "";
    sessions.filters.dateFrom = "";
    sessions.filters.dateTo = "";
    router.params = {};
    await flushEffects();

    expect(sessions.filters.date).toBe("");
    expect(sessions.filters.dateFrom).toBe("");
    expect(sessions.filters.dateTo).toBe("");
    expect(yokedDates.range).toBeNull();

    unmount(component);
    component = mount(AnalyticsPage, { target: document.body });
    await flushEffects();

    const refresh = document.querySelector<HTMLButtonElement>(
      'button[aria-label="Refresh analytics"]',
    );
    expect(refresh).not.toBeNull();
    refresh!.click();
    await flushEffects();

    expect(sessions.filters.date).toBe("");
    expect(sessions.filters.dateFrom).toBe("");
    expect(sessions.filters.dateTo).toBe("");
    expect(yokedDates.range).toBeNull();
  });

  it("advances a restored rolling seed to the current day", async () => {
    const analyticsFetchStates: Array<{
      isPinned: boolean;
      windowDays: number;
      from: string;
      to: string;
    }> = [];
    const sessionLoadStates: typeof analyticsFetchStates = [];
    vi.useFakeTimers({ toFake: ["Date"] });
    vi.setSystemTime(new Date("2026-06-20T12:00:00"));
    vi.stubGlobal(
      "ResizeObserver",
      class {
        observe() {}
        disconnect() {}
      },
    );
    vi.spyOn(analytics, "fetchAll").mockImplementation(() => {
      analyticsFetchStates.push({
        isPinned: analytics.isPinned,
        windowDays: analytics.windowDays,
        from: analytics.from,
        to: analytics.to,
      });
      return Promise.resolve();
    });
    vi.spyOn(sessions, "load").mockImplementation(() => {
      sessionLoadStates.push({
        isPinned: analytics.isPinned,
        windowDays: analytics.windowDays,
        from: analytics.from,
        to: analytics.to,
      });
      return Promise.resolve();
    });
    router.route = "sessions";
    router.params = {};
    analytics.windowDays = 365;
    analytics.isPinned = false;
    analytics.from = "2025-06-21";
    analytics.to = "2026-06-20";
    yokedDates.setEnabled(true);
    yokedDates.updateFromPanel({
      from: "2026-05-22",
      to: "2026-06-20",
      mode: "rolling",
      windowDays: 30,
    });
    vi.setSystemTime(new Date("2026-06-21T12:00:00"));

    component = mount(AnalyticsPage, { target: document.body });
    await flushEffects();

    expect(analytics.isPinned).toBe(false);
    expect(analytics.windowDays).toBe(30);
    expect(analytics.from).toBe("2026-05-23");
    expect(analytics.to).toBe("2026-06-21");
    expect(sessions.filters.dateFrom).toBe("2026-05-23");
    expect(sessions.filters.dateTo).toBe("2026-06-21");
    expect(router.params).toMatchObject({
      window_days: "30",
      date_from: "2026-05-23",
      date_to: "2026-06-21",
    });
    expect(analyticsFetchStates[0]).toEqual({
      isPinned: false,
      windowDays: 30,
      from: "2026-05-23",
      to: "2026-06-21",
    });
    expect(sessionLoadStates[0]).toEqual({
      isPinned: false,
      windowDays: 30,
      from: "2026-05-23",
      to: "2026-06-21",
    });
  });

  it("retains independent Sessions and Quality ranges when linking is disabled", async () => {
    vi.useFakeTimers({ toFake: ["Date"] });
    vi.setSystemTime(new Date("2026-07-10T12:00:00"));
    vi.stubGlobal(
      "ResizeObserver",
      class {
        constructor(private callback: ResizeObserverCallback) {}
        observe(target: Element) {
          this.callback(
            [
              {
                target,
                contentRect: {
                  width: 600,
                  height: 200,
                  x: 0,
                  y: 0,
                  top: 0,
                  left: 0,
                  right: 600,
                  bottom: 200,
                  toJSON: () => ({}),
                },
              } as ResizeObserverEntry,
            ],
            this as unknown as ResizeObserver,
          );
        }
        unobserve() {}
        disconnect() {}
      },
    );
    vi.spyOn(analytics, "fetchAll").mockResolvedValue();
    vi.spyOn(analytics, "fetchSignalsForQuality").mockResolvedValue();
    vi.spyOn(sessions, "load").mockResolvedValue();
    vi.spyOn(sessions, "loadProjects").mockResolvedValue();
    vi.spyOn(sessions, "loadAgents").mockResolvedValue();
    vi.spyOn(insights, "load").mockResolvedValue();

    analytics.applyRollingWindow(365);
    router.route = "sessions";
    router.params = {};
    yokedDates.setEnabled(false);

    component = mount(AnalyticsPage, { target: document.body });
    await flushEffects();
    await selectRelativeRange(30);
    expect(analytics.windowDays).toBe(30);

    unmount(component);
    component = undefined;
    router.navigate("quality");
    component = mount(QualityPage, { target: document.body });
    await flushEffects();

    expect(analytics.windowDays).toBe(365);
    await selectRelativeRange(7);
    expect(analytics.windowDays).toBe(7);

    unmount(component);
    component = undefined;
    router.navigate("sessions");
    component = mount(AnalyticsPage, { target: document.body });
    await flushEffects();

    expect(analytics.windowDays).toBe(30);

    unmount(component);
    component = undefined;
    router.navigate("quality");
    component = mount(QualityPage, { target: document.body });
    await flushEffects();

    expect(analytics.windowDays).toBe(7);
  });

  it("does not refresh analytical scans from SSE updates", () => {
    expect(source).not.toContain("subscribeDebounced");
    // SSE only flags new data; the periodic refetch lives in RefreshControl.
    expect(source).toContain("analytics.markNewData");
    expect(source).toContain("events.subscribe");
  });

  it("delegates the refresh affordance to the shared RefreshControl", () => {
    expect(source).toContain("<RefreshControl");
    expect(source).toContain("analytics.lastUpdatedAt");
    // The scheduler, label tick, and icon now live in the shared component.
    expect(source).not.toContain("createRefreshScheduler");
    expect(source).not.toContain("REFRESH_INTERVAL_MS");
    expect(source).not.toContain("REFRESH_LABEL_INTERVAL_MS");
    expect(source).not.toContain("formatRefreshAge");
    expect(source).not.toContain("RefreshCwIcon");
    expect(source).not.toContain("setInterval");
  });

  it("shows relative last-updated status without ambiguous badges", () => {
    expect(source).not.toContain("formatUpdatedAt");
    expect(source).not.toContain("analytics.hasNewData");
    expect(source).not.toContain("New data");
    expect(source).not.toContain(".new-data");
  });

  it("keeps refresh progress out of content layout flow", () => {
    const queryProgress = source.match(/\.query-progress\s*{[^}]+}/)?.[0] ?? "";

    expect(queryProgress).toContain("position: absolute");
    expect(queryProgress).toContain("left: 0;");
    expect(queryProgress).toContain("right: 0;");
    expect(queryProgress).not.toContain("position: sticky");
    expect(queryProgress).not.toContain("margin:");
  });

  it("preserves rolling sessions analytics URLs with window_days", () => {
    expect(source).toContain('"window_days"');
    expect(source).toContain("parseSessionAnalyticsWindowDays");
    expect(source).toContain("writeSessionDateParams");
  });

  it("refreshes analytics through date-aware session writeback", () => {
    const helperStart = source.indexOf("function refreshAnalytics");
    const helperEnd = source.indexOf("\n\n  function handleActivityRangeSelect", helperStart);
    const helperBlock = source.slice(helperStart, helperEnd);

    expect(helperStart).toBeGreaterThan(-1);
    expect(helperBlock).toContain("analytics.fetchAll()");
    expect(helperBlock).toContain("writeSessionDateParams(state)");
    expect(source).toContain("onRefresh={refreshAnalytics}");
    expect(source).not.toContain("onRefresh={() => analytics.fetchAll()}");
  });

  it("applies URL and yoke dates before the initial analytics fetch", () => {
    const onMountIndex = source.indexOf("onMount(() =>");
    const firstEffectAfterMount = source.indexOf("$effect(() =>", onMountIndex);
    const onMountBlock = source.slice(onMountIndex, firstEffectAfterMount);

    expect(onMountBlock).not.toContain("analytics.fetchAll();");
    expect(source).toContain("const firstRun = !analyticsDateUrlInitRan");
    expect(source).toContain("if (changed || firstRun)");
  });

  it("uses an Activity brush as a Sessions filter without replacing the chart window", async () => {
    Object.defineProperty(HTMLElement.prototype, "clientWidth", {
      configurable: true,
      get: () => 600,
    });
    vi.stubGlobal(
      "ResizeObserver",
      class {
        observe() {}
        unobserve() {}
        disconnect() {}
      },
    );
    vi.spyOn(analytics, "fetchAll").mockResolvedValue();
    const loadSessions = vi.spyOn(sessions, "load").mockResolvedValue();
    const from = "2026-08-01";
    const to = "2026-08-10";
    window.history.replaceState(null, "", `/sessions?date_from=${from}&date_to=${to}`);
    router.isRootPath = false;
    router.params = { date_from: from, date_to: to };
    analytics.from = from;
    analytics.to = to;
    analytics.isPinned = true;
    analytics.granularity = "day";
    analytics.selectedActivityRange = null;
    analytics.errors.activity = null;
    sessions.filters.dateFrom = from;
    sessions.filters.dateTo = to;
    analytics.activity = {
      granularity: "day",
      series: Array.from({ length: 10 }, (_, index) => ({
        date: `2026-08-${String(index + 1).padStart(2, "0")}`,
        sessions: 1,
        messages: 2,
        user_messages: 1,
        assistant_messages: 1,
        tool_calls: 0,
        thinking_messages: 0,
        by_agent: {},
      })),
    };

    component = mount(AnalyticsPage, { target: document.body });
    await flushEffects();
    await vi.waitFor(() => {
      expect(document.querySelector(".lc-brush-context")).not.toBeNull();
    });
    const brush = document.querySelector<HTMLElement>(".lc-brush-context");
    expect(brush).not.toBeNull();
    vi.spyOn(brush!, "getBoundingClientRect").mockReturnValue({
      x: 0,
      y: 0,
      left: 0,
      top: 0,
      right: 552,
      bottom: 124,
      width: 552,
      height: 124,
      toJSON: () => ({}),
    });
    brush!.dispatchEvent(
      new MouseEvent("pointerdown", {
        bubbles: true,
        clientX: 120,
        clientY: 40,
      }),
    );
    window.dispatchEvent(
      new MouseEvent("pointermove", {
        bubbles: true,
        clientX: 300,
        clientY: 40,
      }),
    );
    brush!.dispatchEvent(
      new MouseEvent("pointerup", {
        bubbles: true,
        clientX: 300,
        clientY: 40,
      }),
    );
    await flushEffects();

    expect(analytics.from).toBe(from);
    expect(analytics.to).toBe(to);
    expect(sessions.filters.dateFrom).toBe("2026-08-03");
    expect(sessions.filters.dateTo).toBe("2026-08-06");
    expect(router.params).toEqual({ date_from: from, date_to: to });
    expect(loadSessions).toHaveBeenCalled();
  });

  it("only seeds saved yoke dates during initial URL hydration", () => {
    const seedIndex = source.indexOf("yokedDates.seedForPanel()");
    const firstRunIndex = source.indexOf("if (firstRun) {");

    expect(seedIndex).toBeGreaterThan(-1);
    expect(firstRunIndex).toBeGreaterThan(-1);
    expect(seedIndex).toBeGreaterThan(firstRunIndex);
    expect(source).toContain("if (router.isRootPath)");
  });

  it("treats drill-down clears as analytics date changes", () => {
    const signatureStart = source.indexOf("function analyticsPanelDateSignature");
    const signatureEnd = source.indexOf("\n\n  function applyAnalyticsPanelDate", signatureStart);
    const signatureBlock = source.slice(signatureStart, signatureEnd);
    const applyStart = source.indexOf("function applyAnalyticsPanelDate");
    const applyEnd = source.indexOf("\n\n  function handleActivityRangeSelect", applyStart);
    const applyBlock = source.slice(applyStart, applyEnd);

    expect(signatureStart).toBeGreaterThan(-1);
    expect(signatureBlock).toContain("selectedDate: analytics.selectedDate");
    expect(signatureBlock).toContain("selectedDow: analytics.selectedDow");
    expect(signatureBlock).toContain("selectedHour: analytics.selectedHour");
    expect(applyBlock).toContain("const before = analyticsPanelDateSignature();");
    expect(applyBlock).toContain("const after = analyticsPanelDateSignature();");
  });

  it("only applies analytics URL dates when the date signature changes", () => {
    const helperStart = source.indexOf("function sessionAnalyticsDateUrlSignature");
    const helperEnd = source.indexOf("function clearSessionDateFilters", helperStart);
    const helperBlock = source.slice(helperStart, helperEnd);
    const effectStart = source.indexOf("const dateSignature =");
    const effectEnd = source.indexOf("onDestroy(() => {", effectStart);
    const effectBlock = source.slice(effectStart, effectEnd);

    expect(helperStart).toBeGreaterThan(-1);
    expect(helperBlock).toContain("state.mode");
    expect(helperBlock).toContain("state.windowDays");
    expect(helperBlock).toContain("from: state.from");
    expect(helperBlock).toContain("to: state.to");
    expect(source).toContain("syncSessionFiltersForDateState(state)");
    expect(source).toContain("let lastAnalyticsDateUrlSignature: string | null = $state(null);");
    expect(effectBlock).toContain(
      "const dateChanged = firstRun ||\n        lastAnalyticsDateUrlSignature !== dateSignature;",
    );
    expect(effectBlock).toContain("if (dateChanged) {");
    expect(effectBlock).toContain("changed = applyAnalyticsPanelDate(state);");
    expect(effectBlock).toContain("lastAnalyticsDateUrlSignature = dateSignature;");
  });

  it("does not use the rolling fallback when cleared session date filters remove URL dates", () => {
    const noStateStart = source.indexOf("if (!state) {");
    const noStateEnd = source.indexOf(
      "let changed = false;\n      let sessionChanged = false;",
      noStateStart,
    );
    const noStateBlock = source.slice(noStateStart, noStateEnd);

    expect(noStateBlock).toContain("dateChanged && sessionDateFiltersAreClear()");
    expect(noStateBlock).toContain("yokedDates.clear();");
    expect(noStateBlock).toContain("} else if (dateChanged) {");
    expect(noStateBlock).toContain("state = rollingPanelDate(analytics.windowDays);");
    expect(noStateBlock).toContain("changed = applyAnalyticsPanelDate(state);");
  });
});
