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
  callGenerated: (request: () => Promise<unknown>) => request(),
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
    api.preview.mockImplementation((requestBody: { path_prefix: string }) =>
      Promise.resolve({
        mapping_token: `token:${requestBody.path_prefix}`,
        normalized_project: "agentsview",
        matched_sessions: 1,
        updated_sessions: 1,
        distinct_projects: 1,
        project_samples: [],
        session_samples: [],
      }),
    );
    api.apply.mockResolvedValue({ mapping: {}, result: {} });
  });

  afterEach(() => {
    if (component) void unmount(component);
    component = undefined;
    document.body.innerHTML = "";
    vi.useRealTimers();
  });

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

  it("shows completed corrections inside Save while the rest of the batch is pending", async () => {
    const second = Promise.withResolvers<object>();
    const refreshed = Promise.withResolvers<boolean>();
    api.apply.mockResolvedValueOnce({}).mockReturnValueOnce(second.promise);
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
    second.resolve({});
    await flush();
    await flush();
    expect(progress.getAttribute("aria-valuenow")).toBe("3");
    refreshed.resolve(true);
    await flush();
  });
});
