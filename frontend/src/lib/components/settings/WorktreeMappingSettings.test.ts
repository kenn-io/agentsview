// @vitest-environment jsdom
import { describe, expect, it, vi } from "vitest";
import { mount, unmount } from "svelte";
// @ts-ignore
import WorktreeMappingSettings from "./WorktreeMappingSettings.svelte";
import { SettingsService } from "../../api/generated/index";

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
    SettingsService: {
      getApiV1SettingsWorktreeMappings: vi.fn(),
      postApiV1SettingsWorktreeMappings: vi.fn(),
      putApiV1SettingsWorktreeMappingsId: vi.fn(),
    },
  };
});

const settingsService = SettingsService as unknown as {
  getApiV1SettingsWorktreeMappings: ReturnType<typeof vi.fn>;
  postApiV1SettingsWorktreeMappings: ReturnType<typeof vi.fn>;
  putApiV1SettingsWorktreeMappingsId: ReturnType<typeof vi.fn>;
};

describe("WorktreeMappingSettings", () => {
  vi.clearAllMocks();

  it("does not request local worktree mappings in read-only mode", () => {
    const component = mount(WorktreeMappingSettings, {
      target: document.body,
      props: {
        readOnly: true,
      },
    });

    expect(
      settingsService.getApiV1SettingsWorktreeMappings,
    ).not.toHaveBeenCalled();
    expect(document.body.textContent).toContain("local mode");

    unmount(component);
  });

  it("sends layout rows through the generated API", async () => {
    settingsService.getApiV1SettingsWorktreeMappings.mockResolvedValue({
      machine: "test",
      mappings: [],
    });
    settingsService.postApiV1SettingsWorktreeMappings.mockResolvedValue({
      id: 1,
      machine: "test",
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

    const component = mount(WorktreeMappingSettings, {
      target: document.body,
      props: {
        readOnly: false,
      },
    });

    await new Promise((resolve) => setTimeout(resolve, 0));

    const inputs = document.body.querySelectorAll('input[type="text"]');
    const pathPrefixInput = inputs[0] as HTMLInputElement;
    const projectInput = inputs[1] as HTMLInputElement;
    const layoutButton = Array.from(document.body.querySelectorAll("button"))
      .find((button) => button.textContent?.includes("repo.worktrees"));
    expect(layoutButton).toBeTruthy();
    layoutButton?.click();
    await new Promise((resolve) => setTimeout(resolve, 0));

    expect(projectInput.disabled).toBe(true);
    pathPrefixInput.value = "/tmp/service";
    pathPrefixInput.dispatchEvent(new Event("input", { bubbles: true }));
    await new Promise((resolve) => setTimeout(resolve, 0));

    let saveButton = document.body.querySelector(
      ".primary-btn",
    ) as HTMLButtonElement;
    saveButton.click();
    await new Promise((resolve) => setTimeout(resolve, 0));

    expect(
      settingsService.postApiV1SettingsWorktreeMappings,
    ).toHaveBeenCalledWith({
      requestBody: {
        path_prefix: "/tmp/service",
        layout: "repo_dot_worktrees",
        project: "",
        enabled: true,
      },
    });

    unmount(component);
  });
});
