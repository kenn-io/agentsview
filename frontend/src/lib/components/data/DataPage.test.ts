// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { mount, tick, unmount } from "svelte";
import type { DbProjectInventory, DbProjectInventoryRow } from "../../api/generated/index";

const api = vi.hoisted(() => ({
  getApiV1DataProjects: vi.fn(),
  getApiV1DataProjectRules: vi.fn(),
  candidates: vi.fn(),
  preview: vi.fn(),
  apply: vi.fn(),
  applyMappings: vi.fn(),
}));
const syncMock = vi.hoisted(() => ({
  serverVersion: { version: "1.0.0", read_only: false } as object | null,
  readOnly: false,
}));

vi.mock("../../api/generated/index", () => ({
  DataService: {
    getApiV1DataProjects: api.getApiV1DataProjects,
    getApiV1DataProjectRules: api.getApiV1DataProjectRules,
    getApiV1DataProjectReclassificationCandidates: api.candidates,
  },
  SettingsService: {
    postApiV1SettingsWorktreeMappingsPreview: api.preview,
    postApiV1SettingsWorktreeMappingsReclassify: api.apply,
    postApiV1SettingsWorktreeMappingsApply: api.applyMappings,
  },
}));
vi.mock("../../api/runtime.js", () => ({
  callGenerated: (request: () => Promise<unknown>) => request(),
  isAbortError: () => false,
}));
vi.mock("../../stores/router.svelte.js", () => ({
  router: { params: {}, replaceParams: vi.fn() },
}));
vi.mock("../../stores/sync.svelte.js", () => ({ sync: syncMock }));

import { fireEvent, screen } from "@testing-library/svelte";
import DataPage from "./DataPage.svelte";
import { data } from "../../stores/data.svelte.js";
import { router } from "../../stores/router.svelte.js";
import { m } from "../../i18n/index.js";

function makeRow(overrides: Partial<DbProjectInventoryRow> = {}): DbProjectInventoryRow {
  return {
    agents: 1,
    distinct_cwds: 1,
    enabled_rules_targeting: 0,
    label: "Project",
    machines: 1,
    project_key: "k1",
    recorded_as_original: false,
    sessions: 1,
    ...overrides,
  };
}

function makeInventory(rows: DbProjectInventoryRow[]): DbProjectInventory {
  return {
    governed_sessions: 0,
    projects: rows,
    total_projects: rows.length,
    total_sessions: rows.reduce((sum, r) => sum + r.sessions, 0),
  };
}

let component: ReturnType<typeof mount> | undefined;

beforeEach(() => {
  api.getApiV1DataProjects.mockReset();
  api.getApiV1DataProjectRules.mockReset();
  api.candidates.mockReset();
  api.preview.mockReset();
  api.apply.mockReset();
  api.applyMappings.mockReset();
  api.candidates.mockResolvedValue({ candidates: [] });
  syncMock.serverVersion = { version: "1.0.0", read_only: false };
  syncMock.readOnly = false;
  data.inventory = null;
  data.loading = false;
  data.error = "";
  data.view = "inventory";
  data.selectedProjectKey = "";
  data.rulesMachine = "";
  (router as unknown as { params: Record<string, string> }).params = {};
});

afterEach(() => {
  if (component) unmount(component);
  component = undefined;
  document.body.innerHTML = "";
  vi.useRealTimers();
  vi.restoreAllMocks();
});

async function flush() {
  await tick();
  await Promise.resolve();
  await tick();
}

function deferred<T>() {
  let resolve!: (value: T) => void;
  const promise = new Promise<T>((done) => {
    resolve = done;
  });
  return { promise, resolve };
}

