// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vite-plus/test";
import { fireEvent, screen } from "@testing-library/svelte";
import { mount, tick, unmount } from "svelte";
import ProjectReclassificationEditor from "./ProjectReclassificationEditor.svelte";
import { m } from "../../i18n/index.js";

const api = vi.hoisted(() => ({
  candidates: vi.fn(),
  preview: vi.fn(),
  apply: vi.fn(),
}));

vi.mock("../../api/generated/index", () => ({
  DataService: {
    getApiV1DataProjectReclassificationCandidates: api.candidates,
  },
  SettingsService: {
    postApiV1SettingsWorktreeMappingsPreview: api.preview,
    postApiV1SettingsWorktreeMappingsReclassify: api.apply,
  },
}));
vi.mock("../../api/runtime.js", () => ({
  callGenerated: vi.fn((request: () => Promise<unknown>) => request()),
  isAbortError: vi.fn(() => false),
}));

const candidate = {
  id: "candidate-1",
  machine: "remote.example",
  suggested_prefix: "/srv/worktrees/example/repo/branch",
  contributing_sessions: 3,
  distinct_cwds: 2,
  evidence_kind: "identity",
  evidence_root: "/srv/worktrees/example/repo/branch",
  examples: [],
  available: true,
};

const preview = {
  mapping_token: "token-1",
  normalized_project: "target-project",
  matched_sessions: 7,
  updated_sessions: 6,
  distinct_projects: 2,
  project_samples: [
    { project: "wrong-project", count: 6 },
    { project: "another-project", count: 1 },
  ],
  session_samples: [],
};

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

