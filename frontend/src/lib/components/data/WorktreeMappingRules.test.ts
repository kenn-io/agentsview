// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vite-plus/test";
import { fireEvent, screen } from "@testing-library/svelte";
import { mount, tick, unmount } from "svelte";
// @ts-ignore
import WorktreeMappingRules from "./WorktreeMappingRules.svelte";
import { DataService, SettingsService } from "../../api/generated/index";

vi.mock("../../api/runtime.js", async (importOriginal) => {
  const orig =
    await importOriginal<typeof import("../../api/runtime.js")>();
  return {
    ...orig,
    callGenerated: vi.fn((request: () => Promise<unknown>) => request()),
  };
});

vi.mock("../../api/generated/index", async (importOriginal) => {
  const orig =
    await importOriginal<typeof import("../../api/generated/index")>();
  return {
    ...orig,
    DataService: {
      getApiV1DataProjectRules: vi.fn(),
    },
    SettingsService: {
      postApiV1SettingsWorktreeMappings: vi.fn(),
      putApiV1SettingsWorktreeMappingsId: vi.fn(),
      deleteApiV1SettingsWorktreeMappingsId: vi.fn(),
      postApiV1SettingsWorktreeMappingsApply: vi.fn(),
    },
  };
});

const dataService = DataService as unknown as {
  getApiV1DataProjectRules: ReturnType<typeof vi.fn>;
};

const settingsService = SettingsService as unknown as {
  postApiV1SettingsWorktreeMappings: ReturnType<typeof vi.fn>;
  putApiV1SettingsWorktreeMappingsId: ReturnType<typeof vi.fn>;
  deleteApiV1SettingsWorktreeMappingsId: ReturnType<typeof vi.fn>;
  postApiV1SettingsWorktreeMappingsApply: ReturnType<typeof vi.fn>;
};

function response(machine: string, rules: unknown[] = []) {
  return {
    local_machine: "local-host",
    machine,
    machines: ["local-host", "remote-host"],
    rules,
  };
}

function rule(overrides: Record<string, unknown> = {}) {
  return {
    id: 1,
    machine: "remote-host",
    path_prefix: "/srv/worktrees/example",
    layout: "explicit",
    project: "canonical-project",
    original_project: "branch-label",
    enabled: true,
    created_at: "2026-07-04T00:00:00.000Z",
    updated_at: "2026-07-04T00:00:00.000Z",
    source_archive_id: "",
    governed_sessions: 5,
    ...overrides,
  };
}

function deferred<T>() {
  let resolve!: (value: T) => void;
  let reject!: (reason: unknown) => void;
  const promise = new Promise<T>((resolvePromise, rejectPromise) => {
    resolve = resolvePromise;
    reject = rejectPromise;
  });
  return { promise, resolve, reject };
}

async function flush() {
  await tick();
  await new Promise((resolve) => setTimeout(resolve, 0));
  await tick();
}

