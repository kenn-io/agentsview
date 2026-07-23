// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from "vite-plus/test";
import { fireEvent, screen } from "@testing-library/svelte";
import { mount, tick, unmount } from "svelte";
import { activity } from "../../stores/activity.svelte.js";
import { router } from "../../stores/router.svelte.js";
import { yokedDates } from "../../stores/yokedDates.svelte.js";
import source from "./ActivityPage.svelte?raw";
// @ts-ignore
import ActivityPage from "./ActivityPage.svelte";
import type { Report } from "../../api/types.js";

async function flushEffects() {
  await tick();
  await Promise.resolve();
  await tick();
}

function stubActivityPageCollaborators() {
  vi.stubGlobal(
    "ResizeObserver",
    class {
      observe() {}
      disconnect() {}
    },
  );
  vi.spyOn(activity, "attach").mockReturnValue(() => {});
  vi.spyOn(activity, "loadFilterOptions").mockResolvedValue(true);
  vi.spyOn(activity, "load").mockResolvedValue(true);
}

async function openCalendar(triggerLabel: string) {
  await fireEvent.click(screen.getByRole("button", { name: triggerLabel }));
  await fireEvent.click(screen.getByRole("radio", { name: "Calendar" }));
}

function projectReport(): Report {
  return {
    timezone: "UTC",
    range_start: "2026-07-01T00:00:00Z",
    range_end: "2026-07-02T00:00:00Z",
    bucket_unit: "hour",
    bucket_seconds: 3600,
    bucket_count: 24,
    partial: false,
    as_of: null,
    effective_end: "2026-07-02T00:00:00Z",
    elapsed_bucket_count: 24,
    buckets: [],
    peak: { agents: 0, at: null },
    totals: {
      active_minutes: 0,
      idle_minutes: 0,
      agent_minutes: 20,
      sessions: 1,
      untimed_sessions: 0,
      distinct_projects: 1,
      distinct_models: 0,
      output_tokens: 0,
      cost: 0,
      automated_agent_minutes: 0,
      automated_cost: 0,
      automated_sessions: 0,
      interactive_agent_minutes: 20,
      interactive_cost: 0,
      interactive_sessions: 1,
    },
    by_project: [{
      key: "wrong-project",
      project_key: "pl1:sha256:wrong",
      agent_minutes: 20,
      cost: 0,
      interactive_agent_minutes: 20,
      automated_agent_minutes: 0,
      interactive_cost: 0,
      automated_cost: 0,
    }],
    by_model: [],
    by_agent: [],
    by_session: [],
    intervals: [],
    projects: {},
  } as Report;
}

function calendarDay(label: string): HTMLButtonElement {
  return screen.getByRole("button", { name: label }) as HTMLButtonElement;
}

describe("ActivityPage refresh control layout", () => {
  it("keeps the shared refresh control inline with the toolbar filters", () => {
    expect(source).toContain("<RefreshControl");
    expect(source).toContain("activity.lastUpdatedAt");
    expect(source).not.toContain("refresh-slot");
    expect(source).not.toContain("margin-left: auto");
  });
});

describe("ActivityPage breakdown links", () => {
  let component: ReturnType<typeof mount> | undefined;

  afterEach(() => {
    if (component) unmount(component);
    component = undefined;
    vi.restoreAllMocks();
    vi.unstubAllGlobals();
    document.body.innerHTML = "";
    activity.report = null;
  });

  it("renders project rows as Data links with no reclassify controls", async () => {
    stubActivityPageCollaborators();
    activity.report = projectReport();

    component = mount(ActivityPage, { target: document.body });
    await flushEffects();

    expect(
      document.body.querySelector("button[aria-label^=\"Reclassify\"]"),
    ).toBeNull();
    const link = document.body.querySelector("a.bar-label") as HTMLAnchorElement;
    expect(link).toBeTruthy();
    expect(link.getAttribute("href")).toBe(
      "/data?project_key=pl1%3Asha256%3Awrong",
    );
    expect(link.getAttribute("title")).toBe("View wrong-project in Data");
  });
});

describe("ActivityPage date yoke controls", () => {
  it("updates shared yoke state from the unified range picker", () => {
    expect(source).toContain("<RangePicker");
    expect(source).toContain("seedActivityYoke");
    expect(source).toContain("yokedDates.updateFromPanel");
  });

  it("yokes week and month selections using resolved period starts", () => {
    expect(source).toContain("startOfIsoWeek(activity.date)");
    expect(source).toContain("startOfMonth(activity.date)");
    expect(source).not.toContain("panelDateState(activity.date, addDays(activity.date, 6)");
    expect(source).not.toContain("panelDateState(activity.date, endOfMonth(activity.date)");
  });

  it("preserves relative range selections as rolling yoke state", () => {
    const applyIndex = source.indexOf("function applyRange");
    const helperIndex = source.indexOf("function yokeStateForSelection");
    const applyBlock = source.slice(applyIndex, helperIndex);

    expect(helperIndex).toBeGreaterThan(applyIndex);
    expect(source).toContain('mode: "rolling"');
    expect(source).toContain("windowDays: sel.days");
    expect(applyBlock).toContain("yokeStateForSelection(sel, range)");
    expect(applyBlock).toContain("lastActivityDateSignature = dateSignature");
  });

  it("preserves rolling window intent in activity URLs", () => {
    expect(source).toContain("activity.rollingWindowDays");
    expect(source).toContain("activity.setCustomRange");
    expect(source).toContain("params.window_days");
    expect(source).toContain('mode: "relative", days: activity.rollingWindowDays');
  });
});

