import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import type { Report } from "../api/types/activity.js";

const api = vi.hoisted(() => ({
  getActivityReport: vi.fn(),
  getProjects: vi.fn(),
  getAgents: vi.fn(),
  getMachines: vi.fn(),
}));

vi.mock("../api/generated/index", () => ({
  ActivityService: { getApiV1ActivityReport: api.getActivityReport },
  MetadataService: {
    getApiV1Projects: api.getProjects,
    getApiV1Agents: api.getAgents,
    getApiV1Machines: api.getMachines,
  },
}));
vi.mock("../api/runtime.js", () => ({ configureGeneratedClient: vi.fn() }));
vi.mock("./sync.svelte.js", () => ({ sync: { onSyncComplete: vi.fn() } }));
vi.mock("./router.svelte.js", () => ({
  router: { params: {}, replaceParams: vi.fn(), route: "activity" },
}));

import { activity } from "./activity.svelte.js";
import { sync } from "./sync.svelte.js";
import * as routerMod from "./router.svelte.js";

// activity.svelte.ts registers its sync hook once, at import time. Capture the
// callback now, before beforeEach resets the recorded call, so the
// sync-refresh tests can invoke it directly.
const syncCallback = vi.mocked(sync.onSyncComplete).mock.calls[0]?.[0];

function makeReport(overrides: Partial<Report> = {}): Report {
  return {
    timezone: "UTC",
    range_start: "2026-06-16T00:00:00Z",
    range_end: "2026-06-17T00:00:00Z",
    bucket_unit: "minute",
    bucket_seconds: 300,
    bucket_count: 288,
    partial: false,
    as_of: null,
    effective_end: "2026-06-17T00:00:00Z",
    elapsed_bucket_count: 288,
    buckets: [],
    peak: { agents: 0, at: null },
    totals: {
      active_minutes: 0,
      idle_minutes: 1440,
      agent_minutes: 0,
      sessions: 0,
      untimed_sessions: 0,
      distinct_projects: 0,
      distinct_models: 0,
      output_tokens: 0,
      cost: 0,
    },
    by_project: [],
    by_model: [],
    by_agent: [],
    by_session: [],
    intervals: [],
    ...overrides,
  } as Report;
}

// Holds an ActivityPage attachment for tests that register one, so afterEach
// can release it and keep the shared singleton's attach count isolated.
let detach: (() => void) | null = null;

beforeEach(() => {
  api.getActivityReport.mockReset();
  api.getProjects.mockReset();
  api.getAgents.mockReset();
  api.getMachines.mockReset();
  api.getProjects.mockResolvedValue({ projects: [] });
  api.getAgents.mockResolvedValue({ agents: [] });
  api.getMachines.mockResolvedValue({ machines: [] });
  activity.report = null;
  activity.loading = false;
  activity.error = null;
  activity.lastUpdatedAt = null;
  activity.hasNewData = false;
  activity.projects = [];
  activity.agents = [];
  activity.machines = [];
  activity.setPreset("day");
  activity.setDate("2026-06-16");
  activity.setProject("");
  activity.setAgent("");
  activity.setMachine("");
  activity.setAutomation("all");
  // Reset the filter-option cache so each test exercises the fetch.
  activity.invalidateFilterOptions();
  // Restore a fresh router.replaceParams spy. The writeUrl test reassigns it,
  // so reset here to keep that reassignment from leaking into later tests.
  (
    routerMod.router as unknown as { replaceParams: ReturnType<typeof vi.fn> }
  ).replaceParams = vi.fn();
});
afterEach(() => {
  // Release any ActivityPage attachment so the singleton's attach count does
  // not leak into the next test's sync-refresh assertions.
  detach?.();
  detach = null;
});

