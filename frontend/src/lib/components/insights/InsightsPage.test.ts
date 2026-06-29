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
import { ui } from "../../stores/ui.svelte.js";
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

  it("refreshes rolling insight URL/yoke bounds after signal fetches", () => {
    const fetchIndex = source.indexOf("function fetchInsightSignals");
    const nextHandlerIndex = source.indexOf(
      "\n\n  function handleProjectChange",
      fetchIndex,
    );
    const fetchBlock = source.slice(fetchIndex, nextHandlerIndex);

    expect(fetchBlock).toContain("analytics.fetchSignalsForInsights()");
    expect(fetchBlock).toContain("updateYokeFromInsights(state)");
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
});
