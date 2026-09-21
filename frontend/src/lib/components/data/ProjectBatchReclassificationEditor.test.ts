// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vite-plus/test";
import { fireEvent, screen } from "@testing-library/svelte";
import { mount, tick, unmount } from "svelte";
import type { DbProjectInventoryRow } from "../../api/generated/index";

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

  isAbortError: () => false,
}));

import ProjectBatchReclassificationEditor from "./ProjectBatchReclassificationEditor.svelte";

function row(project_key: string, label: string): DbProjectInventoryRow {
  return {
    agents: 1,
    distinct_cwds: 1,
    enabled_rules_targeting: 0,
    label,
    machines: 1,
    project_key,
    recorded_as_original: false,
    sessions: 1,
  };
}

async function flush() {
  await tick();
  await Promise.resolve();
  await tick();
}

describe("ProjectBatchReclassificationEditor", () => {
  let component: ReturnType<typeof mount> | undefined;

  beforeEach(() => {
    vi.useFakeTimers();
    api.candidates.mockReset();
    api.preview.mockReset();
    api.apply.mockReset();
    api.candidates.mockImplementation(({ project_label }: { project_label: string }) =>
      Promise.resolve({
        candidates: [
          {
            id: `candidate-${project_label}`,
            machine: "machine-a",
            suggested_prefix: `/worktrees/${project_label}`,
            contributing_sessions: 1,
            distinct_cwds: 1,
            evidence_kind: "snapshot",
            examples: [],
            available: true,
          },
        ],
      }),
    );
    api.preview.mockImplementation(
      (requestBody: { path_prefix: string; original_project: string }) =>
        Promise.resolve({
          mapping_token: `token:${requestBody.path_prefix}`,
          mapping_set_token: "rules-0",
          normalized_project: "agentsview",
          matched_sessions: 1,
          updated_sessions: 1,
          distinct_projects: 1,
          matched_projects: [requestBody.original_project],
          matched_project_keys: [{ "source-alpha": "k1", "source-beta": "k2", "source-gamma": "k3" }[requestBody.original_project]],
          matched_session_ids: [requestBody.original_project],
          updated_session_ids: [requestBody.original_project],
          project_samples: [],
          session_samples: [],
        }),
    );
    api.apply.mockResolvedValue({ mapping: {}, result: { mapping_set_token: "rules-0" } });
  });

  afterEach(() => {
    if (component) void unmount(component);
    component = undefined;
    document.body.innerHTML = "";
    vi.useRealTimers();
  });

  it.each([
    { sources: ["source-alpha"], expected: "3 projects" },
    { sources: ["source-alpha", "source-beta"], expected: "4 projects" },
  ])(
    "counts preview projects for $sources, including unselected projects only once",
    async ({ sources, expected }) => {
      api.preview.mockImplementation(({ original_project }: { original_project: string }) =>
        Promise.resolve({
          mapping_token: "preview-token",
          normalized_project: "agentsview",
          matched_sessions: 3,
          updated_sessions: 3,
          distinct_projects: original_project === "source-alpha" ? 3 : 2,
          matched_project_keys: [original_project, "unselected-key"],
          matched_session_ids: [original_project],
          updated_session_ids: [original_project],
          matched_projects:
            original_project === "source-alpha"
              ? ["source-alpha", "unselected-project", "shared-project"]
              : ["source-beta", "shared-project"],
          project_samples: [],
          session_samples: [],
        }),
      );
      component = mount(ProjectBatchReclassificationEditor, {
        target: document.body,
        props: {
          rows: sources.map((source) => row(source, source)),
          projects: [{ name: "agentsview", session_count: 20 }],
          onRefresh: vi.fn(),
          onComplete: vi.fn(),
        },
      });
      await flush();
      await fireEvent.click(screen.getByTitle("Project"));
      await fireEvent.mouseDown(screen.getByRole("option", { name: "agentsview (20)" }));
      await vi.advanceTimersByTimeAsync(300);
      await flush();
      expect(screen.getByText(expected)).toBeTruthy();
    },
  );

  it("maps every selected project's suggested folder to one target", async () => {
    const onRefresh = vi.fn().mockResolvedValue(true);
    const onComplete = vi.fn();
    component = mount(ProjectBatchReclassificationEditor, {
      target: document.body,
      props: {
        rows: [row("k1", "source-alpha"), row("k2", "source-beta"), row("k3", "source-gamma")],
        projects: [{ name: "agentsview", session_count: 20 }],
        onRefresh,
        onComplete,
      },
    });
    await flush();
    await flush();

    expect(document.body.textContent).toContain("/worktrees/source-alpha");
    expect(document.body.textContent).toContain("/worktrees/source-gamma");

    await fireEvent.click(screen.getByTitle("Project"));
    await fireEvent.mouseDown(screen.getByRole("option", { name: "agentsview (20)" }));
    await vi.advanceTimersByTimeAsync(300);
    await flush();

    await fireEvent.click(screen.getByRole("button", { name: "Save 3 corrections" }));
    await flush();
    await flush();
    await flush();

    expect(api.apply).toHaveBeenCalledTimes(3);
    expect(api.apply.mock.calls.map(([request]) => request)).toEqual([
      expect.objectContaining({
        path_prefix: "/worktrees/source-alpha",
        project: "agentsview",
        original_project: "source-alpha",
      }),
      expect.objectContaining({
        path_prefix: "/worktrees/source-beta",
        project: "agentsview",
        original_project: "source-beta",
      }),
      expect.objectContaining({
        path_prefix: "/worktrees/source-gamma",
        project: "agentsview",
        original_project: "source-gamma",
      }),
    ]);
    expect(onRefresh).toHaveBeenCalledWith("agentsview");
    expect(onComplete).toHaveBeenCalledWith("agentsview", 3);
  });

  it("counts overlapping sessions once and requires confirmation outside the selection", async () => {
    api.preview.mockImplementation(({ original_project }: { original_project: string }) => Promise.resolve({
      mapping_token: "token",
      mapping_set_token: "rules-0",
      normalized_project: "agentsview",
      matched_sessions: 2,
      updated_sessions: 1,
      distinct_projects: 2,
      matched_projects: [original_project, "outside"],
      matched_project_keys: [original_project === "source-alpha" ? "k1" : "k2", "outside-key"],
      matched_session_ids: original_project === "source-alpha" ? ["s1", "s2"] : ["s2", "s3"],
      updated_session_ids: api.apply.mock.calls.length ? [] : ["s2"],
      project_samples: [],
      session_samples: [],
    }));
    component = mount(ProjectBatchReclassificationEditor, {
      target: document.body,
      props: {
        rows: [row("k1", "source-alpha"), row("k2", "source-beta")],
        projects: [{ name: "agentsview", session_count: 20 }],
        onRefresh: vi.fn().mockResolvedValue(true),
        onComplete: vi.fn(),
      },
    });
    await flush();
    await fireEvent.click(screen.getByTitle("Project"));
    await fireEvent.mouseDown(screen.getByRole("option", { name: "agentsview (20)" }));
    await vi.advanceTimersByTimeAsync(300);
    await flush();
    expect(screen.getByText("3 sessions matched")).toBeTruthy();
    expect(screen.getByText("1 session will change")).toBeTruthy();
    await fireEvent.click(screen.getByRole("button", { name: "Save 2 corrections" }));
    expect(api.apply).not.toHaveBeenCalled();
    expect(screen.getByRole("alert").textContent).toContain("outside your selection");
    await fireEvent.click(screen.getByRole("button", { name: "Confirm and save" }));
    await flush();
    await flush();
    expect(api.apply).toHaveBeenCalledTimes(2);
  });

  it.each(["before save", "during apply"])("refreshes changed impact and requires confirmation %s", async (when) => {
    const originalPreview = api.preview.getMockImplementation()!;
    let changed = false;
    api.preview.mockImplementation(async (request) => ({
      ...await originalPreview(request),
      mapping_token: changed ? "new-preview" : "reviewed-preview",
      matched_session_ids: changed ? ["one", "two"] : ["one"],
      updated_session_ids: changed ? ["one", "two"] : ["one"],
    }));
    if (when === "during apply") {
      api.apply.mockImplementationOnce(async () => {
        changed = true;
        throw { status: 409 };
      });
    }
    const onComplete = vi.fn();
    component = mount(ProjectBatchReclassificationEditor, {
      target: document.body,
      props: {
        rows: [row("k1", "source-alpha")],
        projects: [{ name: "agentsview", session_count: 20 }],
        onRefresh: vi.fn().mockResolvedValue(true), onComplete,
      },
    });
    await flush();
    await fireEvent.click(screen.getByTitle("Project"));
    await fireEvent.mouseDown(screen.getByRole("option", { name: "agentsview (20)" }));
    await vi.advanceTimersByTimeAsync(300);
    expect(screen.getByText("1 session matched")).toBeTruthy();
    if (when === "before save") changed = true;
    await fireEvent.click(screen.getByRole("button", { name: "Save 1 correction" }));
    await flush();
    await flush();
    expect(api.apply).toHaveBeenCalledTimes(when === "before save" ? 0 : 1);
    expect(onComplete).not.toHaveBeenCalled();
    expect(screen.getByText("2 sessions matched")).toBeTruthy();
    expect(screen.getByRole("alert").textContent).toContain("review it before applying");
    await fireEvent.click(screen.getByRole("button", { name: "Back" }));
    await fireEvent.click(screen.getByRole("button", { name: "Save 1 correction" }));
    expect(onComplete).not.toHaveBeenCalled();
    await fireEvent.click(screen.getByRole("button", { name: "Confirm and save" }));
    await flush();
    await flush();
    expect(api.apply).toHaveBeenLastCalledWith(expect.objectContaining({ mapping_token: "new-preview" }));
    expect(onComplete).toHaveBeenCalledWith("agentsview", 1);
  });

  it.each(["own save", "new session", "outside rule edit"])("checks remaining corrections after %s", async (change) => {
    const originalPreview = api.preview.getMockImplementation()!;
    api.preview.mockImplementation(async (request) => {
      const afterSave = api.apply.mock.calls.length > 0;
      return {
        ...await originalPreview(request),
        mapping_token: afterSave ? "next-preview" : "reviewed-preview",
        mapping_set_token: afterSave ? (change === "outside rule edit" ? "outside-rules" : "saved-rules") : "rules-0",
        matched_session_ids: afterSave && change === "new session" ? ["shared", "new"] : ["shared"],
        updated_session_ids: afterSave ? (change === "new session" ? ["new"] : []) : ["shared"],
      };
    });
    api.apply.mockResolvedValue({ result: { mapping_set_token: "saved-rules" } });
    const onComplete = vi.fn();
    component = mount(ProjectBatchReclassificationEditor, {
      target: document.body,
      props: {
        rows: [row("k1", "source-alpha"), row("k2", "source-beta")],
        projects: [{ name: "agentsview", session_count: 20 }],
        onRefresh: vi.fn().mockResolvedValue(true), onComplete,
      },
    });
    await flush();
    await fireEvent.click(screen.getByTitle("Project"));
    await fireEvent.mouseDown(screen.getByRole("option", { name: "agentsview (20)" }));
    await vi.advanceTimersByTimeAsync(300);
    await fireEvent.click(screen.getByRole("button", { name: "Save 2 corrections" }));
    await flush();
    await flush();
    await flush();
    if (change === "own save") {
      expect(api.apply).toHaveBeenCalledTimes(2);
      expect(api.apply).toHaveBeenLastCalledWith(expect.objectContaining({ mapping_token: "next-preview" }));
      expect(onComplete).toHaveBeenCalledWith("agentsview", 2);
    } else {
      expect(api.apply).toHaveBeenCalledTimes(1);
      expect(onComplete).not.toHaveBeenCalled();
      expect(screen.getByRole("alert").textContent).toContain("review it before applying");
      expect(screen.getByRole("button", { name: "Confirm and save" })).toBeTruthy();
    }
  });

  it("shows completed corrections inside Save while the rest of the batch is pending", async () => {
    const second = Promise.withResolvers<object>();
    const refreshed = Promise.withResolvers<boolean>();
    api.apply.mockResolvedValueOnce({ result: { mapping_set_token: "rules-0" } }).mockReturnValueOnce(second.promise);
    component = mount(ProjectBatchReclassificationEditor, {
      target: document.body,
      props: {
        rows: [row("k1", "source-alpha"), row("k2", "source-beta"), row("k3", "source-gamma")],
        projects: [{ name: "agentsview", session_count: 20 }],
        onRefresh: () => refreshed.promise,
        onComplete: vi.fn(),
      },
    });
    await flush();
    await fireEvent.click(screen.getByTitle("Project"));
    await fireEvent.mouseDown(screen.getByRole("option", { name: "agentsview (20)" }));
    await vi.advanceTimersByTimeAsync(300);
    await fireEvent.click(screen.getByRole("button", { name: "Save 3 corrections" }));
    await flush();
    await flush();
    const progress = screen.getByRole("progressbar");
    expect(progress.getAttribute("aria-valuenow")).toBe("1");
    expect(progress.getAttribute("aria-valuemax")).toBe("3");
    expect(screen.getByRole("button", { name: "Saving 1 of 3…" }).contains(progress)).toBe(true);
    second.resolve({ result: { mapping_set_token: "rules-0" } });
    await flush();
    await flush();
    expect(progress.getAttribute("aria-valuenow")).toBe("3");
    refreshed.resolve(true);
    await flush();
  });
});
