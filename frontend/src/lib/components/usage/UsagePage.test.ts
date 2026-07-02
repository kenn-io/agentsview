import {
  afterEach,
  describe,
  expect,
  it,
  vi,
} from "vite-plus/test";
import { mount, tick, unmount } from "svelte";
import { router } from "../../stores/router.svelte.js";
import { sessions } from "../../stores/sessions.svelte.js";
import { usage } from "../../stores/usage.svelte.js";
import source from "./UsagePage.svelte?raw";
import UsagePage from "./UsagePage.svelte";

async function flushEffects() {
  await tick();
  await Promise.resolve();
  await tick();
}

let component: ReturnType<typeof mount> | undefined;

function usageSummaryWithUnsupported(kind?: string) {
  return {
    from: "2024-06-01",
    to: "2024-06-01",
    totals: {
      inputTokens: 0,
      outputTokens: 0,
      cacheCreationTokens: 0,
      cacheReadTokens: 0,
      totalCost: 0,
    },
    daily: [],
    projectTotals: [],
    modelTotals: [],
    agentTotals: [],
    sessionCounts: {
      total: 0,
      byProject: {},
      byAgent: {},
    },
    cacheStats: {
      cacheReadTokens: 0,
      cacheCreationTokens: 0,
      uncachedInputTokens: 0,
      outputTokens: 0,
      hitRate: 0,
      savingsVsUncached: 0,
    },
    ...(kind ? { unsupportedUsage: { kind } } : {}),
  };
}

afterEach(() => {
  if (component) {
    unmount(component);
    component = undefined;
  }
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
  document.body.innerHTML = "";
  router.route = "sessions";
  router.params = {};
  router.sessionId = null;
  usage.summary = null;
  usage.topSessions = null;
  usage.errors.summary = null;
  sessions.projects = [];
});