describe("load", () => {
  it("sends preset/date/timezone and stores the report", async () => {
    api.getActivityReport.mockResolvedValue(makeReport());
    await activity.load();
    expect(api.getActivityReport).toHaveBeenCalledTimes(1);
    const arg = api.getActivityReport.mock.calls[0]![0];
    expect(arg.preset).toBe("day");
    expect(arg.date).toBe("2026-06-16");
    expect(typeof arg.timezone).toBe("string");
    expect(activity.report?.range_start).toBe("2026-06-16T00:00:00Z");
    expect(activity.error).toBeNull();
  });

  it("passes project/agent/machine filters", async () => {
    api.getActivityReport.mockResolvedValue(makeReport());
    activity.setProject("p1");
    activity.setAgent("claude");
    activity.setMachine("m1");
    await activity.load();
    const arg = api.getActivityReport.mock.calls.at(-1)![0];
    expect(arg.project).toBe("p1");
    expect(arg.agent).toBe("claude");
    expect(arg.machine).toBe("m1");
  });

  it("defaults the automation class to all", async () => {
    api.getActivityReport.mockResolvedValue(makeReport());
    await activity.load();
    expect(api.getActivityReport.mock.calls.at(-1)![0].automation).toBe("all");
  });

  it("passes the selected automation class", async () => {
    api.getActivityReport.mockResolvedValue(makeReport());
    activity.setAutomation("automated");
    await activity.load();
    expect(api.getActivityReport.mock.calls.at(-1)![0].automation).toBe(
      "automated",
    );
  });

  it("ignores a stale response when params change mid-flight", async () => {
    let resolveFirst!: (r: Report) => void;
    api.getActivityReport.mockImplementationOnce(
      () =>
        new Promise<Report>((r) => {
          resolveFirst = r;
        }),
    );
    const p1 = activity.load();
    activity.setDate("2026-06-17");
    api.getActivityReport.mockResolvedValueOnce(
      makeReport({ range_start: "2026-06-17T00:00:00Z" }),
    );
    await activity.load();
    resolveFirst(makeReport({ range_start: "2026-06-16T00:00:00Z" }));
    await p1;
    expect(activity.report?.range_start).toBe("2026-06-17T00:00:00Z");
  });

  it("uses a fallback message for non-Error rejections", async () => {
    api.getActivityReport.mockRejectedValueOnce("boom");
    await activity.load();
    expect(activity.error).toBe("Failed to load activity report");
    expect(activity.report).toBeNull();
    expect(activity.loading).toBe(false);
  });

  it("surfaces the message for Error rejections", async () => {
    api.getActivityReport.mockRejectedValueOnce(new Error("network down"));
    await activity.load();
    expect(activity.error).toBe("network down");
    expect(activity.report).toBeNull();
    expect(activity.loading).toBe(false);
  });

  it("keeps the existing report when a background refresh fails", async () => {
    api.getActivityReport.mockResolvedValueOnce(makeReport());
    await activity.load();
    const loadedReport = activity.report;
    const stampedAt = activity.lastUpdatedAt;
    expect(loadedReport).not.toBeNull();

    api.getActivityReport.mockRejectedValueOnce(new Error("network down"));
    await activity.load({ background: true });

    // Report-first rendering depends on the report staying mounted: a transient
    // background refresh failure must not blank the dashboard or surface an
    // error, and must not bump the freshness stamp (the age label keeps growing
    // to signal staleness). A non-background load still clears on error (above).
    expect(activity.report).toBe(loadedReport);
    expect(activity.error).toBeNull();
    expect(activity.loading).toBe(false);
    expect(activity.lastUpdatedAt).toBe(stampedAt);
  });

  it("recomputes rolling custom ranges before refreshing", async () => {
    vi.useFakeTimers({ toFake: ["Date"] });
    try {
      vi.setSystemTime(new Date("2026-06-19T12:00:00"));
      activity.setCustomRange("2026-05-20", "2026-06-19", 30);
      const replaceParams =
        routerMod.router.replaceParams as ReturnType<typeof vi.fn>;
      replaceParams.mockClear();
      api.getActivityReport.mockResolvedValue(makeReport());

      vi.setSystemTime(new Date("2026-06-20T12:00:00"));
      await activity.load({ background: true });

      const arg = api.getActivityReport.mock.calls.at(-1)![0];
      expect(arg.from).toBe(
        new Date("2026-05-21T00:00:00").toISOString(),
      );
      expect(arg.to).toBe(
        new Date("2026-06-21T00:00:00").toISOString(),
      );
      expect(activity.from).toBe("2026-05-21");
      expect(activity.to).toBe("2026-06-20");
      expect(replaceParams.mock.calls.at(-1)?.[0]).toMatchObject({
        preset: "custom",
        from: "2026-05-21",
        to: "2026-06-20",
        window_days: "30",
      });
    } finally {
      vi.useRealTimers();
    }
  });

  it("surfaces the error when a background refresh fails before any report", async () => {
    // No successful load yet, so report is null and there is nothing to keep.
    api.getActivityReport.mockRejectedValueOnce(new Error("first load down"));
    await activity.load();
    expect(activity.report).toBeNull();
    expect(activity.error).toBe("first load down");

    // A refresh (toolbar button or scheduler) firing before any report exists
    // must still surface the failure: background mode degrades to foreground
    // when report is null, instead of clearing then suppressing the error and
    // leaving a misleading "No data" state.
    api.getActivityReport.mockRejectedValueOnce(new Error("refresh down"));
    await activity.load({ background: true });
    expect(activity.report).toBeNull();
    expect(activity.error).toBe("refresh down");
    expect(activity.loading).toBe(false);
  });
});

