// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vite-plus/test";
import { mount, tick, unmount } from "svelte";
import { dismissFlash } from "@kenn-io/kit-ui";
// @ts-ignore
import SettingsPage from "./SettingsPage.svelte";
import { SettingsService } from "../../api/generated/index";
import { settings } from "../../stores/settings.svelte.js";
import { router } from "../../stores/router.svelte.js";
import { initI18n, LOCALE_STORAGE_KEY, setLocale } from "../../i18n/index.js";

vi.mock("../../api/runtime.js", async (importOriginal) => {
  const orig = await importOriginal<typeof import("../../api/runtime.js")>();
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
  const orig = await importOriginal<typeof import("../../api/generated/index")>();
  return {
    ...orig,
    SettingsService: {
      getApiV1Settings: vi.fn(),
      putApiV1Settings: vi.fn(),
    },
  };
});

const settingsService = SettingsService as unknown as {
  getApiV1Settings: ReturnType<typeof vi.fn>;
  putApiV1Settings: ReturnType<typeof vi.fn>;
};

beforeEach(() => {
  vi.clearAllMocks();
  localStorage.clear();
  initI18n();
  settings.loading = false;
  settings.loaded = false;
  settings.needsAuth = false;
  settings.error = null;
  settings.saveError = null;
  settings.readOnly = false;
  settings.saving = false;
  dismissFlash();
});

afterEach(() => {
  dismissFlash();
  document.body.innerHTML = "";
});

