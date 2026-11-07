// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vite-plus/test";
import { mount, tick, unmount } from "svelte";
import EmptyArchivePanel from "./EmptyArchivePanel.svelte";
import SidebarToggleButton from "../layout/SidebarToggleButton.svelte";
import { initI18n } from "../../i18n/index.js";
import { router } from "../../stores/router.svelte.js";
import { settings } from "../../stores/settings.svelte.js";
import { ui } from "../../stores/ui.svelte.js";

let component: ReturnType<typeof mount>;

beforeEach(() => {
  localStorage.clear();
  initI18n();
  settings.loaded = true;
});

afterEach(() => {
  unmount(component);
  document.body.innerHTML = "";
  settings.loaded = false;
  settings.error = null;
  settings.sessionProviders = [];
  settings.disabledAgents = [];
  ui.sidebarOpen = true;
  ui.isMobileViewport = false;
  router.route = "sessions";
  router.params = {};
  window.history.replaceState(null, "", "/");
});

describe("EmptyArchivePanel", () => {
  it("restores the desktop sidebar and transfers focus between toggles", async () => {
    ui.sidebarOpen = true;
    ui.isMobileViewport = false;
    component = mount(EmptyArchivePanel, { target: document.body });
    const sidebarToggle = mount(SidebarToggleButton, {
      target: document.body,
      props: { placement: "sidebar" },
    });
    try {
      const close = document.querySelector<HTMLButtonElement>(".sidebar-panel-control--sidebar")!;
      close.focus();
      close.click();
      await tick();
      expect(ui.sidebarOpen).toBe(false);
      const open = document.querySelector<HTMLButtonElement>(
        ".empty-archive-panel .sidebar-panel-control--content",
      );
      expect(open).not.toBeNull();
      await vi.waitFor(() => expect(document.activeElement).toBe(open));
      open!.click();
      await tick();
      expect(ui.sidebarOpen).toBe(true);
      await vi.waitFor(() => expect(document.activeElement).toBe(close));
    } finally {
      unmount(sidebarToggle);
    }
  });

  it("shows enabled provider folders and opens Session Providers", async () => {
    settings.sessionProviders = [
      {
        id: "claude",
        display_name: "Claude Code",
        dirs: ["/home/example/.claude/projects"],
        homes: [],
        homes_supported: false,
      },
      {
        id: "gemini",
        display_name: "Gemini CLI",
        dirs: ["/home/example/.gemini/tmp"],
        homes: [],
        homes_supported: false,
      },
    ];
    settings.disabledAgents = ["gemini"];
    component = mount(EmptyArchivePanel, { target: document.body });
    await tick();

    const rows = [...document.querySelectorAll("li")].map((row) =>
      row.textContent?.replace(/\s+/g, " ").trim(),
    );
    expect(rows).toEqual(["Claude Code: /home/example/.claude/projects"]);
    const button = document.querySelector<HTMLButtonElement>(".empty-archive-choose")!;
    expect(button.textContent?.trim()).toBe("Choose folders");
    button.click();
    await tick();
    expect(router.route).toBe("settings");
    expect(router.params.panel).toBe("agent-directories");
  });
});
