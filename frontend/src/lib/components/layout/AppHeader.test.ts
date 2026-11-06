// @vitest-environment jsdom
import { describe, it, expect, vi, beforeEach, afterEach, onTestFinished } from "vite-plus/test";
import { mount, tick, unmount } from "svelte";
const mocks = vi.hoisted(() => ({
  downloadExport: vi.fn().mockResolvedValue(undefined),
  getMarkdownExportUrl: vi.fn().mockReturnValue("/api/v1/sessions/sess-123/md"),
  copyToClipboard: vi.fn().mockResolvedValue(true),
  reportTelemetry: vi.fn(),
  syncClaudeAI: vi.fn(),
}));

vi.mock("../../utils/telemetry.js", () => ({ reportTelemetry: mocks.reportTelemetry }));

vi.mock("../../api/client.js", () => ({
  downloadExport: mocks.downloadExport,
  getMarkdownExportUrl: mocks.getMarkdownExportUrl,
  syncClaudeAI: mocks.syncClaudeAI,
}));

vi.mock("../../utils/clipboard.js", () => ({
  copyToClipboard: mocks.copyToClipboard,
}));

import { sessions } from "../../stores/sessions.svelte.js";
import { sync } from "../../stores/sync.svelte.js";
import { settings } from "../../stores/settings.svelte.js";
import { ui } from "../../stores/ui.svelte.js";
import { router } from "../../stores/router.svelte.js";
import { claudeAISync } from "../../stores/claudeAISync.svelte.js";
import { setLocale } from "../../i18n/index.js";
import type { Session } from "../../api/types.js";

// @ts-ignore
import AppHeader from "./AppHeader.svelte";

function testSession(overrides: Partial<Session> = {}): Session {
  return {
    compaction_count: 0,
    consecutive_failure_max: 0,
    edit_churn_count: 0,
    ended_with_role: "",
    final_failure_streak: 0,
    has_peak_context_tokens: false,
    has_total_output_tokens: false,
    mid_task_compaction_count: 0,
    outcome: "",
    outcome_confidence: "",
    secret_leak_count: 0,
    tool_failure_signal_count: 0,
    tool_retry_count: 0,
    id: "sess-123",
    project: "agentsview",
    machine: "test-machine",
    agent: "codex",
    first_message: "Synthetic test session",
    started_at: "2026-06-13T12:00:00Z",
    ended_at: "2026-06-13T12:05:00Z",
    message_count: 2,
    user_message_count: 1,
    total_output_tokens: 0,
    peak_context_tokens: 0,
    is_automated: false,
    created_at: "2026-06-13T12:00:00Z",
    ...overrides,
  };
}

async function openMoreActions(): Promise<void> {
  document.querySelector<HTMLButtonElement>('button[aria-label="More actions"]')!.click();
  await tick();
}

function menuItem(text: string): HTMLButtonElement | undefined {
  return Array.from(document.querySelectorAll<HTMLButtonElement>('[role="menuitem"]')).find(
    (item) => item.textContent?.includes(text),
  );
}

// jsdom has no layout, so TopBar's collapse measurement needs widths: a
// tab row narrower than the bar keeps the tabs, a wider one collapses them.
function stubTopBarWidths(tabRow: number, bar: number): () => void {
  const offsetWidth = vi
    .spyOn(HTMLElement.prototype, "offsetWidth", "get")
    .mockImplementation(function (this: HTMLElement) {
      return this.classList.contains("kit-top-bar__probe") ? tabRow : 0;
    });
  const clientWidth = vi
    .spyOn(HTMLElement.prototype, "clientWidth", "get")
    .mockImplementation(function (this: HTMLElement) {
      return this.classList.contains("kit-top-bar") ? bar : 0;
    });
  return () => {
    offsetWidth.mockRestore();
    clientWidth.mockRestore();
  };
}

const currentTabs = () =>
  Array.from(document.querySelectorAll('[aria-current="page"]'), (element) =>
    element.textContent?.trim(),
  );