describe("ProjectReclassificationEditor", () => {
  let component: ReturnType<typeof mount> | undefined;

  beforeEach(() => {
    vi.useFakeTimers();
    api.candidates.mockReset();
    api.preview.mockReset();
    api.apply.mockReset();
    api.candidates.mockResolvedValue({ candidates: [candidate] });
    api.preview.mockResolvedValue(preview);
    api.apply.mockResolvedValue({ mapping: {}, result: preview });
  });

  afterEach(() => {
    if (component) unmount(component);
    component = undefined;
    document.body.innerHTML = "";
    vi.useRealTimers();
  });

  function render(overrides: Record<string, unknown> = {}) {
    component = mount(ProjectReclassificationEditor, {
      target: document.body,
      props: {
        projectLabel: "wrong-project",
        projectKey: "pl1:sha256:wrong",
        projects: [{ name: "target-project", session_count: 12 }],
        onRefresh: vi.fn().mockResolvedValue(true),
        onComplete: vi.fn(),
        ...overrides,
      },
    });
  }

  async function chooseTarget() {
    await fireEvent.click(screen.getByTitle("Project"));
    await fireEvent.mouseDown(screen.getByRole("option", { name: "target-project (12)" }));
    await vi.advanceTimersByTimeAsync(300);
    await flush();
  }

  it("loads archive-wide candidates, preselects one, and previews full-archive impact", async () => {
    render();
    await flush();

    expect(api.candidates.mock.lastCall?.[0]).toEqual({
      project_label: "wrong-project",
      project_key: "pl1:sha256:wrong",
    });
    expect(screen.getByText(/Current project: wrong-project/)).toBeTruthy();
    expect(screen.getByText("Observed folders")).toBeTruthy();
    expect(screen.getByRole("button", { name: candidate.suggested_prefix })).toBeTruthy();
    expect(screen.getByDisplayValue(candidate.suggested_prefix)).toBeTruthy();
    expect(screen.getByText("remote.example")).toBeTruthy();

    await chooseTarget();

    expect(api.preview.mock.lastCall?.[0]).toEqual({
      machine: "remote.example",
      path_prefix: candidate.suggested_prefix,
      project: "target-project",
      original_project: "wrong-project",
      layout: "explicit",
      enabled: true,
    });
    expect(screen.getByText(/7 sessions matched/)).toBeTruthy();
    expect(screen.getByText(/6 sessions will change/)).toBeTruthy();
    expect(screen.getByText(/2 projects/)).toBeTruthy();
    expect(screen.getByRole("alert").textContent).toContain("multiple projects");
    expect(screen.queryByText(/Will be saved as/)).toBeNull();
  });

  it("shows the localized unknown fallback for an empty label but sends the raw label", async () => {
    render({ projectLabel: "" });
    await flush();

    expect(screen.getByText(/Current project: unknown/)).toBeTruthy();
    expect(api.candidates.mock.lastCall?.[0]).toEqual({
      project_label: "",
      project_key: "pl1:sha256:wrong",
    });

    await chooseTarget();

    expect(api.preview.mock.lastCall?.[0]).toEqual(
      expect.objectContaining({ original_project: "" }),
    );
  });

  it("presents the unknown sentinel as unclassified but sends the raw label", async () => {
    render({ projectLabel: "unknown" });
    await flush();

    expect(
      screen.getByText(
        m.data_reclassify_original({
          project: m.data_project_unclassified(),
        }),
      ),
    ).toBeTruthy();
    expect(api.candidates.mock.lastCall?.[0]).toEqual({
      project_label: "unknown",
      project_key: "pl1:sha256:wrong",
    });
  });

  it("shows the server-normalized target when it differs from the typed one", async () => {
    api.preview.mockResolvedValueOnce({
      ...preview,
      normalized_project: "target_project",
    });
    render();
    await flush();
    await chooseTarget();

    expect(screen.getByText("Will be saved as target_project")).toBeTruthy();
  });

  it("debounces prefix edits and never accepts an obsolete preview token", async () => {
    render();
    await flush();
    await fireEvent.click(screen.getByTitle("Project"));
    await fireEvent.mouseDown(screen.getByRole("option", { name: "target-project (12)" }));
    const prefix = screen.getByLabelText("Folder path");
    await fireEvent.input(prefix, { target: { value: "/srv/first" } });
    await vi.advanceTimersByTimeAsync(200);
    await fireEvent.input(prefix, { target: { value: "/srv/final" } });
    await vi.advanceTimersByTimeAsync(300);
    await flush();

    expect(api.preview).toHaveBeenCalledTimes(1);
    expect(api.preview.mock.calls[0]![0].path_prefix).toBe("/srv/final");
  });

  it("commits a custom target and blocks a zero-match preview", async () => {
    api.preview.mockResolvedValueOnce({
      ...preview,
      mapping_token: "empty-token",
      matched_sessions: 0,
      updated_sessions: 0,
      distinct_projects: 0,
      project_samples: [],
    });
    render();
    await flush();
    await fireEvent.click(screen.getByTitle("Project"));
    await fireEvent.input(screen.getByRole("combobox"), {
      target: { value: "new-project" },
    });
    await fireEvent.mouseDown(screen.getByRole("option", { name: 'Use project "new-project"' }));
    await vi.advanceTimersByTimeAsync(300);
    await flush();

    expect(api.preview.mock.calls[0]![0].project).toBe("new-project");
    expect(screen.getByText(/matches no sessions/)).toBeTruthy();
    expect(
      screen.getByRole("button", { name: "Save and apply mapping" }) as HTMLButtonElement,
    ).toHaveProperty("disabled", true);
  });

  it("keeps the selected target preview across repeated empty query resets", async () => {
    render();
    await flush();
    await fireEvent.click(screen.getByTitle("Project"));
    await fireEvent.input(screen.getByRole("combobox"), {
      target: { value: "new-project" },
    });
    await fireEvent.mouseDown(screen.getByRole("option", { name: 'Use project "new-project"' }));
    await flush();

    // Typeahead reports an empty query while closing after selection, then
    // again when it reopens and closes without a new selection. Those resets
    // do not change the selected target and must not cancel its preview.
    await fireEvent.click(screen.getByTitle("Project"));
    await fireEvent.keyDown(screen.getByRole("combobox"), { key: "Escape" });
    await vi.advanceTimersByTimeAsync(300);
    await flush();

    expect(api.preview).toHaveBeenCalledTimes(1);
    expect(api.preview.mock.calls[0]![0].project).toBe("new-project");
    expect(
      screen.getByRole("button", { name: "Save and apply mapping" }) as HTMLButtonElement,
    ).toHaveProperty("disabled", false);
  });

  it("discards a preview response superseded by a later draft", async () => {
    let resolveFirst!: (value: typeof preview) => void;
    api.preview
      .mockImplementationOnce(
        () =>
          new Promise((resolve) => {
            resolveFirst = resolve;
          }),
      )
      .mockResolvedValueOnce({ ...preview, mapping_token: "latest-token" });
    render();
    await flush();
    await chooseTarget();
    const prefix = screen.getByLabelText("Folder path");
    await fireEvent.input(prefix, { target: { value: "/srv/latest" } });
    await vi.advanceTimersByTimeAsync(300);
    await flush();
    resolveFirst({ ...preview, mapping_token: "obsolete-token" });
    await flush();

    await fireEvent.click(screen.getByRole("button", { name: "Save and apply mapping" }));
    await flush();
    expect(api.apply.mock.calls[0]![0].mapping_token).toBe("latest-token");
  });

  it("invalidates an accepted preview as soon as the target query changes", async () => {
    render();
    await flush();
    await chooseTarget();
    expect(
      screen.getByRole("button", { name: "Save and apply mapping" }) as HTMLButtonElement,
    ).toHaveProperty("disabled", false);

    await fireEvent.click(screen.getByTitle("Project"));
    await fireEvent.input(screen.getByRole("combobox"), {
      target: { value: "target-pro" },
    });

    const apply = screen.getByRole("button", { name: "Save and apply mapping" });
    expect(apply as HTMLButtonElement).toHaveProperty("disabled", true);
    await fireEvent.click(apply);
    expect(api.apply).not.toHaveBeenCalled();
  });

  it("stops showing a canceled preview as loading when the draft becomes invalid", async () => {
    const pending = deferred<typeof preview>();
    api.preview.mockReturnValueOnce(pending.promise);
    render();
    await flush();
    await fireEvent.click(screen.getByTitle("Project"));
    await fireEvent.mouseDown(screen.getByRole("option", { name: "target-project (12)" }));
    await vi.advanceTimersByTimeAsync(300);
    await flush();
    expect(screen.getByText(/Calculating full-archive impact/)).toBeTruthy();

    await fireEvent.input(screen.getByLabelText("Folder path"), { target: { value: "" } });
    await flush();

    expect(screen.queryByText(/Calculating full-archive impact/)).toBeNull();
    expect(
      screen.getByRole("button", { name: "Save and apply mapping" }) as HTMLButtonElement,
    ).toHaveProperty("disabled", true);
  });

  it("shows every observed folder directly and explains unavailable path evidence", async () => {
    api.candidates.mockResolvedValue({
      candidates: [
        candidate,
        {
          ...candidate,
          id: "candidate-2",
          machine: "other.example",
          suggested_prefix: "/",
          available: false,
        },
      ],
    });
    render();
    await flush();

    expect(screen.getByText("Observed folders")).toBeTruthy();
    expect(screen.getByRole("button", { name: candidate.suggested_prefix })).toBeTruthy();
    expect(screen.getByRole("button", { name: "/" })).toBeTruthy();
    expect(screen.queryByLabelText("Folder path")).toBeNull();
    await fireEvent.click(screen.getByRole("button", { name: "/" }));
    await flush();
    expect(screen.getByText(/filesystem root/)).toBeTruthy();
    expect(
      screen.getByRole("button", { name: "Save and apply mapping" }) as HTMLButtonElement,
    ).toHaveProperty("disabled", true);
  });

  it("does not offer mapping controls for a filesystem-root candidate", async () => {
    api.candidates.mockResolvedValue({
      candidates: [
        {
          ...candidate,
          suggested_prefix: "/",
          available: false,
        },
      ],
    });
    render();
    await flush();

    expect(screen.getByRole("button", { name: "/" })).toBeTruthy();
    expect(screen.getByText(/filesystem root/)).toBeTruthy();
    expect(screen.queryByLabelText("Folder path")).toBeNull();
    expect(screen.queryByTitle("Project")).toBeNull();
    expect(screen.queryByRole("button", { name: "Save and apply mapping" })).toBeNull();
  });

  it("applies exactly once and offers only refresh retry after a post-commit failure", async () => {
    api.preview.mockResolvedValue({ ...preview, normalized_project: "renamed-target" });
    const retry = deferred<boolean>();
    const onRefresh = vi.fn().mockResolvedValueOnce(false).mockReturnValueOnce(retry.promise);
    const onComplete = vi.fn();
    render({ onRefresh, onComplete });
    await flush();
    await chooseTarget();

    const apply = screen.getByRole("button", { name: "Save and apply mapping" });
    await fireEvent.click(apply);
    await flush();

    expect(api.apply).toHaveBeenCalledTimes(1);
    expect(api.apply.mock.calls[0]![0].mapping_token).toBe("token-1");
    expect(onRefresh).toHaveBeenCalledWith("renamed-target");
    expect(screen.queryByRole("button", { name: "Save and apply mapping" })).toBeNull();
    expect(screen.getByText(/mapping was saved, but the project inventory/)).toBeTruthy();
    const retryButton = screen.getByRole("button", { name: "Retry refresh" });
    await fireEvent.click(retryButton);
    await fireEvent.click(retryButton);
    await flush();
    expect(api.apply).toHaveBeenCalledTimes(1);
    expect(onRefresh).toHaveBeenCalledTimes(2);
    expect(screen.getByRole("button", { name: "Refreshing…" }) as HTMLButtonElement).toHaveProperty(
      "disabled",
      true,
    );

    retry.resolve(true);
    await flush();
    expect(onComplete).toHaveBeenCalledTimes(1);
  });

  it("marks the initial post-commit refresh in flight before exposing a retry", async () => {
    const pendingRefresh = deferred<boolean>();
    const onRefresh = vi.fn().mockReturnValue(pendingRefresh.promise);
    render({ onRefresh });
    await flush();
    await chooseTarget();

    await fireEvent.click(screen.getByRole("button", { name: "Save and apply mapping" }));
    await flush();

    const refreshing = screen.getByRole("button", { name: "Refreshing…" });
    expect(refreshing as HTMLButtonElement).toHaveProperty("disabled", true);
    expect(screen.queryByText(/mapping was saved, but the project inventory/)).toBeNull();
    await fireEvent.click(refreshing);
    expect(onRefresh).toHaveBeenCalledTimes(1);

    pendingRefresh.resolve(false);
    await flush();
    expect(screen.getByText(/mapping was saved, but the project inventory/)).toBeTruthy();
    expect(
      screen.getByRole("button", { name: "Retry refresh" }) as HTMLButtonElement,
    ).toHaveProperty("disabled", false);
  });

  it("keeps apply in flight and refreshes with the trimmed draft target when unnormalized", async () => {
    api.preview.mockResolvedValue({ ...preview, normalized_project: "" });
    const pendingApply = deferred<{ mapping: object; result: typeof preview }>();
    const onRefresh = vi.fn().mockResolvedValue(false);
    api.apply.mockReturnValueOnce(pendingApply.promise);
    render({ onRefresh });
    await flush();
    await chooseTarget();

    await fireEvent.click(screen.getByRole("button", { name: "Save and apply mapping" }));
    await flush();
    const applying = screen.getByRole("button", { name: "Saving and applying…" });
    expect(applying as HTMLButtonElement).toHaveProperty("disabled", true);
    await fireEvent.click(applying);
    expect(onRefresh).not.toHaveBeenCalled();

    pendingApply.resolve({ mapping: {}, result: preview });
    await flush();
    expect(api.apply).toHaveBeenCalledTimes(1);
    expect(onRefresh).toHaveBeenCalledTimes(1);
    expect(onRefresh).toHaveBeenCalledWith("target-project");
  });

  it("refreshes with the target captured at apply time, not a later edit", async () => {
    const pendingApply = deferred<{ mapping: object; result: typeof preview }>();
    api.apply.mockReturnValueOnce(pendingApply.promise);
    const onRefresh = vi.fn().mockResolvedValue(true);
    render({ onRefresh });
    await flush();
    await chooseTarget();

    await fireEvent.click(screen.getByRole("button", { name: "Save and apply mapping" }));
    await flush();

    // Change the target selection while the reclassify request is in flight.
    await fireEvent.click(screen.getByTitle("Project"));
    await fireEvent.input(screen.getByRole("combobox"), {
      target: { value: "other-project" },
    });
    await fireEvent.mouseDown(screen.getByRole("option", { name: 'Use project "other-project"' }));
    await flush();

    pendingApply.resolve({ mapping: {}, result: preview });
    await flush();

    expect(onRefresh).toHaveBeenCalledTimes(1);
    expect(onRefresh).toHaveBeenCalledWith("target-project");
  });

  it("still refreshes the inventory when the editor unmounts mid-apply", async () => {
    const pendingApply = deferred<{ mapping: object; result: typeof preview }>();
    api.apply.mockReturnValueOnce(pendingApply.promise);
    const onRefresh = vi.fn().mockResolvedValue(true);
    const onComplete = vi.fn();
    render({ onRefresh, onComplete });
    await flush();
    await chooseTarget();

    await fireEvent.click(screen.getByRole("button", { name: "Save and apply mapping" }));
    await flush();

    if (component) unmount(component);
    component = undefined;
    pendingApply.resolve({ mapping: {}, result: preview });
    await flush();

    expect(onRefresh).toHaveBeenCalledTimes(1);
    expect(onRefresh).toHaveBeenCalledWith("target-project");
    expect(onComplete).not.toHaveBeenCalled();
  });

  it("localizes and number-formats project sample session counts", async () => {
    api.preview.mockResolvedValueOnce({
      ...preview,
      project_samples: [{ project: "wrong-project", count: 1234 }],
    });
    render();
    await flush();
    await chooseTarget();

    expect(screen.getByRole("alert").textContent).toContain("wrong-project (1,234 sessions)");
  });

  it("keeps same-machine folders distinct without repeating the machine on every row", async () => {
    api.candidates.mockResolvedValue({
      candidates: [
        candidate,
        {
          ...candidate,
          id: "candidate-2",
          suggested_prefix: "/srv/worktrees/example/repo/other-branch",
        },
      ],
    });
    render();
    await flush();

    const otherPath = "/srv/worktrees/example/repo/other-branch";
    expect(screen.getByRole("button", { name: candidate.suggested_prefix })).toBeTruthy();
    expect(screen.getByText("branch")).toBeTruthy();
    expect(screen.getByText("other-branch")).toBeTruthy();
    expect(screen.getAllByText("remote.example")).toHaveLength(1);
    await fireEvent.click(screen.getByRole("button", { name: otherPath }));
    await flush();

    expect(screen.getByDisplayValue(otherPath)).toBeTruthy();
  });

  it("invokes onOpenRules with the candidate's machine", async () => {
    const onOpenRules = vi.fn();
    render({ onOpenRules });
    await flush();

    await fireEvent.click(screen.getByRole("button", { name: "Open Rules" }));

    expect(onOpenRules).toHaveBeenCalledTimes(1);
    expect(onOpenRules).toHaveBeenCalledWith("remote.example");
  });

  it("refreshes the preview after a mapping-set conflict", async () => {
    api.apply.mockRejectedValueOnce(Object.assign(new Error("changed"), { status: 409 }));
    render();
    await flush();
    await chooseTarget();
    await fireEvent.click(screen.getByRole("button", { name: "Save and apply mapping" }));
    await flush();

    expect(api.apply).toHaveBeenCalledTimes(1);
    expect(api.preview).toHaveBeenCalledTimes(2);
    expect(screen.getByText(/changed since the preview/)).toBeTruthy();
    expect(screen.getByRole("button", { name: "Save and apply mapping" })).toBeTruthy();
  });

  it("renders candidates as read-only evidence without editing, preview, or apply", async () => {
    api.candidates.mockResolvedValue({
      candidates: [
        candidate,
        {
          ...candidate,
          id: "candidate-2",
          suggested_prefix: "/srv/worktrees/example/repo/other-branch",
        },
      ],
    });
    render({ readOnly: true });
    await flush();

    expect(screen.getByText(/This store is read-only/)).toBeTruthy();
    expect(screen.getByRole("note").textContent).toContain("read-only");
    const otherPath = "/srv/worktrees/example/repo/other-branch";
    await fireEvent.click(screen.getByRole("button", { name: otherPath }));
    await vi.advanceTimersByTimeAsync(300);
    await flush();

    expect(screen.getAllByText("remote.example").length).toBeGreaterThan(0);
    expect(screen.getAllByText(/3 sessions/).length).toBeGreaterThan(0);
    expect(screen.queryByLabelText("Folder path")).toBeNull();
    expect(screen.queryByTitle("Project")).toBeNull();
    expect(screen.queryByRole("button", { name: "Save and apply mapping" })).toBeNull();
    expect(api.preview).not.toHaveBeenCalled();
  });

  it("preselects the sole candidate as read-only evidence without previewing", async () => {
    render({ readOnly: true });
    await flush();
    await vi.advanceTimersByTimeAsync(300);
    await flush();

    expect(screen.getAllByText("remote.example").length).toBeGreaterThan(0);
    expect(screen.getAllByText(/3 sessions/).length).toBeGreaterThan(0);
    expect(screen.getByRole("button", { name: candidate.suggested_prefix })).toBeTruthy();
    expect(screen.queryByLabelText("Folder path")).toBeNull();
    expect(screen.queryByTitle("Project")).toBeNull();
    expect(screen.queryByRole("button", { name: "Save and apply mapping" })).toBeNull();
    expect(api.preview).not.toHaveBeenCalled();
  });

  it("shows the rules note with the Open Rules action only when onOpenRules is provided", async () => {
    render({ onOpenRules: vi.fn() });
    await flush();

    expect(screen.getByText(/edited in the Rules view/)).toBeTruthy();
    expect(screen.getByRole("button", { name: "Open Rules" })).toBeTruthy();

    if (component) unmount(component);
    component = undefined;
    document.body.innerHTML = "";
    render();
    await flush();

    expect(screen.queryByText(/edited in the Rules view/)).toBeNull();
    expect(screen.queryByRole("button", { name: "Open Rules" })).toBeNull();
  });
});