describe("loadFilterOptions", () => {
  it("fetches options with one-shot + automated included and stores them", async () => {
    api.getProjects.mockResolvedValueOnce({
      projects: [{ name: "proj-a", count: 1 }],
    });
    api.getAgents.mockResolvedValueOnce({
      agents: [{ name: "claude", count: 2 }],
    });
    api.getMachines.mockResolvedValueOnce({
      machines: ["laptop", "desktop"],
    });

    await activity.loadFilterOptions();

    const full = { includeOneShot: true, includeAutomated: true };
    expect(api.getProjects).toHaveBeenCalledWith(full);
    expect(api.getAgents).toHaveBeenCalledWith(full);
    expect(api.getMachines).toHaveBeenCalledWith(full);

    expect(activity.projects).toEqual([{ name: "proj-a", count: 1 }]);
    expect(activity.agents).toEqual([{ name: "claude", count: 2 }]);
    expect(activity.machines).toEqual(["laptop", "desktop"]);
  });

  it("fetches once across repeated calls", async () => {
    api.getProjects.mockResolvedValue({ projects: [] });
    api.getAgents.mockResolvedValue({ agents: [] });
    api.getMachines.mockResolvedValue({ machines: [] });

    await activity.loadFilterOptions();
    await activity.loadFilterOptions();

    expect(api.getProjects).toHaveBeenCalledTimes(1);
    expect(api.getAgents).toHaveBeenCalledTimes(1);
    expect(api.getMachines).toHaveBeenCalledTimes(1);
  });

  it("leaves lists empty when a fetch fails", async () => {
    api.getProjects.mockRejectedValueOnce(new Error("boom"));
    api.getAgents.mockResolvedValueOnce({ agents: [] });
    api.getMachines.mockResolvedValueOnce({ machines: [] });

    await activity.loadFilterOptions();

    expect(activity.projects).toEqual([]);
  });

  it("retries on the next call after a transient failure", async () => {
    api.getProjects
      .mockRejectedValueOnce(new Error("boom"))
      .mockResolvedValueOnce({ projects: [{ name: "proj-a", count: 1 }] });
    api.getAgents.mockResolvedValue({ agents: [] });
    api.getMachines.mockResolvedValue({ machines: [] });

    await activity.loadFilterOptions();
    expect(activity.projects).toEqual([]); // projects failed, not cached

    await activity.loadFilterOptions(); // un-cached load retries
    expect(api.getProjects).toHaveBeenCalledTimes(2);
    expect(activity.projects).toEqual([{ name: "proj-a", count: 1 }]);
  });

  it("refetches after invalidateFilterOptions", async () => {
    api.getProjects.mockResolvedValue({ projects: [] });
    api.getAgents.mockResolvedValue({ agents: [] });
    api.getMachines.mockResolvedValue({ machines: [] });

    await activity.loadFilterOptions();
    expect(api.getProjects).toHaveBeenCalledTimes(1);

    // Mirrors the sync.onSyncComplete hook: a completed sync drops the
    // cache so newly imported projects/agents/machines are picked up.
    activity.invalidateFilterOptions();
    await activity.loadFilterOptions();
    expect(api.getProjects).toHaveBeenCalledTimes(2);
  });
});