describe("UsagePage refresh behavior", () => {
  it("renders the unsupported Copilot note from the summary contract", async () => {
    vi.stubGlobal(
      "ResizeObserver",
      class {
        observe() {}
        disconnect() {}
      },
    );
    vi.spyOn(usage, "fetchAll").mockResolvedValue();

    router.route = "usage";
    router.params = {};
    usage.summary = usageSummaryWithUnsupported("copilot-no-token-data");

    component = mount(UsagePage, { target: document.body });
    await flushEffects();

    expect(document.body.textContent).toContain(
      "Copilot sessions matched this range",
    );
  });

  it("keeps the note hidden without an unsupported usage signal", async () => {
    vi.stubGlobal(
      "ResizeObserver",
      class {
        observe() {}
        disconnect() {}
      },
    );
    vi.spyOn(usage, "fetchAll").mockResolvedValue();

    router.route = "usage";
    router.params = {};
    usage.summary = usageSummaryWithUnsupported();

    component = mount(UsagePage, { target: document.body });
    await flushEffects();

    expect(document.body.textContent).not.toContain(
      "Copilot sessions matched this range",
    );
  });

  it("renders a generic unsupported usage note for unknown kinds", async () => {
    vi.stubGlobal(
      "ResizeObserver",
      class {
        observe() {}
        disconnect() {}
      },
    );
    vi.spyOn(usage, "fetchAll").mockResolvedValue();

    router.route = "usage";
    router.params = {};
    usage.summary = usageSummaryWithUnsupported("future-no-token-data");

    component = mount(UsagePage, { target: document.body });
    await flushEffects();

    expect(document.body.textContent).toContain(
      "Matching sessions do not expose token usage data",
    );
    expect(document.body.textContent).not.toContain(
      "Copilot sessions matched this range",
    );
  });

  it("does not auto-refresh usage scans from SSE updates", () => {
    expect(source).not.toContain("subscribeDebounced");
    expect(source).not.toContain("REFRESH_MS");
    // SSE only flags new data; the periodic refetch lives in RefreshControl.
    expect(source).toContain("usage.markNewData");
    expect(source).toContain("events.subscribe");
  });

  it("delegates the refresh affordance and scheduler to RefreshControl", () => {
    expect(source).toContain("<RefreshControl");
    expect(source).toContain("usage.lastUpdatedAt");
    expect(source).toContain("label={m.usage_refresh()}");
    expect(source).toContain("title={m.shared_refresh()}");
    // The scheduler, label tick, and icon now live in the shared component.
    expect(source).not.toContain("REFRESH_LABEL_INTERVAL_MS");
    expect(source).not.toContain("formatRefreshAge");
    expect(source).not.toContain("RefreshCwIcon");
    expect(source).not.toContain("setInterval");
  });

  it("shows relative last-updated status without ambiguous badges", () => {
    expect(source).not.toContain("formatUpdatedAt");
    expect(source).not.toContain("usage.hasNewData");
    expect(source).not.toContain("New data");
    expect(source).not.toContain(".new-data");
  });

  it("treats termination as a usage URL session filter", () => {
    expect(source).toContain('"termination",');
    expect(source).toContain("filtersToParams(sessions.filters)");
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

  it("updates shared yoke state from usage range selections", () => {
    expect(source).toContain("<RangePicker");
    expect(source).toContain("function applyRange");
    expect(source).toContain("updateYokeFromUsage");
    expect(source).toContain("yokedDates.updateFromPanel");
  });

  it("seeds bare usage URLs from shared yoked dates", () => {
    expect(source).toContain("const seed = yokedDates.seedForPanel()");
    expect(source).toContain("applyUsagePanelDate(state)");
    expect(source).toContain("usage.applyRollingWindow");
    expect(source).toContain("usage.applyDateRange");
  });

  it("hydrates supported termination filters from usage URLs", () => {
    const filterKeysIndex = source.indexOf("const SESSION_FILTER_KEYS");
    const urlInitIndex = source.indexOf("let urlInitRan", filterKeysIndex);
    const filterKeysBlock = source.slice(filterKeysIndex, urlInitIndex);

    expect(filterKeysBlock).toContain('"termination"');
  });

  it("does not hydrate session-only date filters from usage URLs", () => {
    const filterKeysIndex = source.indexOf("const SESSION_FILTER_KEYS");
    const urlInitIndex = source.indexOf("let urlInitRan", filterKeysIndex);
    const filterKeysBlock = source.slice(filterKeysIndex, urlInitIndex);

    expect(filterKeysBlock).not.toContain('"date"');
    expect(filterKeysBlock).not.toContain('"date_from"');
    expect(filterKeysBlock).not.toContain('"date_to"');
  });

  it("sanitizes mixed usage URL session params before hydrating", () => {
    const initStart = source.indexOf("if (hasSessionFilterKeys)");
    const initEnd = source.indexOf("if (hasDateParam)", initStart);
    const initBlock = source.slice(initStart, initEnd);

    expect(source).toContain("function usageSupportedSessionParams");
    expect(initBlock).toContain(
      "parseFiltersFromParams(supportedSessionParams)",
    );
    expect(initBlock).toContain(
      "sessions.initFromParams(supportedSessionParams)",
    );
    expect(initBlock).not.toContain("parseFiltersFromParams(params)");
    expect(initBlock).not.toContain("sessions.initFromParams(params)");
  });

  it("mounts the pairwise comparison panel additively", () => {
    expect(source).toContain("UsagePairwiseComparisonPanel");
    expect(source).toContain("<UsagePairwiseComparisonPanel />");
  });

  it("keeps pairwise comparison below bounded secondary usage panels", () => {
    const topSessionsIndex = source.indexOf("<TopSessionsTable />");
    const cacheEfficiencyIndex = source.indexOf("<CacheEfficiencyPanel />");
    const pairwiseIndex = source.indexOf("<UsagePairwiseComparisonPanel />");

    expect(topSessionsIndex).toBeGreaterThan(-1);
    expect(cacheEfficiencyIndex).toBeGreaterThan(-1);
    expect(pairwiseIndex).toBeGreaterThan(cacheEfficiencyIndex);
    expect(pairwiseIndex).toBeGreaterThan(topSessionsIndex);
    expect(source).toContain('class="chart-panel bounded"');
    expect(source).toContain("max-height:");
    expect(source).toContain("overflow: auto;");
  });
});