describe("SettingsPage", () => {
  it("renders browser-local settings with the Data-mode worktree pointer", async () => {
    let resolveSettings!: (value: unknown) => void;
    settingsService.getApiV1Settings.mockReturnValue(
      new Promise((resolve) => {
        resolveSettings = resolve;
      }),
    );
    const navigate = vi.spyOn(router, "navigate").mockReturnValue(true);

    const component = mount(SettingsPage, {
      target: document.body,
    });
    await tick();

    expect(document.body.textContent).toContain("Loading settings");
    expect(document.body.textContent).not.toContain("Date ranges");

    resolveSettings({
      agent_dirs: {},
      chart_palette: "agentsview",
      github_configured: false,
      host: "127.0.0.1",
      port: 8080,
      read_only: true,
      require_auth: false,
      terminal: { mode: "auto" },
    });
    await tick();
    await tick();

    expect(document.body.textContent).toContain("Date ranges");
    expect(document.body.textContent).toContain("Link date ranges across pages");
    // The mapping manager moved to Data; Settings keeps only a pointer.
    expect(document.body.textContent).toContain("Worktree mappings");
    expect(document.body.textContent).toContain("Project classification rules have moved to Data.");
    expect(document.body.textContent).not.toContain("available in local mode only");

    const pointer = Array.from(document.body.querySelectorAll("button")).find((b) =>
      b.textContent?.includes("Open Data › Rules"),
    );
    expect(pointer).toBeTruthy();
    pointer!.click();
    await tick();
    expect(navigate).toHaveBeenCalledWith("data", { view: "rules" });

    unmount(component);
    navigate.mockRestore();
  });

  it("persists the selected interface language for reload", async () => {
    settingsService.getApiV1Settings.mockResolvedValue({
      agent_dirs: {},
      chart_palette: "agentsview",
      github_configured: false,
      host: "127.0.0.1",
      port: 8080,
      read_only: false,
      require_auth: false,
      terminal: { mode: "auto" },
    });

    const component = mount(SettingsPage, {
      target: document.body,
    });
    await tick();
    await tick();

    expect(document.body.querySelector('select[aria-label="Interface language"]')).toBeNull();
    expect(document.body.textContent).toContain("Settings");

    const trigger = document.body.querySelector(
      'button[title="Interface language"]',
    ) as HTMLButtonElement | null;
    expect(trigger).toBeTruthy();

    trigger!.click();
    await tick();

    const option = Array.from(document.body.querySelectorAll('[role="option"]')).find((el) =>
      el.textContent?.includes("日本語"),
    );
    expect(option).toBeTruthy();

    (option as HTMLElement).dispatchEvent(new MouseEvent("mousedown", { bubbles: true }));
    await tick();

    expect(localStorage.getItem(LOCALE_STORAGE_KEY)).toBe("ja");
    expect(document.body.textContent).toContain("Settings");

    unmount(component);
  });

  it("groups settings and switches the visible panel", async () => {
    settingsService.getApiV1Settings.mockResolvedValue({
      agent_dirs: {},
      chart_palette: "agentsview",
      github_configured: false,
      host: "127.0.0.1",
      port: 8080,
      read_only: false,
      require_auth: false,
      terminal: { mode: "auto" },
    });
    const component = mount(SettingsPage, {
      target: document.body,
    });
    await tick();
    await tick();

    const nav = document.body.querySelector('nav[aria-label="Settings"]');
    expect(nav).not.toBeNull();
    const searchStatus = document.body.querySelector('[role="status"]');
    expect(searchStatus).not.toBeNull();
    expect(searchStatus?.textContent?.trim()).toBe("");
    expect(nav!.textContent).toContain("Preferences");
    expect(nav!.textContent).toContain("Data");
    expect(nav!.textContent).toContain("Connections");
    expect(nav!.textContent).toContain("Session Providers");
    expect(nav!.textContent).not.toContain("Agent Directories");

    const visiblePanel = () =>
      document.body.querySelector<HTMLElement>(".settings-panel:not([hidden])");
    expect(visiblePanel()?.textContent).toContain("Appearance");

    const scroller = document.body.querySelector<HTMLElement>(".kit-settings__scroll")!;
    scroller.scrollTop = 240;

    const terminal = Array.from(nav!.querySelectorAll("button")).find((button) =>
      button.textContent?.includes("Terminal"),
    );
    expect(terminal).toBeTruthy();
    terminal!.click();
    await tick();

    expect(visiblePanel()?.textContent).toContain("Launch mode");
    expect(visiblePanel()?.textContent).not.toContain("Block visibility");
    expect(scroller.scrollTop).toBe(0);

    unmount(component);
  });

  it("filters settings by keywords and restores the selected panel", async () => {
    settingsService.getApiV1Settings.mockResolvedValue({
      agent_dirs: {},
      chart_palette: "agentsview",
      github_configured: false,
      host: "127.0.0.1",
      port: 8080,
      read_only: false,
      require_auth: false,
      terminal: { mode: "auto" },
    });
    const component = mount(SettingsPage, {
      target: document.body,
    });
    await tick();
    await tick();

    const nav = document.body.querySelector('nav[aria-label="Settings"]')!;
    const terminal = Array.from(nav.querySelectorAll("button")).find((button) =>
      button.textContent?.includes("Terminal"),
    )!;
    terminal.click();
    await tick();

    const search = document.body.querySelector<HTMLInputElement>(
      'input[type="search"][aria-label="Search settings"]',
    );
    expect(search).not.toBeNull();

    search!.value = "vectors";
    search!.dispatchEvent(new Event("input", { bubbles: true }));
    await tick();

    expect(nav.querySelectorAll("button")).toHaveLength(1);
    expect(nav.textContent).toContain("Embeddings");

    search!.value = "no such setting";
    search!.dispatchEvent(new Event("input", { bubbles: true }));
    await tick();

    expect(document.body.textContent).toContain("No matching settings");
    expect(
      document.body.querySelector(".settings-page")?.classList.contains("settings-no-results"),
    ).toBe(true);

    settings.loading = true;
    await tick();
    settings.loading = false;
    settings.loaded = true;
    await tick();

    expect(
      document.body.querySelector(".settings-page")?.classList.contains("settings-no-results"),
    ).toBe(true);
    expect(document.body.querySelector(".kit-settings__panel")).not.toBeNull();

    const restoredSearch = document.body.querySelector<HTMLInputElement>(
      'input[type="search"][aria-label="Search settings"]',
    )!;
    const restoredNav = document.body.querySelector('nav[aria-label="Settings"]')!;
    restoredSearch.value = "";
    restoredSearch.dispatchEvent(new Event("input", { bubbles: true }));
    await tick();

    expect(restoredNav.querySelectorAll("button")).toHaveLength(9);
    expect(
      document.body.querySelector(".settings-page")?.classList.contains("settings-no-results"),
    ).toBe(false);
    expect(restoredNav.querySelector('[aria-current="true"]')?.textContent).toContain("Terminal");

    unmount(component);
  });

  it("preserves a terminal draft while switching panels", async () => {
    settingsService.getApiV1Settings.mockResolvedValue({
      agent_dirs: {},
      chart_palette: "agentsview",
      github_configured: false,
      host: "127.0.0.1",
      port: 8080,
      read_only: false,
      require_auth: false,
      terminal: { mode: "auto" },
    });
    const component = mount(SettingsPage, {
      target: document.body,
    });
    await tick();
    await tick();

    const nav = document.body.querySelector('nav[aria-label="Settings"]')!;
    const terminal = Array.from(nav.querySelectorAll("button")).find((button) =>
      button.textContent?.includes("Terminal"),
    )!;
    terminal.click();
    await tick();

    const customMode = Array.from(
      document.body.querySelectorAll<HTMLElement>('[role="radio"]'),
    ).find((control) => control.textContent?.includes("Custom"));
    expect(customMode).toBeTruthy();
    customMode!.click();
    await tick();

    const binary = document.body.querySelector<HTMLInputElement>("#terminal-bin")!;
    binary.value = "/usr/bin/kitty";
    binary.dispatchEvent(new Event("input", { bubbles: true }));
    await tick();

    const search = document.body.querySelector<HTMLInputElement>(
      'input[type="search"][aria-label="Search settings"]',
    )!;
    search.value = "no such setting";
    search.dispatchEvent(new Event("input", { bubbles: true }));
    await tick();
    expect(document.body.textContent).toContain("No matching settings");
    expect(
      document.body.querySelector(".settings-page")?.classList.contains("settings-no-results"),
    ).toBe(true);
    expect(document.body.querySelector("#terminal-bin")).toBe(binary);

    search.value = "";
    search.dispatchEvent(new Event("input", { bubbles: true }));
    await tick();

    const appearance = Array.from(nav.querySelectorAll("button")).find((button) =>
      button.textContent?.includes("Appearance"),
    )!;
    appearance.click();
    await tick();
    terminal.click();
    await tick();

    expect(document.body.querySelector<HTMLInputElement>("#terminal-bin")?.value).toBe(
      "/usr/bin/kitty",
    );

    unmount(component);
  });

  it("searches localized keywords and localizes the clear control", async () => {
    setLocale("zh-CN");
    settingsService.getApiV1Settings.mockResolvedValue({
      agent_dirs: {},
      chart_palette: "agentsview",
      github_configured: false,
      host: "127.0.0.1",
      port: 8080,
      read_only: false,
      require_auth: false,
      terminal: { mode: "auto" },
    });
    const component = mount(SettingsPage, {
      target: document.body,
    });
    await tick();
    await tick();

    const search = document.body.querySelector<HTMLInputElement>(
      'input[type="search"][aria-label="搜索设置"]',
    )!;
    search.value = "向量";
    search.dispatchEvent(new Event("input", { bubbles: true }));
    await tick();

    const nav = document.body.querySelector('nav[aria-label="设置"]')!;
    expect(nav.querySelectorAll("button")).toHaveLength(1);
    expect(nav.textContent).toContain("语义嵌入");
    expect(document.body.querySelector('button[aria-label="清除搜索"]')).not.toBeNull();

    search.value = "vectors";
    search.dispatchEvent(new Event("input", { bubbles: true }));
    await tick();

    expect(nav.querySelectorAll("button")).toHaveLength(1);
    expect(nav.textContent).toContain("语义嵌入");
    expect(document.body.querySelector('[role="status"]')?.textContent).toContain("显示：语义嵌入");

    unmount(component);
  });

  it("matches localized search terms without requiring accents", async () => {
    setLocale("fr");
    settingsService.getApiV1Settings.mockResolvedValue({
      agent_dirs: {},
      chart_palette: "agentsview",
      github_configured: false,
      host: "127.0.0.1",
      port: 8080,
      read_only: false,
      require_auth: false,
      terminal: { mode: "auto" },
    });
    const component = mount(SettingsPage, {
      target: document.body,
    });
    await tick();
    await tick();

    const search = document.body.querySelector<HTMLInputElement>(
      'input[type="search"][aria-label="Rechercher dans les paramètres"]',
    )!;
    search.value = "parametres";
    search.dispatchEvent(new Event("input", { bubbles: true }));
    await tick();

    const nav = document.body.querySelector('nav[aria-label="Paramètres"]')!;
    expect(nav.querySelectorAll("button")).toHaveLength(1);
    expect(nav.textContent).toContain("Langue");

    unmount(component);
  });

  it("shows a danger flash when a settings save fails", async () => {
    settingsService.getApiV1Settings.mockResolvedValue({
      agent_dirs: {},
      chart_palette: "agentsview",
      github_configured: false,
      host: "127.0.0.1",
      port: 8080,
      read_only: false,
      require_auth: false,
      terminal: { mode: "auto" },
    });
    settingsService.putApiV1Settings.mockRejectedValue(
      new Error("settings endpoint unavailable"),
    );
    const component = mount(SettingsPage, { target: document.body });
    await tick();
    await tick();

    const matplotlib = Array.from(
      document.body.querySelectorAll<HTMLElement>('[role="radio"]'),
    ).find((control) => control.textContent?.includes("Matplotlib"));
    expect(matplotlib).toBeTruthy();
    matplotlib!.click();

    await vi.waitFor(() => {
      const flash = document.body.querySelector<HTMLElement>(
        '.kit-flash-banner[data-kit-tone="danger"]',
      );
      expect(flash).not.toBeNull();
      expect(flash?.textContent).toContain("settings endpoint unavailable");
    });

    await unmount(component);
  });
});