describe("sync refresh hook", () => {
  it("refetches options on sync while an ActivityPage is attached", async () => {
    expect(typeof syncCallback).toBe("function");
    api.getActivityReport.mockResolvedValue(makeReport());
    api.getProjects.mockResolvedValue({ projects: [] });
    api.getAgents.mockResolvedValue({ agents: [] });
    api.getMachines.mockResolvedValue({ machines: [] });

    detach = activity.attach();
    await activity.loadFilterOptions();
    expect(api.getProjects).toHaveBeenCalledTimes(1);

    // A completed sync runs the registered hook: invalidate + refetch. The
    // refetch calls getProjects synchronously (before its first await), so
    // the count bumps immediately -- without any explicit reload here. An
    // invalidate-only hook would leave it at 1.
    syncCallback?.();
    expect(api.getProjects).toHaveBeenCalledTimes(2);

    // Settle the in-flight refetch the hook started.
    await activity.loadFilterOptions();
  });

  it("does not reload the report on sync, only flags new data via SSE", async () => {
    api.getActivityReport.mockResolvedValue(makeReport());
    api.getProjects.mockResolvedValue({ projects: [] });
    api.getAgents.mockResolvedValue({ agents: [] });
    api.getMachines.mockResolvedValue({ machines: [] });

    detach = activity.attach();
    await activity.load();
    expect(api.getActivityReport).toHaveBeenCalledTimes(1);

    // The sync hook refreshes filter options but must NOT reload the report:
    // re-aggregating on every sync thrashes the CPU while a session is active.
    // Refetching is bounded to the RefreshControl scheduler and manual button;
    // SSE only flags new data. load() calls getActivityReport synchronously, so
    // an errant reload would already show here.
    syncCallback?.();
    expect(api.getActivityReport).toHaveBeenCalledTimes(1);

    // Settle the in-flight options refetch the hook started.
    await activity.loadFilterOptions();
  });

  it("invalidates without refetching on sync when no page is attached", async () => {
    api.getActivityReport.mockResolvedValue(makeReport());
    api.getProjects.mockResolvedValue({ projects: [] });
    api.getAgents.mockResolvedValue({ agents: [] });
    api.getMachines.mockResolvedValue({ machines: [] });

    await activity.loadFilterOptions();
    expect(api.getProjects).toHaveBeenCalledTimes(1);

    // No ActivityPage attached: the hook invalidates but must not fetch options
    // or reload the report on its own. Both calls run synchronously before their
    // first await, so an errant refetch/reload would already show here.
    syncCallback?.();
    expect(api.getProjects).toHaveBeenCalledTimes(1);
    expect(api.getActivityReport).not.toHaveBeenCalled();

    // The invalidation took effect: the next explicit load refetches.
    await activity.loadFilterOptions();
    expect(api.getProjects).toHaveBeenCalledTimes(2);
  });
});

describe("freshness state", () => {
  it("stamps lastUpdatedAt and clears new-data hints on a load", async () => {
    vi.useFakeTimers({ toFake: ["Date"] });
    try {
      api.getActivityReport.mockResolvedValue(makeReport());
      vi.setSystemTime(new Date("2026-06-16T12:00:00Z"));

      expect(activity.lastUpdatedAt).toBeNull();
      await activity.load();
      expect(activity.lastUpdatedAt).toBe(
        new Date("2026-06-16T12:00:00Z").getTime(),
      );
      expect(activity.hasNewData).toBe(false);

      activity.markNewData();
      expect(activity.hasNewData).toBe(true);

      vi.setSystemTime(new Date("2026-06-16T12:05:00Z"));
      await activity.load();
      expect(activity.lastUpdatedAt).toBe(
        new Date("2026-06-16T12:05:00Z").getTime(),
      );
      expect(activity.hasNewData).toBe(false);
    } finally {
      vi.useRealTimers();
    }
  });

  it("markNewData is a no-op before the first report loads", () => {
    expect(activity.lastUpdatedAt).toBeNull();
    activity.markNewData();
    expect(activity.hasNewData).toBe(false);
  });

  it("does not stamp lastUpdatedAt when a load fails", async () => {
    vi.useFakeTimers({ toFake: ["Date"] });
    try {
      vi.setSystemTime(new Date("2026-06-16T12:00:00Z"));
      api.getActivityReport.mockRejectedValueOnce(new Error("network down"));
      await activity.load();
      expect(activity.lastUpdatedAt).toBeNull();
      activity.markNewData();
      expect(activity.hasNewData).toBe(false);
    } finally {
      vi.useRealTimers();
    }
  });
});

