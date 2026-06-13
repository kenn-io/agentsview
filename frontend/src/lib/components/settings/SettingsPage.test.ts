// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { mount, tick, unmount } from "svelte";
// @ts-ignore
import SettingsPage from "./SettingsPage.svelte";
import { SettingsService } from "../../api/generated/index";
import { settings } from "../../stores/settings.svelte.js";

vi.mock("../../api/runtime.js", async (importOriginal) => {
  const orig =
    await importOriginal<typeof import("../../api/runtime.js")>();
  return {
    ...orig,
    configureGeneratedClient: vi.fn(),
    getAuthToken: vi.fn(() => ""),
    isRemoteConnection: vi.fn(() => false),
    setAuthToken: vi.fn(),
    setServerUrl: vi.fn(),
  };
});

vi.mock("../../api/generated/index", async (importOriginal) => {
  const orig =
    await importOriginal<typeof import("../../api/generated/index")>();
  return {
    ...orig,
    SettingsService: {
      getApiV1Settings: vi.fn(),
      getApiV1SettingsWorktreeMappings: vi.fn(),
    },
  };
});

const settingsService = SettingsService as unknown as {
  getApiV1Settings: ReturnType<typeof vi.fn>;
  getApiV1SettingsWorktreeMappings: ReturnType<typeof vi.fn>;
};

beforeEach(() => {
  vi.clearAllMocks();
  settings.loading = false;
  settings.loaded = false;
  settings.needsAuth = false;
  settings.error = null;
  settings.readOnly = false;
});

afterEach(() => {
  document.body.innerHTML = "";
});

describe("SettingsPage", () => {
  it("does not mount local-only settings before backend mode is loaded", async () => {
    let resolveSettings!: (value: unknown) => void;
    settingsService.getApiV1Settings.mockReturnValue(
      new Promise((resolve) => {
        resolveSettings = resolve;
      }),
    );

    const component = mount(SettingsPage, {
      target: document.body,
    });
    await tick();

    expect(document.body.textContent).toContain("Loading settings");
    expect(
      settingsService.getApiV1SettingsWorktreeMappings,
    ).not.toHaveBeenCalled();

    resolveSettings({
      agent_dirs: {},
      github_configured: false,
      host: "127.0.0.1",
      port: 8080,
      read_only: true,
      require_auth: false,
      terminal: { mode: "auto" },
    });
    await tick();

    expect(
      settingsService.getApiV1SettingsWorktreeMappings,
    ).not.toHaveBeenCalled();

    unmount(component);
  });
});