describe("DataPage", () => {
  it("shows the loading status before the inventory arrives", async () => {
    api.getApiV1DataProjects.mockImplementationOnce(() => new Promise(() => {}));
    component = mount(DataPage, { target: document.body });
    await tick();

    expect(document.body.textContent).toContain(m.data_loading());
  });

  it("shows the unknown-project-key notice when the store flags it", async () => {
    const inventory = makeInventory([makeRow({ project_key: "k1" })]);
    (router as unknown as { params: Record<string, string> }).params = {
      project_key: "missing",
    };
    api.getApiV1DataProjects.mockResolvedValueOnce(inventory);

    component = mount(DataPage, { target: document.body });
    await tick();
    await Promise.resolve();
    await tick();

    expect(document.body.textContent).toContain(m.data_unknown_project_key());
  });

  it("shows summary strip totals via the plural messages", async () => {
    const inventory = makeInventory([
      makeRow({ project_key: "k1", sessions: 3 }),
      makeRow({ project_key: "k2", sessions: 4, enabled_rules_targeting: 1 }),
    ]);
    inventory.governed_sessions = 4;
    api.getApiV1DataProjects.mockResolvedValueOnce(inventory);

    component = mount(DataPage, { target: document.body });
    await tick();
    await Promise.resolve();
    await tick();

    expect(document.body.textContent).toContain(m.data_summary_projects({ count: 2 }));
    expect(document.body.textContent).toContain(m.data_summary_sessions({ count: 7 }));
    expect(document.body.textContent).toContain(m.data_summary_governed({ count: 4 }));
  });

  it("shows the error status and retries the load on click", async () => {
    api.getApiV1DataProjects.mockRejectedValueOnce(new Error("boom"));
    component = mount(DataPage, { target: document.body });
    await tick();
    await Promise.resolve();
    await tick();

    expect(document.body.textContent).toContain("boom");
    expect(api.getApiV1DataProjects).toHaveBeenCalledTimes(1);

    const inventory = makeInventory([makeRow({ project_key: "k1" })]);
    api.getApiV1DataProjects.mockResolvedValueOnce(inventory);
    const retryBtn = document.querySelector<HTMLButtonElement>(".retry-btn");
    expect(retryBtn).not.toBeNull();
    retryBtn?.click();
    await tick();
    await Promise.resolve();
    await tick();

    expect(api.getApiV1DataProjects).toHaveBeenCalledTimes(2);
    expect(document.body.textContent).toContain(m.data_summary_projects({ count: 1 }));
  });

  it("shows a foreground reload error above the retained inventory", async () => {
    data.inventory = makeInventory([makeRow({ project_key: "k1" })]);
    api.getApiV1DataProjects.mockRejectedValueOnce(new Error("server unavailable"));

    component = mount(DataPage, { target: document.body });
    await flush();

    expect(document.body.textContent).toContain("server unavailable");
    expect(document.body.textContent).toContain(m.data_summary_projects({ count: 1 }));
    expect(document.querySelector<HTMLButtonElement>(".retry-btn")).not.toBeNull();
  });

  it("shows the empty message and no table when there are no projects", async () => {
    const inventory = makeInventory([]);
    api.getApiV1DataProjects.mockResolvedValueOnce(inventory);

    component = mount(DataPage, { target: document.body });
    await tick();
    await Promise.resolve();
    await tick();

    expect(document.body.textContent).toContain(m.data_empty());
    expect(document.querySelector("table")).toBeNull();
  });

  it("renders the workspace beside the table when a row is selected", async () => {
    const inventory = makeInventory([makeRow({ project_key: "k1", label: "wrong-project" })]);
    (router as unknown as { params: Record<string, string> }).params = { project_key: "k1" };
    api.getApiV1DataProjects.mockResolvedValueOnce(inventory);

    component = mount(DataPage, { target: document.body });
    await flush();

    expect(document.querySelector(".pane-table table")).not.toBeNull();
    expect(document.querySelector(".pane-detail")).not.toBeNull();
    expect(screen.getByRole("heading", { name: "wrong-project" })).toBeTruthy();
  });

  const candidate = {
    id: "candidate-1",
    machine: "machine-a",
    suggested_prefix: "/srv/worktrees/repo/branch",
    contributing_sessions: 3,
    distinct_cwds: 2,
    evidence_kind: "identity",
    evidence_root: "/srv/worktrees/repo/branch",
    examples: [],
    available: true,
  };
  const previewResult = {
    mapping_token: "token-1",
    normalized_project: "",
    matched_sessions: 7,
    updated_sessions: 6,
    distinct_projects: 1,
    project_samples: [],
    session_samples: [],
  };

  async function applyReclassification() {
    vi.useFakeTimers();
    const inventory = makeInventory([
      makeRow({ project_key: "k1", label: "wrong-project", sessions: 3 }),
      makeRow({ project_key: "k2", label: "target-project", sessions: 12 }),
    ]);
    (router as unknown as { params: Record<string, string> }).params = { project_key: "k1" };
    api.getApiV1DataProjects.mockResolvedValue(inventory);
    api.candidates.mockResolvedValue({ candidates: [candidate] });
    api.preview.mockResolvedValue(previewResult);
    api.apply.mockResolvedValue({ mapping: {}, result: previewResult });
    const refreshSpy = vi.spyOn(data, "refreshAfterApply").mockResolvedValue(true);

    component = mount(DataPage, { target: document.body });
    await flush();
    await flush();

    await fireEvent.click(screen.getByTitle("Target project"));
    await fireEvent.mouseDown(screen.getByRole("option", { name: "target-project (12)" }));
    await vi.advanceTimersByTimeAsync(300);
    await flush();
    await fireEvent.click(screen.getByRole("button", { name: "Apply reclassification" }));
    await flush();
    await flush();
    return refreshSpy;
  }

  it("routes the editor's refresh through data.refreshAfterApply with the selected key", async () => {
    const refreshSpy = await applyReclassification();

    expect(refreshSpy).toHaveBeenCalledTimes(1);
    expect(refreshSpy).toHaveBeenCalledWith("k1", "target-project");
  });

  it("remounts the editor after a completed apply that keeps the selection", async () => {
    await applyReclassification();

    expect(api.candidates).toHaveBeenCalledTimes(2);
  });

  it("keeps a mid-apply dismissal dismissed when the deferred apply refresh lands", async () => {
    vi.useFakeTimers();
    const before = makeInventory([
      makeRow({ project_key: "k1", label: "wrong-project", sessions: 3 }),
      makeRow({ project_key: "k2", label: "target-project", sessions: 12 }),
    ]);
    const after = makeInventory([
      makeRow({ project_key: "k2", label: "target-project", sessions: 15 }),
    ]);
    (router as unknown as { params: Record<string, string> }).params = { project_key: "k1" };
    api.getApiV1DataProjects.mockResolvedValueOnce(before).mockResolvedValue(after);
    api.candidates.mockResolvedValue({ candidates: [candidate] });
    api.preview.mockResolvedValue(previewResult);
    const pendingApply = deferred<{ mapping: object; result: typeof previewResult }>();
    api.apply.mockReturnValueOnce(pendingApply.promise);

    component = mount(DataPage, { target: document.body });
    await flush();
    await flush();

    await fireEvent.click(screen.getByTitle("Target project"));
    await fireEvent.mouseDown(screen.getByRole("option", { name: "target-project (12)" }));
    await vi.advanceTimersByTimeAsync(300);
    await flush();
    await fireEvent.click(screen.getByRole("button", { name: "Apply reclassification" }));
    await flush();

    // Dismiss the workspace while the reclassify request is still in flight.
    await fireEvent.click(screen.getByRole("button", { name: m.data_workspace_close() }));
    await flush();
    expect(document.querySelector(".pane-detail")).toBeNull();

    pendingApply.resolve({ mapping: {}, result: previewResult });
    await flush();
    await flush();

    // The committed apply still refreshes the inventory, but the dismissal
    // wins: the workspace must not reopen on the applied target's row.
    expect(api.getApiV1DataProjects).toHaveBeenCalledTimes(2);
    expect(data.selectedProjectKey).toBe("");
    expect(document.querySelector(".pane-detail")).toBeNull();
  });

  function rulesResponse(project: string) {
    return {
      local_machine: "machine-a",
      machine: "machine-a",
      machines: ["machine-a"],
      rules: [
        {
          id: 1,
          machine: "machine-a",
          path_prefix: "/srv/worktrees/example",
          layout: "explicit",
          project,
          original_project: "",
          enabled: true,
          created_at: "2026-07-04T00:00:00.000Z",
          updated_at: "2026-07-04T00:00:00.000Z",
          source_archive_id: "",
          governed_sessions: 2,
        },
      ],
    };
  }

  it("selects the matching inventory project from a rules cross-link", async () => {
    const inventory = makeInventory([
      makeRow({ project_key: "k1", label: "other-project" }),
      makeRow({ project_key: "k2", label: "target-project" }),
    ]);
    (router as unknown as { params: Record<string, string> }).params = { view: "rules" };
    api.getApiV1DataProjects.mockResolvedValue(inventory);
    api.getApiV1DataProjectRules.mockResolvedValue(rulesResponse("target-project"));

    component = mount(DataPage, { target: document.body });
    await flush();

    await fireEvent.click(screen.getByRole("button", { name: "target-project" }));
    await flush();

    expect(data.view).toBe("inventory");
    expect(data.selectedProjectKey).toBe("k2");
    expect(document.querySelector(".pane-detail")).not.toBeNull();
  });

  it("falls back to the unselected inventory when the cross-linked label is unknown", async () => {
    const inventory = makeInventory([makeRow({ project_key: "k1", label: "other-project" })]);
    (router as unknown as { params: Record<string, string> }).params = { view: "rules" };
    api.getApiV1DataProjects.mockResolvedValue(inventory);
    api.getApiV1DataProjectRules.mockResolvedValue(rulesResponse("ghost-project"));

    component = mount(DataPage, { target: document.body });
    await flush();
    expect(api.getApiV1DataProjects).toHaveBeenCalledTimes(1);

    await fireEvent.click(screen.getByRole("button", { name: "ghost-project" }));
    await flush();

    expect(data.view).toBe("inventory");
    expect(data.selectedProjectKey).toBe("");
    expect(api.getApiV1DataProjects).toHaveBeenCalledTimes(2);
    expect(document.querySelector(".pane-detail")).toBeNull();
    expect(document.querySelector(".pane-table table")).not.toBeNull();
  });

  it("fetches rules exactly once when a machine is selected in the rules view", async () => {
    (router as unknown as { params: Record<string, string> }).params = { view: "rules" };
    api.getApiV1DataProjects.mockResolvedValue(makeInventory([]));
    api.getApiV1DataProjectRules.mockImplementation((args: { machine?: string }) =>
      Promise.resolve({
        ...rulesResponse("target-project"),
        machine: args.machine ?? "machine-a",
        machines: ["machine-a", "machine-b"],
      }),
    );

    component = mount(DataPage, { target: document.body });
    await flush();
    expect(api.getApiV1DataProjectRules).toHaveBeenCalledTimes(1);

    await fireEvent.click(screen.getByRole("button", { name: "Select machine" }));
    await fireEvent.mouseDown(screen.getByRole("option", { name: "machine-b" }));
    await flush();

    // The selection delegates to the store, whose {#key} remount performs the
    // one and only load for the new machine.
    expect(data.rulesMachine).toBe("machine-b");
    expect(api.getApiV1DataProjectRules).toHaveBeenCalledTimes(2);
    expect(api.getApiV1DataProjectRules).toHaveBeenLastCalledWith({ machine: "machine-b" });
  });

  it("remounts the rules view with the new machine when the store's rules machine changes", async () => {
    (router as unknown as { params: Record<string, string> }).params = { view: "rules" };
    api.getApiV1DataProjects.mockResolvedValue(makeInventory([]));
    api.getApiV1DataProjectRules.mockResolvedValue(rulesResponse("target-project"));

    component = mount(DataPage, { target: document.body });
    await flush();

    expect(api.getApiV1DataProjectRules).toHaveBeenCalledTimes(1);
    expect(api.getApiV1DataProjectRules).toHaveBeenCalledWith({ machine: undefined });

    data.setRulesMachine("machine-b");
    await flush();

    expect(api.getApiV1DataProjectRules).toHaveBeenCalledTimes(2);
    expect(api.getApiV1DataProjectRules).toHaveBeenLastCalledWith({ machine: "machine-b" });
  });

  it("refreshes the cached inventory in the background after a rules mutation", async () => {
    (router as unknown as { params: Record<string, string> }).params = { view: "rules" };
    api.getApiV1DataProjects.mockResolvedValue(makeInventory([]));
    api.getApiV1DataProjectRules.mockResolvedValue(rulesResponse("target-project"));
    api.applyMappings.mockResolvedValue({
      machine: "machine-a",
      updated_sessions: 1,
      matched_sessions: 1,
    });
    const loadSpy = vi.spyOn(data, "load").mockResolvedValue(true);

    component = mount(DataPage, { target: document.body });
    await flush();
    loadSpy.mockClear();

    await fireEvent.click(screen.getByRole("button", { name: "Apply mappings" }));
    await flush();

    expect(loadSpy).toHaveBeenCalledWith({ background: true });
  });

  it("clears the URL selection and returns focus to the row on close", async () => {
    const inventory = makeInventory([makeRow({ project_key: "k1", label: "wrong-project" })]);
    (router as unknown as { params: Record<string, string> }).params = { project_key: "k1" };
    api.getApiV1DataProjects.mockResolvedValueOnce(inventory);

    component = mount(DataPage, { target: document.body });
    await flush();

    await fireEvent.click(screen.getByRole("button", { name: m.data_workspace_close() }));
    await flush();

    expect(document.querySelector(".pane-detail")).toBeNull();
    const replace = router.replaceParams as unknown as ReturnType<typeof vi.fn>;
    expect(replace.mock.lastCall?.[0]).toEqual({});

    await new Promise((resolve) => requestAnimationFrame(() => resolve(undefined)));
    const rowEl = document.querySelector<HTMLElement>('[data-project-key="k1"]');
    expect(rowEl).not.toBeNull();
    expect(document.activeElement).toBe(rowEl);
  });
});