describe("url state", () => {
  it("hydrates preset/date/filters from params", () => {
    activity.hydrateFromUrl({
      preset: "week", date: "2026-06-16", project: "p1", agent: "claude",
    });
    expect(activity.preset).toBe("week");
    expect(activity.date).toBe("2026-06-16");
    expect(activity.project).toBe("p1");
    expect(activity.agent).toBe("claude");
  });

  it("hydrates rolling URL state from window_days before fixed dates", () => {
    vi.useFakeTimers({ toFake: ["Date"] });
    try {
      vi.setSystemTime(new Date("2026-06-19T12:00:00"));
      activity.hydrateFromUrl({
        preset: "custom",
        from: "2026-01-01",
        to: "2026-01-31",
        window_days: "30",
      });

      expect(activity.preset).toBe("custom");
      expect(activity.from).toBe("2026-05-20");
      expect(activity.to).toBe("2026-06-19");
      expect(activity.rollingWindowDays).toBe(30);
    } finally {
      vi.useRealTimers();
    }
  });

  it("defaults preset to day and date to today when absent", () => {
    activity.hydrateFromUrl({});
    expect(activity.preset).toBe("day");
    expect(activity.date).toMatch(/^\d{4}-\d{2}-\d{2}$/);
  });

  it("writeUrl replaces params, omitting empty filters and the day default", () => {
    const spy = vi.fn();
    // Setters also write back through replaceParams; install the spy after them
    // so the count below measures the explicit writeUrl() call alone.
    activity.setPreset("month");
    activity.setDate("2026-06-01");
    activity.setProject("");
    // Replace router.replaceParams with a spy for this test.
    (routerMod.router as unknown as { replaceParams: typeof spy }).replaceParams =
      spy;
    activity.writeUrl();
    expect(spy).toHaveBeenCalledTimes(1);
    const written = spy.mock.calls[0]![0] as Record<string, string>;
    expect(written.preset).toBe("month");
    expect(written.date).toBe("2026-06-01");
    expect("project" in written).toBe(false);
  });

  it("omits date from the URL when the anchor date is empty", () => {
    // Mirror the setter test: read the beforeEach default replaceParams mock
    // rather than a second spy, so this pins the real writeUrl() output.
    const spy = routerMod.router.replaceParams as ReturnType<typeof vi.fn>;
    activity.setPreset("day");
    activity.setDate("");
    spy.mockClear();
    activity.writeUrl();
    const params = spy.mock.calls.at(-1)![0] as Record<string, string>;
    expect(params.preset).toBe("day");
    expect("date" in params).toBe(false);
  });

  it("setters write back to the url via replaceParams", () => {
    // Observe the beforeEach default mock, not a second spy, so this pins the
    // setter -> writeUrl -> router.replaceParams contract Task 4 depends on.
    const spy = routerMod.router.replaceParams as ReturnType<typeof vi.fn>;
    spy.mockClear();
    activity.setPreset("week");
    activity.setDate("2026-06-16");
    expect(spy).toHaveBeenCalledTimes(2);
    const last = spy.mock.calls.at(-1)![0] as Record<string, string>;
    expect(last.preset).toBe("week");
    expect(last.date).toBe("2026-06-16");
    expect("project" in last).toBe(false);
  });

  it("writes and clears rolling window URL intent for custom ranges", () => {
    const spy = routerMod.router.replaceParams as ReturnType<typeof vi.fn>;
    activity.setCustomRange("2026-05-20", "2026-06-19", 30);
    spy.mockClear();

    activity.writeUrl();
    const rolling = spy.mock.calls.at(-1)![0] as Record<string, string>;
    expect(rolling).toEqual({
      preset: "custom",
      from: "2026-05-20",
      to: "2026-06-19",
      window_days: "30",
    });

    spy.mockClear();
    activity.setFrom("2026-05-21");
    const fixed = spy.mock.calls.at(-1)![0] as Record<string, string>;
    expect(fixed.from).toBe("2026-05-21");
    expect("window_days" in fixed).toBe(false);
  });

  it("hydrates the automation class from params", () => {
    activity.hydrateFromUrl({ automation: "interactive" });
    expect(activity.automation).toBe("interactive");
  });

  it("defaults automation to all for an absent or unknown value", () => {
    activity.hydrateFromUrl({});
    expect(activity.automation).toBe("all");
    activity.hydrateFromUrl({ automation: "bogus" });
    expect(activity.automation).toBe("all");
  });

  it("writeUrl omits automation when all and includes it otherwise", () => {
    const spy = routerMod.router.replaceParams as ReturnType<typeof vi.fn>;
    activity.setAutomation("all");
    spy.mockClear();
    activity.writeUrl();
    const cleared = spy.mock.calls.at(-1)![0] as Record<string, string>;
    expect("automation" in cleared).toBe(false);

    activity.setAutomation("automated");
    spy.mockClear();
    activity.writeUrl();
    const set = spy.mock.calls.at(-1)![0] as Record<string, string>;
    expect(set.automation).toBe("automated");
  });
});

