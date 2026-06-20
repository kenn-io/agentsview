// @vitest-environment jsdom
import {
  afterEach,
  describe,
  expect,
  it,
  vi,
} from "vite-plus/test";
import { mount, tick, unmount } from "svelte";
import { analytics } from "../../stores/analytics.svelte.js";
import { router } from "../../stores/router.svelte.js";
import { sessions } from "../../stores/sessions.svelte.js";
import { yokedDates } from "../../stores/yokedDates.svelte.js";
import source from "./AnalyticsPage.svelte?raw";
// @ts-ignore
import AnalyticsPage from "./AnalyticsPage.svelte";

async function flushEffects() {
  await tick();
  await Promise.resolve();
  await tick();
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
  yokedDates.clear();
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
    const queryProgress =
      source.match(/\.query-progress\s*{[^}]+}/)?.[0] ?? "";

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
    const helperEnd = source.indexOf(
      "\n\n  function handleDateRangeChange",
      helperStart,
    );
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

  it("routes timeline range selections through the shared date-change path", () => {
    expect(source).toContain(
      "<ActivityTimeline onDateRangeChange={handleDateRangeChange} />",
    );
  });

  it("only seeds saved yoke dates during initial URL hydration", () => {
    const seedIndex = source.indexOf("const seed = yokedDates.seedForPanel()");
    const guardedSeedIndex = source.indexOf(
      "if (firstRun) {\n          const seed = yokedDates.seedForPanel()",
    );

    expect(seedIndex).toBeGreaterThan(-1);
    expect(guardedSeedIndex).toBeGreaterThan(-1);
  });

  it("treats drill-down clears as analytics date changes", () => {
    const signatureStart = source.indexOf(
      "function analyticsPanelDateSignature",
    );
    const signatureEnd = source.indexOf(
      "\n\n  function applyAnalyticsPanelDate",
      signatureStart,
    );
    const signatureBlock = source.slice(signatureStart, signatureEnd);
    const applyStart = source.indexOf("function applyAnalyticsPanelDate");
    const applyEnd = source.indexOf(
      "\n\n  function handleDateRangeChange",
      applyStart,
    );
    const applyBlock = source.slice(applyStart, applyEnd);

    expect(signatureStart).toBeGreaterThan(-1);
    expect(signatureBlock).toContain("selectedDate: analytics.selectedDate");
    expect(signatureBlock).toContain("selectedDow: analytics.selectedDow");
    expect(signatureBlock).toContain("selectedHour: analytics.selectedHour");
    expect(applyBlock).toContain(
      "const before = analyticsPanelDateSignature();",
    );
    expect(applyBlock).toContain(
      "const after = analyticsPanelDateSignature();",
    );
  });

  it("only applies analytics URL dates when the date signature changes", () => {
    const helperStart = source.indexOf(
      "function sessionAnalyticsDateUrlSignature",
    );
    const helperEnd = source.indexOf(
      "\n\n  function clearSessionDateFilters",
      helperStart,
    );
    const helperBlock = source.slice(helperStart, helperEnd);
    const effectStart = source.indexOf("const dateSignature =");
    const effectEnd = source.indexOf(
      "\n\n  onDestroy",
      effectStart,
    );
    const effectBlock = source.slice(effectStart, effectEnd);

    expect(helperStart).toBeGreaterThan(-1);
    expect(helperBlock).toContain("state.mode");
    expect(helperBlock).toContain("state.windowDays");
    expect(helperBlock).toContain("from: state.from");
    expect(helperBlock).toContain("to: state.to");
    expect(source).toContain("syncSessionFiltersForDateState(state)");
    expect(source).toContain(
      "let lastAnalyticsDateUrlSignature: string | null = $state(null);",
    );
    expect(effectBlock).toContain(
      "const dateChanged = firstRun ||\n        lastAnalyticsDateUrlSignature !== dateSignature;",
    );
    expect(effectBlock).toContain("if (dateChanged) {");
    expect(effectBlock).toContain("changed = applyAnalyticsPanelDate(state);");
    expect(effectBlock).toContain(
      "lastAnalyticsDateUrlSignature = dateSignature;",
    );
  });

  it("does not use the rolling fallback when cleared session date filters remove URL dates", () => {
    const noStateStart = source.indexOf("if (!state) {");
    const noStateEnd = source.indexOf(
      "\n\n      let changed = false;\n      let sessionChanged = false;",
      noStateStart,
    );
    const noStateBlock = source.slice(noStateStart, noStateEnd);

    expect(noStateBlock).toContain(
      "dateChanged && sessionDateFiltersAreClear()",
    );
    expect(noStateBlock).toContain("yokedDates.clear();");
    expect(noStateBlock).toContain("} else if (dateChanged) {");
    expect(noStateBlock).toContain(
      "state = rollingPanelDate(analytics.windowDays);",
    );
    expect(noStateBlock).toContain(
      "changed = applyAnalyticsPanelDate(state);",
    );
  });
});
