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
    api.candidates.mockImplementation(({ projectLabel }: { projectLabel: string }) =>
      Promise.resolve({
        candidates: [{
          id: `candidate-${projectLabel}`,
          machine: "machine-a",
          suggested_prefix: `/worktrees/${projectLabel}`,
          contributing_sessions: 1,
          distinct_cwds: 1,
          evidence_kind: "snapshot",
          examples: [],
          available: true,
        }],
      }),
    );
    api.preview.mockImplementation(({ requestBody }: { requestBody: { path_prefix: string } }) =>
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
        rows: [
          row("k1", "source-alpha"),
          row("k2", "source-beta"),
          row("k3", "source-gamma"),
        ],
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
    expect(api.apply.mock.calls.map(([request]) => request.requestBody)).toEqual([
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
});
