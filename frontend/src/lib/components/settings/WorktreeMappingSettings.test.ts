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
    },
  };
});

const settingsService = SettingsService as unknown as {
  getApiV1SettingsWorktreeMappings: ReturnType<typeof vi.fn>;
};

describe("WorktreeMappingSettings", () => {
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
});