describe("ActivityPage date yoke integration", () => {
  let component: ReturnType<typeof mount> | undefined;

  afterEach(() => {
    if (component) {
      unmount(component);
      component = undefined;
    }
    vi.restoreAllMocks();
    vi.unstubAllGlobals();
    document.body.innerHTML = "";
    window.history.replaceState(null, "", "/");
    router.route = "sessions";
    router.params = {};
    activity.preset = "day";
    activity.from = "";
    activity.to = "";
    activity.rollingWindowDays = null;
    yokedDates.setEnabled(false);
    localStorage.clear();
    vi.useRealTimers();
  });

  it("seeds bare Activity from an enabled representable fixed range", async () => {
    const loadStates: Array<{
      preset: string;
      from: string;
      to: string;
    }> = [];
    vi.stubGlobal(
      "ResizeObserver",
      class {
        observe() {}
        disconnect() {}
      },
    );
    vi.spyOn(activity, "attach").mockReturnValue(() => {});
    vi.spyOn(activity, "loadFilterOptions").mockResolvedValue(true);
    vi.spyOn(activity, "load").mockImplementation(() => {
      loadStates.push({
        preset: activity.preset,
        from: activity.from,
        to: activity.to,
      });
      return Promise.resolve(true);
    });
    router.route = "activity";
    router.params = {};
    activity.preset = "day";
    activity.from = "";
    activity.to = "";
    activity.rollingWindowDays = null;
    yokedDates.setEnabled(true);
    yokedDates.updateFromPanel({
      from: "2026-06-01",
      to: "2026-06-07",
      mode: "fixed",
    });

    component = mount(ActivityPage, { target: document.body });
    await flushEffects();

    expect(activity.preset).toBe("custom");
    expect(activity.from).toBe("2026-06-01");
    expect(activity.to).toBe("2026-06-07");
    expect(loadStates[0]).toEqual({
      preset: "custom",
      from: "2026-06-01",
      to: "2026-06-07",
    });
  });
});

describe("ActivityPage calendar day rollover", () => {
  let component: ReturnType<typeof mount> | undefined;

  afterEach(() => {
    if (component) {
      unmount(component);
      component = undefined;
    }
    vi.restoreAllMocks();
    vi.unstubAllGlobals();
    vi.useRealTimers();
    document.body.innerHTML = "";
    window.history.replaceState(null, "", "/");
    router.route = "sessions";
    router.params = {};
    activity.preset = "day";
    activity.date = "";
    activity.from = "";
    activity.to = "";
    activity.rollingWindowDays = null;
    activity.report = null;
    activity.loading = false;
    activity.error = null;
    yokedDates.setEnabled(false);
    localStorage.clear();
  });

  it("synchronizes the current day when mounting crosses midnight", async () => {
    vi.useFakeTimers();
    vi.setSystemTime(new Date(2026, 5, 17, 23, 59, 59, 999));
    stubActivityPageCollaborators();
    router.route = "activity";
    activity.date = "2026-06-17";

    component = mount(ActivityPage, { target: document.body });
    vi.setSystemTime(new Date(2026, 5, 18, 0, 0, 0));
    await flushEffects();
    await openCalendar("Jun 17, 2026");

    const june18 = calendarDay("Jun 18, 2026");
    expect(june18.disabled).toBe(false);
    await fireEvent.click(june18);
    expect(activity.date).toBe("2026-06-18");
  });

  it("enables each new local day at midnight without remounting", async () => {
    vi.useFakeTimers();
    vi.setSystemTime(new Date(2026, 5, 17, 23, 59, 59, 500));
    stubActivityPageCollaborators();
    router.route = "activity";
    activity.date = "2026-06-17";

    component = mount(ActivityPage, { target: document.body });
    await flushEffects();
    await openCalendar("Jun 17, 2026");

    const june18 = calendarDay("Jun 18, 2026");
    const june19 = calendarDay("Jun 19, 2026");
    expect(june18.disabled).toBe(true);
    expect(june19.disabled).toBe(true);

    await vi.advanceTimersByTimeAsync(500);
    await flushEffects();

    expect(june18.disabled).toBe(false);
    await fireEvent.click(june18);
    expect(activity.date).toBe("2026-06-18");
    expect(june19.disabled).toBe(true);

    await vi.advanceTimersByTimeAsync(24 * 60 * 60 * 1000);
    await flushEffects();

    expect(june19.disabled).toBe(false);
    await fireEvent.click(june19);
    expect(activity.date).toBe("2026-06-19");

    expect(vi.getTimerCount()).toBeGreaterThan(0);
    unmount(component);
    component = undefined;
    expect(vi.getTimerCount()).toBe(0);
  });

  it("catches up to the current local day after a delayed timeout", async () => {
    vi.useFakeTimers();
    vi.setSystemTime(new Date(2026, 4, 30, 23, 59, 59, 500));
    stubActivityPageCollaborators();
    router.route = "activity";
    activity.date = "2026-05-30";

    component = mount(ActivityPage, { target: document.body });
    await flushEffects();
    await openCalendar("May 30, 2026");

    const june3 = calendarDay("Jun 3, 2026");
    expect(june3.disabled).toBe(true);

    vi.setSystemTime(new Date(2026, 5, 3, 12, 0, 0));
    await vi.advanceTimersByTimeAsync(500);
    await flushEffects();

    expect(june3.disabled).toBe(false);
    await fireEvent.click(june3);
    expect(activity.date).toBe("2026-06-03");
  });
});