describe("AppHeader export actions", () => {
  let component: ReturnType<typeof mount> | undefined;
  let restoreWidths: (() => void) | undefined;

  beforeEach(() => {
    vi.clearAllMocks();
    sessions.activeSessionId = "sess-123";
    sessions.sessions = [testSession()];
    sync.serverVersion = null;
    settings.loaded = true;
    settings.readOnly = false;
    settings.error = null;
    ui.isMobileViewport = false;
    ui.sidebarOpen = true;
    ui.followLatest = false;
    router.route = "sessions";
    setLocale("en");
  });

  afterEach(() => {
    if (component) {
      unmount(component);
      component = undefined;
    }
    restoreWidths?.();
    restoreWidths = undefined;
    document.body.innerHTML = "";
    ui.isMobileViewport = false;
    ui.sidebarOpen = true;
    router.route = "sessions";
    settings.loaded = false;
    settings.readOnly = false;
    settings.error = null;
  });

  it.each([
    { archive_only: false, read_only: false, importLabel: "Import conversations", importDisabled: false, syncLabel: "Sync sessions" },
    { archive_only: false, read_only: true, importLabel: "Import unavailable in read-only mode", importDisabled: true, syncLabel: "Refresh data" },
    { archive_only: true, read_only: false, importLabel: "Import unavailable in archive-only mode", importDisabled: true, syncLabel: null },
  ])(
    "offers the available actions for archive_only=$archive_only read_only=$read_only",
    async ({ archive_only, read_only, importLabel, importDisabled, syncLabel }) => {
      sync.serverVersion = { archive_only, read_only } as NonNullable<typeof sync.serverVersion>;
      component = mount(AppHeader, { target: document.body });
      await tick();

      const syncButton = document.querySelector<HTMLButtonElement>(".sync-btn");
      if (syncLabel) expect(syncButton?.getAttribute("aria-label")).toBe(syncLabel);
      else expect(syncButton).toBeNull();

      await openMoreActions();
      expect(menuItem("Download HTML export")).toBeDefined();
      const importItem = menuItem(importLabel)!;
      expect(importItem).toBeDefined();
      expect(importItem.getAttribute("aria-disabled") === "true").toBe(importDisabled);
      importItem.click();
      await tick();
      expect(document.querySelector('[role="dialog"]') !== null).toBe(!importDisabled);
    },
  );

  it("shows a visible Import button only while Claude.ai Sync runs", async () => {
    mocks.syncClaudeAI.mockReturnValue(new Promise(() => {}));
    component = mount(AppHeader, { target: document.body });
    await tick();
    expect(document.querySelector(".import-btn")).toBeNull();
    void claudeAISync.start(undefined, () => {});
    await tick();
    const importButton = document.querySelector<HTMLButtonElement>(".import-btn")!;
    expect(importButton.getAttribute("aria-label")).toBe(
      "Claude.ai Sync is running. Open Import to see progress or stop it.",
    );
    expect(importButton.querySelector(".sync-spinner")).not.toBeNull();
    claudeAISync.stop();
    await tick();
    expect(document.querySelector(".import-btn")).toBeNull();
  });

  it("leaves the transcript controls to the session header on wide screens", async () => {
    component = mount(AppHeader, { target: document.body });
    await tick();
    expect(document.querySelector(".view-trigger")).toBeNull();
    expect(document.querySelector('button[aria-label="Follow latest messages"]')).toBeNull();
  });

  it("copies markdown export link from the actions menu", async () => {
    component = mount(AppHeader, { target: document.body });
    await tick();

    await openMoreActions();
    menuItem("Copy markdown export link")!.click();
    await tick();

    expect(mocks.getMarkdownExportUrl).toHaveBeenCalledWith("sess-123");
    expect(mocks.copyToClipboard).toHaveBeenCalledWith(
      "http://localhost:3000/api/v1/sessions/sess-123/md",
    );
    await vi.waitFor(() =>
      expect(mocks.reportTelemetry).toHaveBeenCalledWith("export_run", { format: "markdown_link" }),
    );
  });

  it("copies active session source path from the actions menu", async () => {
    sessions.sessions = [
      testSession({
        file_path: "/tmp/agentsview/sessions/session-123.jsonl",
      }),
    ];

    component = mount(AppHeader, { target: document.body });
    await tick();

    await openMoreActions();
    menuItem("Copy source file path")!.click();
    await tick();

    expect(mocks.copyToClipboard).toHaveBeenCalledWith(
      "/tmp/agentsview/sessions/session-123.jsonl",
    );
  });

  it("shows the transcript controls in the top bar on narrow screens", async () => {
    ui.isMobileViewport = true;
    component = mount(AppHeader, { target: document.body });
    await tick();

    const followButton = document.querySelector<HTMLButtonElement>(
      'button[aria-label="Follow latest messages"]',
    )!;
    expect(followButton.getAttribute("aria-pressed")).toBe("false");

    followButton.click();
    await tick();
    expect(ui.followLatest).toBe(true);
    expect(followButton.getAttribute("aria-pressed")).toBe("true");

    followButton.click();
    await tick();
    expect(ui.followLatest).toBe(false);
    expect(followButton.getAttribute("aria-pressed")).toBe("false");
  });

  it("keeps the sidebar toggle out of the desktop title bar", async () => {
    component = mount(AppHeader, { target: document.body });
    await tick();

    const sidebarButton = document.querySelector<HTMLButtonElement>(
      'button[aria-label="Toggle sidebar"]',
    );
    const shortcutsButton = document.querySelector<HTMLButtonElement>(
      'button[aria-label="Keyboard shortcuts"]',
    );

    expect(sidebarButton).toBeNull();
    expect(shortcutsButton).not.toBeNull();
    expect(shortcutsButton?.title).toBe("Keyboard shortcuts (?)");
  });

  it("keeps the mobile hamburger and its drawer behavior", async () => {
    ui.isMobileViewport = true;
    ui.sidebarOpen = true;

    component = mount(AppHeader, { target: document.body });
    await tick();

    const sidebarButton = document.querySelector<HTMLButtonElement>(
      'button[aria-label="Toggle sidebar"]',
    );
    expect(sidebarButton).not.toBeNull();
    expect(sidebarButton?.title).toBe("Toggle sidebar (b)");

    sidebarButton!.click();
    await tick();

    expect(ui.sidebarOpen).toBe(false);
  });

  it("opens the sessions drawer from another mobile route", async () => {
    ui.isMobileViewport = true;
    ui.sidebarOpen = false;
    router.route = "usage";

    component = mount(AppHeader, { target: document.body });
    await tick();

    const sidebarButton = document.querySelector<HTMLButtonElement>(
      'button[aria-label="Toggle sidebar"]',
    );

    expect(sidebarButton).not.toBeNull();
    expect(sidebarButton?.getAttribute("aria-controls")).toBe("session-sidebar");
    expect(sidebarButton?.getAttribute("aria-expanded")).toBe("false");

    sidebarButton!.click();
    await tick();

    expect(router.route).toBe("sessions");
    expect(ui.sidebarOpen).toBe(true);
  });

  it("renders every route as a primary-nav tab", async () => {
    sync.serverVersion = {
      api_version: 1,
      data_version: 1,
      insight_generation_available: false,
      friction_available: true,
      friction_build_allowed: true,
      friction_readable: true,
      session_stats_available: false,
      kata_available: false,
      version: "dev",
      commit: "unknown",
      build_date: "",
      read_only: false,
    };
    component = mount(AppHeader, { target: document.body });
    await tick();

    const nav = document.querySelector('nav[aria-label="Primary navigation"]');
    expect(nav).not.toBeNull();
    // jsdom measures every width as 0, so TopBar renders the collapsed
    // dropdown; the measurement probe always carries the full tab row.
    const labels = Array.from(
      nav!.querySelectorAll<HTMLElement>(".kit-top-bar__probe .kit-top-bar__tab"),
    ).map((b) => b.textContent?.trim());
    for (const expected of [
      "Sessions",
      "Usage",
      "Activity",
      "Trends",
      "Recall",
      "Pinned",
      "Quality",
      "Friction Log",
      "Trash",
      "Recent Edits",
      "Data",
    ]) {
      expect(labels).toContain(expected);
    }
    expect(labels).not.toContain("Token Usage");
  });

  it("shows the Friction Log tab on a server that reads digests but cannot build them", async () => {
    sync.serverVersion = {
      api_version: 1,
      data_version: 1,
      insight_generation_available: false,
      friction_available: false,
      friction_build_allowed: false,
      friction_readable: true,
      session_stats_available: false,
      version: "dev",
      commit: "unknown",
      build_date: "",
      read_only: true,
    } as never;
    component = mount(AppHeader, { target: document.body });
    await tick();
    const labels = Array.from(
      document.querySelectorAll<HTMLElement>(".kit-top-bar__probe .kit-top-bar__tab"),
    ).map((b) => b.textContent?.trim());
    expect(labels).toContain("Friction Log");
  });

  it("hides the Friction Log tab when the server does not store digests", async () => {
    sync.serverVersion = {
      api_version: 1,
      data_version: 1,
      insight_generation_available: false,
      friction_available: false,
      friction_build_allowed: false,
      session_stats_available: false,
      kata_available: false,
      version: "dev",
      commit: "unknown",
      build_date: "",
      read_only: true,
    };
    component = mount(AppHeader, { target: document.body });
    await tick();

    const labels = Array.from(
      document.querySelectorAll<HTMLElement>(".kit-top-bar__probe .kit-top-bar__tab"),
    ).map((b) => b.textContent?.trim());
    expect(labels).toContain("Quality");
    expect(labels).not.toContain("Friction Log");
  });

  for (const readable of [false, true]) {
    it(`marks ${readable ? "the Friction tab" : "no tab"} current on /friction when its tab is ${readable ? "shown" : "hidden"}`, async () => {
      sync.serverVersion = {
        api_version: 1,
        data_version: 1,
        insight_generation_available: false,
        friction_available: false,
        friction_build_allowed: false,
        friction_readable: readable,
        session_stats_available: false,
        kata_available: false,
        version: "dev",
        commit: "unknown",
        build_date: "",
        read_only: true,
      };
      router.route = "friction";
      restoreWidths = stubTopBarWidths(600, 2000);
      component = mount(AppHeader, { target: document.body });
      await tick();

      const bar = document.querySelector(".kit-top-bar")!;
      expect(bar.classList.contains("no-current-tab")).toBe(!readable);
      expect(document.querySelector(".kit-top-bar__tabs")).not.toBeNull();
      expect(currentTabs()).toEqual(readable ? ["Friction Log"] : []);
    });
  }

  it("marks no tab current on Settings and the routed tab again after it", async () => {
    router.route = "settings";
    restoreWidths = stubTopBarWidths(600, 2000);
    component = mount(AppHeader, { target: document.body });
    await tick();

    expect(document.querySelector(".kit-top-bar__tabs")).not.toBeNull();
    expect(currentTabs()).toEqual([]);

    // Sessions is the tab the bar falls back to, so returning to it is the
    // case where nothing re-renders the attribute by itself.
    router.route = "sessions";
    await tick();
    expect(currentTabs()).toEqual(["Sessions"]);

    router.route = "settings";
    await tick();
    expect(currentTabs()).toEqual([]);

    router.route = "usage";
    await tick();
    expect(currentTabs()).toEqual(["Usage"]);
  });

  it("shows a neutral collapsed menu on Settings and the routed tab after it", async () => {
    router.route = "settings";
    restoreWidths = stubTopBarWidths(2000, 600);
    component = mount(AppHeader, { target: document.body });
    await tick();

    const trigger = () =>
      document.querySelector<HTMLElement>(
        ".kit-top-bar__nav-select .kit-select-dropdown__trigger",
      )!;
    const shown = () => ({
      text: trigger().querySelector(".kit-select-dropdown__value")?.textContent,
      label: trigger().getAttribute("aria-label"),
    });
    expect(shown()).toEqual({ text: "—", label: "Primary navigation" });

    router.route = "sessions";
    await tick();
    expect(shown()).toEqual({ text: "Sessions", label: "Primary navigation: Sessions" });

    router.route = "settings";
    await tick();
    expect(shown()).toEqual({ text: "—", label: "Primary navigation" });

    router.route = "usage";
    await tick();
    expect(shown()).toEqual({ text: "Usage", label: "Primary navigation: Usage" });
  });

  it("selects no option in the open collapsed menu on Settings", async () => {
    // jsdom has no scrollIntoView; the menu scrolls its highlight into view.
    Element.prototype.scrollIntoView = vi.fn();
    onTestFinished(() => {
      delete (Element.prototype as Partial<Element>).scrollIntoView;
    });
    router.route = "settings";
    restoreWidths = stubTopBarWidths(2000, 600);
    component = mount(AppHeader, { target: document.body });
    await tick();

    const trigger = () =>
      document.querySelector<HTMLElement>(
        ".kit-top-bar__nav-select .kit-select-dropdown__trigger",
      )!;
    // Opens the menu, reads which options claim to be selected, closes it.
    const selectedOptions = async () => {
      trigger().click();
      await tick();
      const options = [...document.querySelectorAll('.kit-top-bar__nav-select [role="option"]')];
      expect(options.length).toBeGreaterThan(0);
      const names = options
        .filter((option) => option.getAttribute("aria-selected") === "true")
        .map((option) => option.textContent?.trim());
      trigger().click();
      await tick();
      expect(document.querySelector('.kit-top-bar__nav-select [role="option"]')).toBeNull();
      return names;
    };

    expect(await selectedOptions()).toEqual([]);

    router.route = "sessions";
    await tick();
    expect(await selectedOptions()).toEqual(["Sessions"]);

    router.route = "settings";
    await tick();
    expect(await selectedOptions()).toEqual([]);
  });

  it("distinguishes global sync from page refresh controls", async () => {
    component = mount(AppHeader, { target: document.body });
    await tick();

    const syncButton = document.querySelector<HTMLButtonElement>(
      'button[aria-label="Sync sessions"]',
    );

    expect(syncButton).not.toBeNull();
    expect(syncButton?.textContent?.trim()).toBe("Sync");
    expect(syncButton?.querySelector("svg.lucide-database-backup")).not.toBeNull();
  });

  it("labels read-only global refresh with the refresh action", async () => {
    sync.serverVersion = {
      api_version: 1,
      data_version: 1,
      session_stats_available: false,
      friction_available: false,
      friction_build_allowed: false,
      insight_generation_available: false,
      kata_available: false,
      version: "dev",
      commit: "unknown",
      build_date: "",
      read_only: true,
    };

    component = mount(AppHeader, { target: document.body });
    await tick();

    const refreshButton = document.querySelector<HTMLButtonElement>(
      'button[aria-label="Refresh data"]',
    );

    expect(refreshButton).not.toBeNull();
    expect(refreshButton?.textContent?.trim()).toBe("Refresh");
    // Same glyph as the page refresh controls: both mean "refresh data".
    expect(refreshButton?.querySelector("svg.lucide-refresh-cw")).not.toBeNull();
    expect(refreshButton?.querySelector("svg.lucide-database-backup")).toBeNull();
    expect(document.body.textContent).toContain("Recall");
  });

  it("keeps Recall available when settings report a read-only backend", async () => {
    sync.serverVersion = {
      api_version: 1,
      data_version: 1,
      session_stats_available: false,
      friction_available: false,
      friction_build_allowed: false,
      insight_generation_available: false,
      kata_available: false,
      version: "dev",
      commit: "unknown",
      build_date: "",
      read_only: false,
    };
    settings.readOnly = true;

    component = mount(AppHeader, { target: document.body });
    await tick();

    expect(document.body.textContent).toContain("Sessions");
    expect(document.body.textContent).toContain("Recall");
  });

  it("renders translated shell navigation when locale is Simplified Chinese", async () => {
    setLocale("zh-CN");

    component = mount(AppHeader, { target: document.body });
    await tick();

    expect(
      document.querySelector<HTMLButtonElement>('button[aria-label="同步会话"]'),
    ).not.toBeNull();
    expect(document.body.textContent).toContain("会话");
    expect(document.body.textContent).toContain("用量");
    expect(document.body.textContent).toContain("活动");
  });
});
