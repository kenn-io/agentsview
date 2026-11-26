import { reportErrorShown } from "../../utils/telemetry.js";
vi.mock("../../utils/telemetry.js", () => ({ reportErrorShown: vi.fn() }));
// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vite-plus/test";
import { mount, tick, unmount } from "svelte";
import { analytics } from "../../stores/analytics.svelte.js";
import { insights } from "../../stores/insights.svelte.js";
import { router } from "../../stores/router.svelte.js";
import { sessions } from "../../stores/sessions.svelte.js";
import { events } from "../../stores/events.svelte.js";
import { AnalyticsService, MetadataService } from "../../api/generated/index.js";
import type { DbSignalsAnalyticsResponse as SignalsAnalyticsResponse } from "../../api/generated/index.js";
import { sync } from "../../stores/sync.svelte.js";

// @ts-ignore
import QualityPage from "./QualityPage.svelte";

describe("QualityPage", () => {
  let component: ReturnType<typeof mount> | undefined;
  let insightLoadCalls = 0;

  beforeEach(() => {
    vi.mocked(reportErrorShown).mockClear();
    analytics.signals = null;
    analytics.errors.signals = null;
    router.route = "quality";
    router.params = {};
    vi.spyOn(analytics, "fetchSignalsForQuality").mockResolvedValue();
    insightLoadCalls = 0;
    vi.spyOn(insights, "load").mockImplementation(async () => {
      insightLoadCalls += 1;
    });
  });

  afterEach(async () => {
    if (component) await unmount(component);
    component = undefined;
    document.body.innerHTML = "";
    analytics.signals = null;
    analytics.loading.signals = false;
    analytics.querying.signals = false;
    analytics.errors.signals = null;
    router.route = "sessions";
    router.params = {};
    sync.serverVersion = null;
    vi.restoreAllMocks();
  });

  it("adds newly imported agents to the filter without remounting", async () => {
    let onEvent: ((event: { scope: "sessions" }) => void) | undefined;
    vi.spyOn(events, "subscribeDebounced").mockImplementation((listener) => {
      onEvent = listener;
      return () => {};
    });
    let imported = false;
    vi.spyOn(sessions, "loadProjects").mockResolvedValue();
    vi.spyOn(sessions, "loadMachines").mockResolvedValue();
    vi.spyOn(sessions, "loadAgents").mockImplementation(async () => {
      sessions.agents = imported ? [{ name: "codex", session_count: 1 }] : [];
    });
    component = mount(QualityPage, { target: document.body });
    await tick();
    imported = true;
    onEvent!({ scope: "sessions" });
    await tick();
    await tick();
    const trigger = document.querySelector<HTMLButtonElement>('[title="Filter insights by agent"]');
    expect(trigger).not.toBeNull();
    trigger!.click();
    await tick();
    const option = [...document.querySelectorAll<HTMLElement>('[role="option"]')].find((item) =>
      item.textContent?.includes("Codex (1)"),
    );
    expect(option).toBeDefined();
    option!.dispatchEvent(new MouseEvent("mousedown", { bubbles: true }));
    await tick();
    expect(analytics.agent).toBe("codex");
    analytics.agent = "";
  });

  it("loads quality once with the already-selected machine from a label deep link", async () => {
    const requests: string[] = [];
    vi.mocked(analytics.fetchSignalsForQuality).mockImplementation(async () => {
      requests.push(analytics.machine);
    });
    vi.spyOn(sessions, "loadProjects").mockResolvedValue();
    vi.spyOn(sessions, "loadAgents").mockResolvedValue();
    const metadata =
      Promise.withResolvers<Awaited<ReturnType<typeof MetadataService.getApiV1Machines>>>();
    vi.spyOn(MetadataService, "getApiV1Machines").mockReturnValue(metadata.promise);
    router.params = { machine: "Workstation", date_from: "2026-03-01", date_to: "2026-03-31" };
    const savedFilters = sessions.filters;
    const savedMachine = analytics.machine;
    analytics.machine = "id-a";
    sessions.initFromParams(router.params);

    try {
      component = mount(QualityPage, { target: document.body });
      await tick();
      expect(requests).toEqual([]);

      metadata.resolve({
        machines: ["id-a"],
        resolved_machines: ["id-a"],
        machine_labels: { "id-a": "Workstation" },
        machine_aliases: {},
      });
      await sessions.loadMachines();
      await tick();

      expect(requests).toEqual(["id-a"]);
    } finally {
      if (component) await unmount(component);
      component = undefined;
      sessions.filters = savedFilters;
      analytics.machine = savedMachine;
    }
  });

  it("links to the Friction Log from recommendations when available", async () => {
    sync.serverVersion = {
      api_version: 1,
      data_version: 1,
      insight_generation_available: false,
      friction_available: true,
      friction_build_allowed: true,
      friction_readable: true,
      session_stats_available: false,
      kata_available: false,
      version: "dev",
      commit: "unknown",
      build_date: "",
      read_only: false,
    };
    component = mount(QualityPage, { target: document.body });
    await tick();

    const link = document.querySelector<HTMLAnchorElement>("a.friction-link");
    expect(link).not.toBeNull();
    expect(link!.getAttribute("href")).toBe("/friction");
    expect(document.body.textContent).toContain("Daily digests of corrections");

    link!.dispatchEvent(new MouseEvent("click", { bubbles: true, cancelable: true, button: 0 }));
    expect(router.route).toBe("friction");
  });

  it("omits the Friction Log card when the server does not build digests", async () => {
    sync.serverVersion = null;
    component = mount(QualityPage, { target: document.body });
    await tick();
    expect(document.querySelector("a.friction-link")).toBeNull();
  });

  it("renders deterministic quality analysis without loading generated reports", async () => {
    component = mount(QualityPage, { target: document.body });
    await tick();

    expect(document.body.textContent).toContain("Deterministic recommendations");
    expect(document.body.textContent).toContain("Quality patterns");
    expect(insightLoadCalls).toBe(0);
  });

  it("reports the analytics kind for a visible signals failure", async () => {
    analytics.errors.signals = "Signals failed";
    component = mount(QualityPage, { target: document.body });
    await tick();
    expect(reportErrorShown).toHaveBeenCalledExactlyOnceWith("analytics");
  });

  it("shows refresh activity while a filtered quality query is running", async () => {
    component = mount(QualityPage, { target: document.body });
    await tick();

    analytics.querying.signals = true;
    await tick();

    const content = document.querySelector(".content");
    const refreshButton = document.querySelector<HTMLButtonElement>(
      'button[aria-label="Refresh quality"]',
    );

    expect(content?.getAttribute("aria-busy")).toBe("true");
    expect(content?.querySelector(".query-progress")).not.toBeNull();
    expect(refreshButton?.disabled).toBe(true);
  });

  it("shows Quality freshness instead of dashboard freshness", async () => {
    const now = new Date("2026-06-15T15:00:00Z").getTime();
    vi.spyOn(Date, "now").mockReturnValue(now);
    analytics.lastUpdatedAt = now - 3 * 60_000;
    analytics.qualityLastUpdatedAt = now - 7 * 60_000;

    component = mount(QualityPage, { target: document.body });
    await tick();

    expect(document.body.textContent).toContain("Updated 7m ago");
    expect(document.body.textContent).not.toContain("Updated 3m ago");
  });

  it("keeps the error alert and Retry button mounted during a background refresh after failure", async () => {
    vi.restoreAllMocks();
    vi.spyOn(sessions, "loadProjects").mockResolvedValue();
    vi.spyOn(sessions, "loadAgents").mockResolvedValue();
    vi.spyOn(sessions, "loadMachines").mockResolvedValue();

    let onEvent: ((event: { scope: "sync" }) => void) | undefined;
    vi.spyOn(events, "subscribeDebounced").mockImplementation((listener) => {
      onEvent = listener;
      return () => {};
    });

    const first = Promise.withResolvers<SignalsAnalyticsResponse>();
    const backgroundRefresh = Promise.withResolvers<SignalsAnalyticsResponse>();
    const retryRefresh = Promise.withResolvers<SignalsAnalyticsResponse>();
    let backgroundAborted = false;
    const requests = [first, backgroundRefresh, retryRefresh];
    const getSignals = vi
      .spyOn(AnalyticsService, "getApiV1AnalyticsSignals")
      .mockImplementation((_params, options) => {
        const index = getSignals.mock.calls.length - 1;
        if (index === 1) {
          options?.signal?.addEventListener(
            "abort",
            () => {
              backgroundAborted = true;
              backgroundRefresh.reject(new DOMException("Aborted", "AbortError"));
            },
            { once: true },
          );
        }
        return requests[index]!.promise;
      });

    component = mount(QualityPage, { target: document.body });
    await tick();

    // Initial load with no error still shows the skeleton.
    expect(document.querySelector(".skeleton-card")).not.toBeNull();
    expect(document.querySelector('[role="alert"]')).toBeNull();

    first.reject(new Error("injected read failure"));
    await vi.waitFor(() => {
      expect(document.querySelector('[role="alert"]')?.textContent).toContain(
        "injected read failure",
      );
    });
    const retryButton = document.querySelector<HTMLButtonElement>('[role="alert"] button');
    expect(retryButton).not.toBeNull();
    expect(retryButton!.textContent?.trim()).toBe("Retry");

    // An SSE-driven background refresh starts while the alert is on screen.
    onEvent!({ scope: "sync" });
    await tick();

    expect(getSignals).toHaveBeenCalledTimes(2);
    expect(analytics.loading.signals).toBe(true);
    expect(document.querySelector(".skeleton-card")).toBeNull();
    expect(document.querySelector<HTMLButtonElement>('[role="alert"] button')).toBe(retryButton);
    expect(retryButton!.isConnected).toBe(true);

    // Clicking the still-mounted Retry button aborts the background refresh and recovers.
    retryButton!.click();
    await tick();

    expect(backgroundAborted).toBe(true);
    expect(getSignals).toHaveBeenCalledTimes(3);

    retryRefresh.resolve({
      scored_sessions: 1,
      unscored_sessions: 0,
      grade_distribution: { A: 1 },
      avg_health_score: 92,
      outcome_distribution: { completed: 1 },
      outcome_confidence_distribution: { high: 1 },
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
        sessions_with_context_data: 1,
        avg_context_pressure: 0.1,
        high_pressure_sessions: 0,
      },
      quality_health: {
        computed_sessions: 1,
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
    });

    await vi.waitFor(() => {
      expect(document.querySelector('[role="alert"]')).toBeNull();
    });
    expect(document.body.textContent).toContain("92");
  });
});