describe("WorktreeMappingRules", () => {
  let component: Record<string, unknown> | null = null;
  let onMachineChange: ReturnType<typeof vi.fn<(machine: string) => void>>;
  let onSelectProject: ReturnType<typeof vi.fn<(label: string) => void>>;

  function mountRules(props: Record<string, unknown> = {}) {
    return mount(WorktreeMappingRules, {
      target: document.body,
      props: {
        onMachineChange,
        onSelectProject,
        ...props,
      },
    });
  }

  beforeEach(() => {
    vi.clearAllMocks();
    dataService.getApiV1DataProjectRules.mockReset();
    settingsService.postApiV1SettingsWorktreeMappings.mockReset();
    settingsService.putApiV1SettingsWorktreeMappingsId.mockReset();
    settingsService.deleteApiV1SettingsWorktreeMappingsId.mockReset();
    settingsService.postApiV1SettingsWorktreeMappingsApply.mockReset();
    onMachineChange = vi.fn<(machine: string) => void>();
    onSelectProject = vi.fn<(label: string) => void>();
  });

  afterEach(() => {
    if (component) unmount(component);
    component = null;
    document.body.innerHTML = "";
  });

  it("renders the rules as a table with the seven columns", async () => {
    dataService.getApiV1DataProjectRules.mockResolvedValue(
      response("remote-host", [rule()]),
    );

    component = mountRules();
    await flush();

    expect(document.querySelector("table")).not.toBeNull();
    const headers = screen.getAllByRole("columnheader").map((th) => th.textContent?.trim());
    expect(headers).toEqual([
      "Path prefix",
      "Target project",
      "Original label",
      "Enabled",
      "Updated",
      "Governed sessions",
      "Actions",
    ]);

    const row = document.querySelector("tbody tr");
    expect(row).not.toBeNull();
    expect(row?.textContent).toContain("/srv/worktrees/example");
    expect(row?.textContent).toContain("canonical-project");
    expect(row?.textContent).toContain("branch-label");
    expect(row?.textContent).toContain("On");
    const expectedDate = new Intl.DateTimeFormat("en", { dateStyle: "medium" }).format(
      new Date("2026-07-04T00:00:00.000Z"),
    );
    expect(row?.textContent).toContain(expectedDate);
    const governedCell = row?.querySelector("td:nth-child(6)");
    expect(governedCell?.textContent?.trim()).toBe("5");
  });

  it("still loads and renders the table read-only with mutations unavailable", async () => {
    dataService.getApiV1DataProjectRules.mockResolvedValue(
      response("remote-host", [rule()]),
    );

    component = mountRules({ readOnly: true });
    await flush();

    expect(dataService.getApiV1DataProjectRules).toHaveBeenCalledWith({
      machine: undefined,
    });
    expect(document.querySelector("table")).not.toBeNull();
    expect(document.body.textContent).toContain("canonical-project");
    expect(document.body.textContent).toContain(
      "This store is read-only. Manage rules from the writable archive that ingests this machine's sessions.",
    );
    expect(screen.queryByRole("button", { name: "Edit" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Delete" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Add mapping" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Apply mappings" })).toBeNull();
    expect(settingsService.postApiV1SettingsWorktreeMappings).not.toHaveBeenCalled();
    expect(settingsService.putApiV1SettingsWorktreeMappingsId).not.toHaveBeenCalled();
    expect(settingsService.deleteApiV1SettingsWorktreeMappingsId).not.toHaveBeenCalled();
    expect(settingsService.postApiV1SettingsWorktreeMappingsApply).not.toHaveBeenCalled();
  });

  it("renders rows for two mirror rules sharing id 0, machine, and path_prefix but distinct source archives", async () => {
    dataService.getApiV1DataProjectRules.mockResolvedValue(
      response("remote-host", [
        rule({
          id: 0,
          source_archive_id: "archive-a",
          project: "project-from-a",
        }),
        rule({
          id: 0,
          source_archive_id: "archive-b",
          project: "project-from-b",
        }),
      ]),
    );

    component = mountRules({ readOnly: true });
    await flush();

    const rows = document.querySelectorAll("tbody tr");
    expect(rows.length).toBe(2);
    expect(document.body.textContent).toContain("project-from-a");
    expect(document.body.textContent).toContain("project-from-b");
  });

  it("delegates machine selection to onMachineChange without a local reload", async () => {
    dataService.getApiV1DataProjectRules.mockResolvedValueOnce(
      response("local-host", [rule({ machine: "local-host" })]),
    );

    component = mountRules();
    await flush();

    await fireEvent.click(screen.getByRole("button", { name: "Select machine" }));
    await fireEvent.mouseDown(screen.getByRole("option", { name: "remote-host" }));
    await flush();

    expect(onMachineChange).toHaveBeenCalledWith("remote-host");
    // The host remounts the component keyed on the machine; a local fetch
    // here would duplicate the remount's load.
    expect(dataService.getApiV1DataProjectRules).toHaveBeenCalledTimes(1);
  });

  it("reloads rules locally for the selected machine when standalone", async () => {
    dataService.getApiV1DataProjectRules
      .mockResolvedValueOnce(response("local-host", [rule({ machine: "local-host" })]))
      .mockResolvedValueOnce(response("remote-host", [rule({ project: "remote-project" })]));

    component = mountRules({ onMachineChange: undefined });
    await flush();

    await fireEvent.click(screen.getByRole("button", { name: "Select machine" }));
    await fireEvent.mouseDown(screen.getByRole("option", { name: "remote-host" }));
    await flush();

    expect(dataService.getApiV1DataProjectRules).toHaveBeenNthCalledWith(2, {
      machine: "remote-host",
    });
    expect(document.body.textContent).toContain("remote-project");
  });

  it("cross-links target projects and shows the derived label for empty projects", async () => {
    dataService.getApiV1DataProjectRules.mockResolvedValue(
      response("remote-host", [
        rule(),
        rule({
          id: 2,
          path_prefix: "/srv/parent",
          layout: "repo_dot_worktrees",
          project: "",
          original_project: "",
        }),
      ]),
    );

    component = mountRules();
    await flush();

    await fireEvent.click(screen.getByRole("button", { name: "canonical-project" }));
    expect(onSelectProject).toHaveBeenCalledWith("canonical-project");

    const rows = document.querySelectorAll("tbody tr");
    expect(rows.length).toBe(2);
    const derivedCell = rows[1]?.querySelector("td:nth-child(2)");
    expect(derivedCell?.textContent).toContain("repo.worktrees/branch");
    expect(derivedCell?.querySelector("button")).toBeNull();
  });

  it("requests the server default machine and falls back to local_machine", async () => {
    dataService.getApiV1DataProjectRules.mockResolvedValue(response("", []));

    component = mountRules();
    await flush();

    expect(dataService.getApiV1DataProjectRules).toHaveBeenCalledWith({
      machine: undefined,
    });
    const typeahead = screen.getByRole("button", { name: "Select machine" });
    expect(typeahead.textContent).toContain("local-host");
  });

  it("preselects the machine named by the machine prop", async () => {
    dataService.getApiV1DataProjectRules.mockResolvedValue(
      response("remote-host", [rule()]),
    );

    component = mountRules({ machine: "remote-host" });
    await flush();

    expect(dataService.getApiV1DataProjectRules).toHaveBeenCalledWith(
      { machine: "remote-host" },
    );
    expect(document.body.textContent).toContain("canonical-project");
  });

  it("ignores a stale machine response after the selection changes", async () => {
    let resolveRemote!: (value: unknown) => void;
    let resolveLocal!: (value: unknown) => void;
    dataService.getApiV1DataProjectRules
      .mockResolvedValueOnce(response("local-host", [rule({ machine: "local-host", project: "local-project" })]))
      .mockReturnValueOnce(new Promise((resolve) => (resolveRemote = resolve)))
      .mockReturnValueOnce(new Promise((resolve) => (resolveLocal = resolve)));

    component = mountRules({ onMachineChange: undefined });
    await flush();

    await fireEvent.click(screen.getByRole("button", { name: "Edit" }));
    expect(screen.getByRole("button", { name: "Save mapping" })).toBeTruthy();
    await fireEvent.click(screen.getByRole("button", { name: "Select machine" }));
    await fireEvent.mouseDown(screen.getByRole("option", { name: "remote-host" }));
    expect(screen.getByRole("button", { name: "Add mapping" })).toBeTruthy();
    await fireEvent.click(screen.getByRole("button", { name: "Select machine" }));
    await fireEvent.mouseDown(screen.getByRole("option", { name: "local-host" }));

    expect(dataService.getApiV1DataProjectRules).toHaveBeenNthCalledWith(2, {
      machine: "remote-host",
    });
    expect(dataService.getApiV1DataProjectRules).toHaveBeenNthCalledWith(3, {
      machine: "local-host",
    });

    resolveLocal(response("local-host", [rule({ machine: "local-host", project: "new-local" })]));
    await flush();
    resolveRemote(response("remote-host", [rule({ project: "stale-remote" })]));
    await flush();

    expect(document.body.textContent).toContain("new-local");
    expect(document.body.textContent).not.toContain("stale-remote");
  });

  it("creates and applies mappings for the selected machine", async () => {
    dataService.getApiV1DataProjectRules
      .mockResolvedValueOnce(response("remote-host"))
      .mockResolvedValue(response("remote-host", [rule()]));
    settingsService.postApiV1SettingsWorktreeMappings.mockResolvedValue({
      id: 1,
      machine: "remote-host",
      path_prefix: "/tmp/service",
      layout: "repo_dot_worktrees",
      project: "",
      enabled: true,
      created_at: "2026-07-04T00:00:00.000Z",
      updated_at: "2026-07-04T00:00:00.000Z",
    });
    settingsService.putApiV1SettingsWorktreeMappingsId.mockResolvedValue({
      id: 1,
      machine: "test",
      path_prefix: "/tmp/service",
      layout: "explicit",
      project: "service",
      enabled: true,
      created_at: "2026-07-04T00:00:00.000Z",
      updated_at: "2026-07-04T00:00:00.000Z",
    });

    settingsService.postApiV1SettingsWorktreeMappingsApply.mockResolvedValue({
      machine: "remote-host",
      updated_sessions: 2,
      matched_sessions: 3,
    });

    component = mountRules({ readOnly: false });

    await flush();

    const layoutButton = screen.getByRole("button", { name: /repo\.worktrees/ });
    expect(layoutButton).toBeTruthy();
    layoutButton?.click();
    await new Promise((resolve) => setTimeout(resolve, 0));

    const pathPrefixInput = screen.getByRole("textbox", { name: "Parent directory" });
    const projectInput = screen.getByRole("textbox", { name: "Project" }) as HTMLInputElement;
    expect(projectInput.disabled).toBe(true);
    await fireEvent.input(pathPrefixInput, { target: { value: "/tmp/service" } });

    await fireEvent.click(screen.getByRole("button", { name: "Add mapping" }));
    await flush();

    expect(
      settingsService.postApiV1SettingsWorktreeMappings,
    ).toHaveBeenCalledWith({
      requestBody: {
        path_prefix: "/tmp/service",
        layout: "repo_dot_worktrees",
        project: "",
        enabled: true,
        machine: "remote-host",
      },
    });

    await fireEvent.click(screen.getByRole("button", { name: "Apply mappings" }));
    expect(settingsService.postApiV1SettingsWorktreeMappingsApply).toHaveBeenCalledWith({
      requestBody: { machine: "remote-host" },
    });
  });

  it("does not let a completed save overwrite the newly selected machine", async () => {
    const save = deferred<unknown>();
    dataService.getApiV1DataProjectRules
      .mockResolvedValueOnce(response("local-host"))
      .mockResolvedValueOnce(response("remote-host", [rule({ project: "remote-project" })]));
    settingsService.postApiV1SettingsWorktreeMappings.mockReturnValue(save.promise);

    component = mountRules({ onMachineChange: undefined });
    await flush();

    await fireEvent.input(screen.getByRole("textbox", { name: "Path prefix" }), {
      target: { value: "/worktrees/local" },
    });
    await fireEvent.input(screen.getByRole("textbox", { name: "Project" }), {
      target: { value: "local-project" },
    });
    await fireEvent.click(screen.getByRole("button", { name: "Add mapping" }));

    await fireEvent.click(screen.getByRole("button", { name: "Select machine" }));
    await fireEvent.mouseDown(screen.getByRole("option", { name: "remote-host" }));
    await flush();
    await fireEvent.input(screen.getByRole("textbox", { name: "Path prefix" }), {
      target: { value: "/worktrees/remote" },
    });
    await fireEvent.input(screen.getByRole("textbox", { name: "Project" }), {
      target: { value: "remote-draft" },
    });

    save.resolve(rule({ machine: "local-host", project: "local-project" }));
    await flush();

    expect(dataService.getApiV1DataProjectRules).toHaveBeenCalledTimes(2);
    expect(document.body.textContent).toContain("remote-project");
    expect((screen.getByRole("textbox", { name: "Path prefix" }) as HTMLInputElement).value)
      .toBe("/worktrees/remote");
    expect((screen.getByRole("textbox", { name: "Project" }) as HTMLInputElement).value)
      .toBe("remote-draft");
    expect(document.body.textContent).not.toContain("local-project");
  });

  it("does not show a stale apply error after the selected machine changes", async () => {
    const apply = deferred<unknown>();
    dataService.getApiV1DataProjectRules
      .mockResolvedValueOnce(response("local-host", [rule({ machine: "local-host" })]))
      .mockResolvedValueOnce(response("remote-host", [rule({ project: "remote-project" })]));
    settingsService.postApiV1SettingsWorktreeMappingsApply.mockReturnValue(apply.promise);

    component = mountRules({ onMachineChange: undefined });
    await flush();

    await fireEvent.click(screen.getByRole("button", { name: "Apply mappings" }));
    await fireEvent.click(screen.getByRole("button", { name: "Select machine" }));
    await fireEvent.mouseDown(screen.getByRole("option", { name: "remote-host" }));
    await flush();

    apply.reject(new Error("local apply failed"));
    await flush();

    expect(document.body.textContent).toContain("remote-project");
    expect(document.body.textContent).not.toContain("local apply failed");
    expect((screen.getByRole("button", { name: "Apply mappings" }) as HTMLButtonElement).disabled)
      .toBe(false);
  });

  it("does not show a stale apply result after the selected machine changes", async () => {
    const apply = deferred<unknown>();
    dataService.getApiV1DataProjectRules
      .mockResolvedValueOnce(response("local-host", [rule({ machine: "local-host" })]))
      .mockResolvedValueOnce(response("remote-host", [rule({ project: "remote-project" })]));
    settingsService.postApiV1SettingsWorktreeMappingsApply.mockReturnValue(apply.promise);

    component = mountRules({ onMachineChange: undefined });
    await flush();

    await fireEvent.click(screen.getByRole("button", { name: "Apply mappings" }));
    await fireEvent.click(screen.getByRole("button", { name: "Select machine" }));
    await fireEvent.mouseDown(screen.getByRole("option", { name: "remote-host" }));
    await flush();

    apply.resolve({ machine: "local-host", updated_sessions: 91, matched_sessions: 92 });
    await flush();

    expect(document.body.textContent).toContain("remote-project");
    expect(document.body.textContent).not.toContain("91 updated, 92 matched");
  });

  it("does not show a stale save error after the selected machine changes", async () => {
    const save = deferred<unknown>();
    dataService.getApiV1DataProjectRules
      .mockResolvedValueOnce(response("local-host"))
      .mockResolvedValueOnce(response("remote-host", [rule({ project: "remote-project" })]));
    settingsService.postApiV1SettingsWorktreeMappings.mockReturnValue(save.promise);

    component = mountRules({ onMachineChange: undefined });
    await flush();

    await fireEvent.input(screen.getByRole("textbox", { name: "Path prefix" }), {
      target: { value: "/worktrees/local" },
    });
    await fireEvent.input(screen.getByRole("textbox", { name: "Project" }), {
      target: { value: "local-project" },
    });
    await fireEvent.click(screen.getByRole("button", { name: "Add mapping" }));
    await fireEvent.click(screen.getByRole("button", { name: "Select machine" }));
    await fireEvent.mouseDown(screen.getByRole("option", { name: "remote-host" }));
    await flush();

    save.reject(new Error("local save failed"));
    await flush();

    expect(document.body.textContent).toContain("remote-project");
    expect(document.body.textContent).not.toContain("local save failed");
  });

  it("does not let a completed delete refresh the newly selected machine", async () => {
    const deletion = deferred<unknown>();
    dataService.getApiV1DataProjectRules
      .mockResolvedValueOnce(response("local-host", [rule({ machine: "local-host" })]))
      .mockResolvedValueOnce(response("remote-host", [rule({ project: "remote-project" })]));
    settingsService.deleteApiV1SettingsWorktreeMappingsId.mockReturnValue(deletion.promise);

    component = mountRules({ onMachineChange: undefined });
    await flush();

    await fireEvent.click(screen.getByRole("button", { name: "Delete" }));
    await fireEvent.click(screen.getByRole("button", { name: "Delete mapping" }));
    await fireEvent.click(screen.getByRole("button", { name: "Select machine" }));
    await fireEvent.mouseDown(screen.getByRole("option", { name: "remote-host" }));
    await flush();

    deletion.resolve(undefined);
    await flush();

    expect(dataService.getApiV1DataProjectRules).toHaveBeenCalledTimes(2);
    expect(document.body.textContent).toContain("remote-project");
  });

  it("notifies onMutated after a successful save and apply, but not on a failed save", async () => {
    const onMutated = vi.fn();
    dataService.getApiV1DataProjectRules.mockResolvedValue(
      response("remote-host", [rule()]),
    );
    settingsService.postApiV1SettingsWorktreeMappings
      .mockRejectedValueOnce(new Error("save failed"))
      .mockResolvedValueOnce(rule({ id: 2, path_prefix: "/srv/new" }));
    settingsService.postApiV1SettingsWorktreeMappingsApply.mockResolvedValue({
      machine: "remote-host",
      updated_sessions: 2,
      matched_sessions: 3,
    });

    component = mountRules({ onMutated });
    await flush();

    await fireEvent.input(screen.getByRole("textbox", { name: "Path prefix" }), {
      target: { value: "/srv/new" },
    });
    await fireEvent.input(screen.getByRole("textbox", { name: "Project" }), {
      target: { value: "new-project" },
    });
    await fireEvent.click(screen.getByRole("button", { name: "Add mapping" }));
    await flush();

    expect(document.body.textContent).toContain("save failed");
    expect(onMutated).not.toHaveBeenCalled();

    await fireEvent.click(screen.getByRole("button", { name: "Add mapping" }));
    await flush();

    expect(onMutated).toHaveBeenCalledTimes(1);

    await fireEvent.click(screen.getByRole("button", { name: "Apply mappings" }));
    await flush();

    expect(onMutated).toHaveBeenCalledTimes(2);
  });

  it("shows remembered original labels and confirms delete", async () => {
    dataService.getApiV1DataProjectRules.mockResolvedValue(
      response("remote-host", [rule()]),
    );
    settingsService.deleteApiV1SettingsWorktreeMappingsId.mockResolvedValue(undefined);
    component = mountRules();
    await flush();

    expect(document.body.textContent).toContain("branch-label");
    await fireEvent.click(screen.getByRole("button", { name: "Delete" }));
    expect(settingsService.deleteApiV1SettingsWorktreeMappingsId).not.toHaveBeenCalled();
    expect(screen.getByRole("dialog").textContent).toContain("source files still exist");
    expect(screen.getByRole("dialog").textContent).toContain("Orphaned sessions");

    await fireEvent.click(screen.getByRole("button", { name: "Delete mapping" }));
    expect(settingsService.deleteApiV1SettingsWorktreeMappingsId).toHaveBeenCalledWith({ id: "1" });
  });

  it("confirms an enabled-to-disabled save before updating", async () => {
    dataService.getApiV1DataProjectRules.mockResolvedValue(
      response("remote-host", [rule()]),
    );
    settingsService.putApiV1SettingsWorktreeMappingsId.mockResolvedValue(rule({ enabled: false }));
    component = mountRules();
    await flush();

    await fireEvent.click(screen.getByRole("button", { name: "Edit" }));
    await fireEvent.click(screen.getByRole("checkbox", { name: "Enabled" }));
    await fireEvent.click(screen.getByRole("button", { name: "Save mapping" }));

    expect(settingsService.putApiV1SettingsWorktreeMappingsId).not.toHaveBeenCalled();
    expect(screen.getByRole("dialog", { name: "Disable mapping?" })).toBeTruthy();
    await fireEvent.click(screen.getByRole("button", { name: "Disable mapping" }));
    expect(settingsService.putApiV1SettingsWorktreeMappingsId).toHaveBeenCalledWith({
      id: "1",
      requestBody: {
        enabled: false,
        layout: "explicit",
        machine: "remote-host",
        path_prefix: "/srv/worktrees/example",
        project: "canonical-project",
      },
    });
  });
});
