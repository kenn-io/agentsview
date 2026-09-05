// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vite-plus/test";
import { mount, tick, unmount } from "svelte";
import { router } from "../../stores/router.svelte.js";
import { ui } from "../../stores/ui.svelte.js";

const mocks = vi.hoisted(() => ({
  cancelInFlightReads: vi.fn(),
  deleteItem: vi.fn(),
  downloadInsightExport: vi.fn().mockResolvedValue(undefined),
  generate: vi.fn(),
  load: vi.fn().mockResolvedValue(undefined),
  loadAgents: vi.fn(),
  loadProjects: vi.fn(),
  select: vi.fn(),
  selectTask: vi.fn(),
}));

const state = vi.hoisted(() => {
  const selectedItem = {
    id: 42,
    type: "llm_canned",
    kind: "prompt_maturity_review",
    date_from: "2026-07-01",
    date_to: "2026-07-31",
    project: "agentsview",
    agent: "claude",
    model: "local-model",
    content: "# Generated report",
    created_at: "2026-08-01T12:00:00Z",
  };
  return {
    sessions: {
      agents: [] as Array<{ name: string; session_count: number }>,
      projects: [],
      loadAgents: mocks.loadAgents,
      loadProjects: mocks.loadProjects,
    },
    store: {
      dateFrom: "2026-07-01",
      dateTo: "2026-07-31",
      project: "agentsview",
      sessionAgent: "codex",
      automatedScope: "human",
      cannedKind: "prompt_maturity_review",
      agent: "claude",
      promptText: "Focus on verification",
      items: [selectedItem],
      tasks: [],
      loading: false,
      selectedId: 42,
      selectedTaskId: null,
      selectedItem,
      selectedTask: undefined,
      cancelInFlightReads: mocks.cancelInFlightReads,
      deleteItem: mocks.deleteItem,
      dismissTask: vi.fn(),
      generate: mocks.generate,
      load: mocks.load,
      retryTask: vi.fn(),
      select: mocks.select,
      selectTask: mocks.selectTask,
      setAgent: vi.fn(),
      setAutomatedScope: vi.fn(),
      setCannedKind: vi.fn(),
      setDateFrom: vi.fn(),
      setDateTo: vi.fn(),
      setProject: vi.fn(),
      setSessionAgent: vi.fn(),
      setType: vi.fn(),
    },
  };
});

vi.mock("../../stores/insights.svelte.js", () => ({
  insights: state.store,
}));

vi.mock("../../stores/sessions.svelte.js", () => ({
  sessions: state.sessions,
}));

vi.mock("../../stores/sync.svelte.js", () => ({
  sync: {
    serverVersion: {
      read_only: false,
      insight_generation_available: true,
    },
    stats: { earliest_session: "2026-01-01T00:00:00Z" },
  },
}));

vi.mock("../../api/client.js", () => ({
  downloadInsightExport: mocks.downloadInsightExport,
}));

vi.mock("../../utils/markdown.js", () => ({
  renderMarkdown: (content: string) => content,
}));

// @ts-ignore
import GeneratedInsightsPanel from "./GeneratedInsightsPanel.svelte";

describe("GeneratedInsightsPanel", () => {
  let component: ReturnType<typeof mount> | undefined;

  beforeEach(() => {
    vi.clearAllMocks();
    router.route = "recall";
    router.params = { tab: "generated" };
    state.store.selectedId = 42;
    state.store.selectedItem = state.store.items[0]!;
    state.sessions.agents = [];
    ui.activeModal = null;
  });

  afterEach(async () => {
    if (component) await unmount(component);
    component = undefined;
    document.body.innerHTML = "";
    router.params = {};
  });

  it("shows every input that scopes a generated report", async () => {
    component = mount(GeneratedInsightsPanel, { target: document.body });
    await tick();

    const text = document.body.textContent ?? "";
    for (const label of [
      "Date range",
      "Project",
      "Session agent",
      "Session scope",
      "Template",
      "Generator",
      "Optional focus",
    ]) {
      expect(text).toContain(label);
    }
  });

  it("does not reorder the shared session agent list", async () => {
    state.sessions.agents = [
      { name: "codex", session_count: 5 },
      { name: "claude", session_count: 8 },
    ];

    component = mount(GeneratedInsightsPanel, { target: document.body });
    await tick();

    expect(state.sessions.agents.map((agent) => agent.name)).toEqual(["codex", "claude"]);
  });

  it("generates from the scope retained in the insights store", async () => {
    component = mount(GeneratedInsightsPanel, { target: document.body });
    await tick();

    const generate = Array.from(document.querySelectorAll<HTMLButtonElement>("button")).find(
      (button) => button.textContent?.trim() === "Generate",
    );
    expect(generate).toBeDefined();
    generate!.click();

    expect(state.store.setType).toHaveBeenCalledWith("llm_canned");
    expect(mocks.generate).toHaveBeenCalledOnce();
  });

  it("writes selected reports to the Generated insights deep link", async () => {
    component = mount(GeneratedInsightsPanel, { target: document.body });
    await tick();

    const report = Array.from(
      document.querySelectorAll<HTMLButtonElement>(".generated-list button"),
    ).find((button) => button.textContent?.includes("agentsview"));
    expect(report).toBeDefined();
    report!.click();
    await tick();

    expect(mocks.select).toHaveBeenCalledWith(42);
    expect(router.params).toEqual({ tab: "generated", insight: "42" });
  });

  it("keeps export, publish, and delete actions available for saved reports", async () => {
    const setPublishTarget = vi.spyOn(ui, "setPublishTarget");
    component = mount(GeneratedInsightsPanel, { target: document.body });
    await tick();

    const buttons = Array.from(document.querySelectorAll<HTMLButtonElement>("button"));
    buttons.find((button) => button.textContent?.trim() === "Export")!.click();
    buttons.find((button) => button.textContent?.trim() === "Publish")!.click();
    buttons.find((button) => button.ariaLabel === "Delete generated insight")!.click();
    await tick();

    expect(mocks.downloadInsightExport).toHaveBeenCalledWith(42);
    expect(setPublishTarget).toHaveBeenCalledWith({ kind: "insight", id: 42 });
    expect(mocks.deleteItem).toHaveBeenCalledWith(42);
    expect(router.params).toEqual({ tab: "generated" });
  });

  it("cancels archive reads without canceling generation tasks", async () => {
    component = mount(GeneratedInsightsPanel, { target: document.body });
    await tick();
    await unmount(component);
    component = undefined;

    expect(mocks.cancelInFlightReads).toHaveBeenCalledOnce();
  });
});