describe("step", () => {
  it("advances a day preset by one day", () => {
    activity.setPreset("day");
    activity.setDate("2026-06-16");
    activity.step(1);
    expect(activity.date).toBe("2026-06-17");
    activity.step(-1);
    expect(activity.date).toBe("2026-06-16");
  });
  it("advances a week preset by seven days", () => {
    activity.setPreset("week");
    activity.setDate("2026-06-16");
    activity.step(1);
    expect(activity.date).toBe("2026-06-23");
  });
  it("advances a month preset by one calendar month, clamping the day", () => {
    activity.setPreset("month");
    activity.setDate("2026-01-31");
    activity.step(1);
    // Jan 31 + 1 month clamps to Feb 28 instead of overflowing into March.
    expect(activity.date).toBe("2026-02-28");
  });
  it("clamps the day stepping a month preset backward", () => {
    activity.setPreset("month");
    activity.setDate("2026-03-31");
    activity.step(-1);
    // Mar 31 - 1 month clamps to Feb 28 instead of overflowing into March.
    expect(activity.date).toBe("2026-02-28");
  });
});

describe("custom instant translation", () => {
  it("seeds from/to from the anchor date when selecting custom with empty bounds", () => {
    activity.setPreset("day");
    activity.setDate("2026-06-16");
    activity.setFrom("");
    activity.setTo("");
    activity.setPreset("custom");
    expect(activity.from).toBe("2026-06-16");
    expect(activity.to).toBe("2026-06-16");
  });

  it("sends half-open local instants for a custom range", async () => {
    api.getActivityReport.mockResolvedValue(makeReport());
    activity.setPreset("custom");
    activity.setFrom("2026-06-10");
    activity.setTo("2026-06-12");
    await activity.load();
    const arg = api.getActivityReport.mock.calls.at(-1)![0];
    expect(arg.preset).toBe("custom");
    // from = local 00:00 of 2026-06-10; to = local 00:00 of 2026-06-13.
    expect(arg.from).toBe(new Date("2026-06-10T00:00:00").toISOString());
    expect(arg.to).toBe(new Date("2026-06-13T00:00:00").toISOString());
  });

  it("holds the request for a custom range missing a bound", async () => {
    api.getActivityReport.mockResolvedValue(makeReport());
    activity.setPreset("custom");
    activity.setFrom("2026-06-10");
    activity.setTo(""); // a cleared date input leaves the range incomplete
    await activity.load();
    expect(api.getActivityReport).not.toHaveBeenCalled();
    expect(activity.loading).toBe(false);
  });
});
