// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vite-plus/test";
import { fireEvent, screen } from "@testing-library/svelte";
import { mount, tick, unmount } from "svelte";
import type { DbProjectInventoryRow } from "../../api/generated/index";
import ProjectWorkspace from "./ProjectWorkspace.svelte";
import { m } from "../../i18n/index.js";

const api = vi.hoisted(() => ({
  candidates: vi.fn(),
  listSessions: vi.fn(),
  listMessages: vi.fn(),
  preview: vi.fn(),
  apply: vi.fn(),
  assignSession: vi.fn(),
}));

vi.mock("../../api/generated/index", () => ({
  DataService: {
    getApiV1DataProjectReclassificationCandidates: api.candidates,
  },
  SessionsService: {
    getApiV1Sessions: api.listSessions,
    getApiV1SessionsIdMessages: api.listMessages,
  },
  SettingsService: {
    postApiV1SettingsWorktreeMappingsPreview: api.preview,
    postApiV1SettingsWorktreeMappingsReclassify: api.apply,
    putApiV1SettingsSessionProjectAssignmentsSessionId: api.assignSession,
  },
}));
vi.mock("../../api/runtime.js", () => ({
  callGenerated: vi.fn((request: () => Promise<unknown>) => request()),
  isAbortError: vi.fn(() => false),
  configureGeneratedClient: vi.fn(),
  isRemoteConnection: vi.fn(() => false),
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

function makeRow(overrides: Partial<DbProjectInventoryRow> = {}): DbProjectInventoryRow {
  return {
    agents: 2,
    distinct_cwds: 3,
    enabled_rules_targeting: 0,
    first_activity: "2026-01-05T10:00:00Z",
    label: "wrong-project",
    last_activity: "2026-03-09T18:30:00Z",
    machines: 2,
    project_key: "pl1:sha256:wrong",
    recorded_as_original: false,
    sessions: 9,
    ...overrides,
  };
}

async function flush() {
  await tick();
  await Promise.resolve();
  await tick();
}

function deferred<T>() {
  let resolve!: (value: T) => void;
  let reject!: (reason?: unknown) => void;
  const promise = new Promise<T>((res, rej) => {
    resolve = res;
    reject = rej;
  });
  return { promise, resolve, reject };
}

describe("ProjectWorkspace", () => {
  let component: ReturnType<typeof mount> | undefined;

  beforeEach(() => {
    api.candidates.mockReset();
    api.listSessions.mockReset();
    api.listMessages.mockReset();
    api.preview.mockReset();
    api.apply.mockReset();
    api.assignSession.mockReset();
    api.candidates.mockResolvedValue({ candidates: [] });
    api.listSessions.mockResolvedValue({
      sessions: [
        {
          id: "session-automated",
          project: "wrong-project",
          cwd: "/srv/worktrees/example/repo",
          display_name: "Automated review",
          first_message: "Review the changes",
          agent: "codex",
          started_at: "2026-03-09T18:32:00Z",
          is_automated: true,
        },
        {
          id: "session-1",
          project: "wrong-project",
          cwd: "/srv/worktrees/example/repo",
          display_name: "Fix project mapping",
          first_message: "Work out which project this session belongs to",
          agent: "codex",
          started_at: "2026-03-09T18:30:00Z",
          is_automated: false,
        },
      ],
      total: 2,
    });
    api.listMessages.mockResolvedValue({
      count: 2,
      messages: [
        {
          id: 1,
          session_id: "session-1",
          ordinal: 0,
          role: "user",
          content: "Work out which project this session belongs to",
          timestamp: "2026-03-09T18:30:00Z",
          has_thinking: false,
          thinking_text: "",
          has_tool_use: false,
          content_length: 47,
          model: "",
          context_tokens: 0,
          output_tokens: 0,
          is_system: false,
        },
        {
          id: 2,
          session_id: "session-1",
          ordinal: 1,
          role: "assistant",
          content: "The repository layout shows this belongs to project A.",
          timestamp: "2026-03-09T18:31:00Z",
          has_thinking: false,
          thinking_text: "",
          has_tool_use: false,
          content_length: 53,
          model: "codex",
          context_tokens: 0,
          output_tokens: 0,
          is_system: false,
        },
      ],
    });
    api.assignSession.mockResolvedValue({
      session_id: "session-1",
      project: "target-project",
      created_at: "2026-03-09T18:35:00Z",
      updated_at: "2026-03-09T18:35:00Z",
    });
  });

  afterEach(() => {
    if (component) void unmount(component);
    component = undefined;
    document.body.innerHTML = "";
  });

  function render(overrides: Record<string, unknown> = {}) {
    component = mount(ProjectWorkspace, {
      target: document.body,
      props: {
        row: makeRow(),
        projects: [{ name: "target-project", session_count: 12 }],
        readOnly: false,
        onClose: vi.fn(),
        onRefresh: vi.fn().mockResolvedValue(true),
        onComplete: vi.fn(),
        onOpenRules: vi.fn(),
        ...overrides,
      },
    });
  }

  it("renders the compact project context with counts, suggestions, and active rules", async () => {
    api.candidates.mockResolvedValue({ candidates: [candidate] });
    render({ row: makeRow({ enabled_rules_targeting: 2, recorded_as_original: true }) });
    await flush();

    expect(screen.getByRole("heading", { name: "wrong-project" })).toBeTruthy();
    const text = document.body.textContent ?? "";
    expect(text).toContain(m.data_summary_sessions({ count: 9 }));
    expect(text).toContain(m.data_summary_machines({ count: 2 }));
    expect(text).toContain(m.data_summary_folder_suggestions({ count: 1 }));
    expect(text).toContain(m.data_rules_targeting({ count: 2 }));
  });

  it("falls back to the localized unknown label for an empty project label", async () => {
    render({ row: makeRow({ label: "" }) });
    await flush();

    expect(screen.getByRole("heading", { name: m.shared_unknown() })).toBeTruthy();
    const section = document.querySelector("section.workspace");
    expect(section?.getAttribute("aria-label")).toBe(m.shared_unknown());
    // The editor must still receive the raw empty label; it feeds
    // original_project and API calls, not display copy.
    expect(api.candidates).toHaveBeenCalledWith({
      projectLabel: "",
      projectKey: "pl1:sha256:wrong",
    });
  });

  it("presents the unknown sentinel as unclassified without changing API identity", async () => {
    render({ row: makeRow({ label: "unknown" }) });
    await flush();

    expect(screen.getByRole("heading", { name: m.data_project_unclassified() })).toBeTruthy();
    expect(api.candidates).toHaveBeenCalledWith({
      projectLabel: "unknown",
      projectKey: "pl1:sha256:wrong",
    });
  });

  it("mounts the editor for the row's label and key", async () => {
    render();
    await flush();

    expect(api.candidates).toHaveBeenCalledWith({
      projectLabel: "wrong-project",
      projectKey: "pl1:sha256:wrong",
    });
  });

  it("shows a human-started session before newer automated sessions", async () => {
    render();
    await flush();
    await flush();

    expect(api.listSessions).toHaveBeenCalledWith({
      project: "wrong-project",
      includeOneShot: true,
      includeAutomated: true,
      includeChildren: true,
      limit: 20,
      orderBy: "recent",
    });
    expect(api.listMessages).toHaveBeenCalledWith({
      id: "session-1",
      limit: 12,
      direction: "asc",
      roles: "user,assistant",
    });
    expect(screen.getByText("The repository layout shows this belongs to project A.")).toBeTruthy();
    expect(screen.queryByTitle(m.message_content_pin_message())).toBeNull();
  });

  it("keeps cached transcript content when a session left behind fails", async () => {
    const automatedMessages = deferred<never>();
    api.listMessages.mockImplementation(({ id }: { id: string }) => {
      if (id === "session-automated") return automatedMessages.promise;
      return Promise.resolve({
        count: 1,
        messages: [
          {
            id: 1,
            session_id: "session-1",
            ordinal: 0,
            role: "user",
            content: "Cached mapping context",
            timestamp: "2026-03-09T18:30:00Z",
            has_thinking: false,
            thinking_text: "",
            has_tool_use: false,
            content_length: 22,
            model: "",
            context_tokens: 0,
            output_tokens: 0,
            is_system: false,
          },
        ],
      });
    });
    render();
    await flush();
    await flush();
    await fireEvent.click(
      screen.getByRole("button", { name: m.data_reclassify_session_preview_next() }),
    );
    await fireEvent.click(
      screen.getByRole("button", { name: m.data_reclassify_session_preview_previous() }),
    );
    automatedMessages.reject(new Error("late failure"));
    await flush();

    expect(screen.getByText("Cached mapping context")).toBeTruthy();
    expect(screen.queryByText(m.data_reclassify_session_preview_failed())).toBeNull();
  });

  it("assigns only the active session to a selected project", async () => {
    const onRefresh = vi.fn().mockResolvedValue(true);
    render({ onRefresh });
    await flush();
    await flush();

    api.listSessions.mockResolvedValueOnce({
      sessions: [
        {
          id: "session-automated",
          project: "wrong-project",
          cwd: "/srv/worktrees/example/repo",
          display_name: "Automated review",
          first_message: "Review the changes",
          agent: "codex",
          started_at: "2026-03-09T18:32:00Z",
          is_automated: true,
        },
      ],
      total: 1,
    });

    await fireEvent.click(screen.getByTitle(m.data_session_assignment_target()));
    await fireEvent.mouseDown(screen.getByRole("option", { name: "target-project (12)" }));
    await flush();
    await fireEvent.click(screen.getByRole("button", { name: m.data_session_assignment_save() }));
    await flush();

    expect(api.assignSession).toHaveBeenCalledWith({
      sessionId: "session-1",
      requestBody: { project: "target-project" },
    });
    expect(onRefresh).toHaveBeenCalledWith("pl1:sha256:wrong", "target-project");
    expect(api.listMessages).toHaveBeenLastCalledWith({
      id: "session-automated",
      limit: 12,
      direction: "asc",
      roles: "user,assistant",
    });
  });

  it("disables carousel navigation while an assignment is pending", async () => {
    const assignment = deferred<{
      session_id: string;
      project: string;
      created_at: string;
      updated_at: string;
    }>();
    api.assignSession.mockReturnValueOnce(assignment.promise);
    render();
    await flush();
    await flush();

    await fireEvent.click(screen.getByTitle(m.data_session_assignment_target()));
    await fireEvent.mouseDown(screen.getByRole("option", { name: "target-project (12)" }));
    await flush();
    await fireEvent.click(screen.getByRole("button", { name: m.data_session_assignment_save() }));
    await flush();

    expect(
      screen.getByRole("button", { name: m.data_reclassify_session_preview_previous() })
        .getAttribute("disabled"),
    ).not.toBeNull();
    expect(
      screen.getByRole("button", { name: m.data_reclassify_session_preview_next() })
        .getAttribute("disabled"),
    ).not.toBeNull();

    assignment.resolve({
      session_id: "session-1",
      project: "target-project",
      created_at: "2026-03-09T18:35:00Z",
      updated_at: "2026-03-09T18:35:00Z",
    });
    await flush();
  });

  it("reports when an assignment is saved but inventory refresh fails", async () => {
    render({ onRefresh: vi.fn().mockResolvedValue(false) });
    await flush();
    await flush();

    await fireEvent.click(screen.getByTitle(m.data_session_assignment_target()));
    await fireEvent.mouseDown(screen.getByRole("option", { name: "target-project (12)" }));
    await flush();
    await fireEvent.click(screen.getByRole("button", { name: m.data_session_assignment_save() }));
    await flush();

    expect(api.assignSession).toHaveBeenCalledTimes(1);
    expect(api.listSessions).toHaveBeenCalledTimes(2);
    expect(screen.getByText(m.data_session_assignment_refresh_failed())).toBeTruthy();
  });

  it("lets users pin a session to its currently inferred project", async () => {
    const onRefresh = vi.fn().mockResolvedValue(true);
    api.assignSession.mockResolvedValue({
      session_id: "session-1",
      project: "wrong-project",
      created_at: "2026-03-09T18:35:00Z",
      updated_at: "2026-03-09T18:35:00Z",
    });
    render({
      projects: [{ name: "wrong-project", session_count: 9 }],
      onRefresh,
    });
    await flush();
    await flush();

    await fireEvent.click(screen.getByTitle(m.data_session_assignment_target()));
    await fireEvent.mouseDown(screen.getByRole("option", { name: "wrong-project (9)" }));
    await flush();
    const assignButton = screen.getByRole("button", {
      name: m.data_session_assignment_save(),
    });
    expect(assignButton.getAttribute("disabled")).toBeNull();
    await fireEvent.click(assignButton);
    await flush();

    expect(api.assignSession).toHaveBeenCalledWith({
      sessionId: "session-1",
      requestBody: { project: "wrong-project" },
    });
    expect(onRefresh).toHaveBeenCalledWith("pl1:sha256:wrong", "wrong-project");
    expect(api.listMessages).toHaveBeenLastCalledWith({
      id: "session-1",
      limit: 12,
      direction: "asc",
      roles: "user,assistant",
    });
  });

  it("opens the existing all-folders correction for one selected project", async () => {
    api.candidates.mockResolvedValue({ candidates: [candidate] });
    render();
    await flush();

    await fireEvent.click(
      screen.getByRole("button", { name: m.data_workspace_map_whole_project() }),
    );
    await flush();

    expect(screen.getByRole("heading", { name: m.data_batch_correction_heading() })).toBeTruthy();
    expect(
      screen.getByRole("button", { name: m.data_workspace_correct_one_folder() }),
    ).toBeTruthy();
    expect(api.candidates).toHaveBeenLastCalledWith({
      projectLabel: "wrong-project",
      projectKey: "pl1:sha256:wrong",
    });
  });

  it("invokes onClose from the close button and from Escape inside the panel", async () => {
    const onClose = vi.fn();
    render({ onClose });
    await flush();

    const closeButton = screen.getByRole("button", { name: m.data_workspace_close() });
    await fireEvent.click(closeButton);
    expect(onClose).toHaveBeenCalledTimes(1);

    await fireEvent.keyDown(closeButton, { key: "Escape" });
    expect(onClose).toHaveBeenCalledTimes(2);
  });

  it("closes only the target-project dropdown on Escape, without calling onClose", async () => {
    const onClose = vi.fn();
    api.candidates.mockResolvedValue({ candidates: [candidate] });
    render({ onClose });
    await flush();

    await fireEvent.click(screen.getByTitle("Project"));
    await fireEvent.keyDown(screen.getByRole("combobox"), { key: "Escape" });

    expect(onClose).not.toHaveBeenCalled();
  });

  it("passes readOnly through to the editor", async () => {
    render({ readOnly: true });
    await flush();

    expect(screen.getByRole("note").textContent).toContain(m.data_reclassify_read_only());
  });
});
