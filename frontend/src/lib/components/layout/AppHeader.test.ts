// @vitest-environment jsdom
import {
  describe,
  it,
  expect,
  vi,
  beforeEach,
  afterEach,
} from "vite-plus/test";
import { mount, tick, unmount } from "svelte";
const mocks = vi.hoisted(() => ({
  downloadExport: vi.fn().mockResolvedValue(undefined),
  getMarkdownExportUrl: vi
    .fn()
    .mockReturnValue("/api/v1/sessions/sess-123/md"),
  copyToClipboard: vi.fn().mockResolvedValue(true),
}));

vi.mock("../../api/client.js", () => ({
  downloadExport: mocks.downloadExport,
  getMarkdownExportUrl: mocks.getMarkdownExportUrl,
}));

vi.mock("../../utils/clipboard.js", () => ({
  copyToClipboard: mocks.copyToClipboard,
}));

import { sessions } from "../../stores/sessions.svelte.js";
import { sync } from "../../stores/sync.svelte.js";
import { ui } from "../../stores/ui.svelte.js";
import { setLocale } from "../../i18n/index.js";
import type { Session } from "../../api/types.js";

// @ts-ignore
import AppHeader from "./AppHeader.svelte";

function testSession(overrides: Partial<Session> = {}): Session {
  return {
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

describe("AppHeader export actions", () => {
  let component: ReturnType<typeof mount> | undefined;

  beforeEach(() => {
    vi.clearAllMocks();
    sessions.activeSessionId = "sess-123";
    sessions.sessions = [testSession()];
    sync.serverVersion = null;
    ui.isMobileViewport = false;
    ui.followLatest = false;
    setLocale("en");
  });

  afterEach(() => {
    if (component) {
      unmount(component);
      component = undefined;
    }
    document.body.innerHTML = "";
  });

  it("copies markdown export link from export menu", async () => {
    component = mount(AppHeader, { target: document.body });
    await tick();

    const exportButton = document.querySelector<HTMLButtonElement>(
      'button[aria-label="Export session"]',
    );
    expect(exportButton).not.toBeNull();

    exportButton!.click();
    await tick();

    const copyButton = Array.from(
      document.querySelectorAll<HTMLButtonElement>("button"),
    ).find((button) =>
      button.textContent?.includes("Copy markdown export link"),
    );
    expect(copyButton).not.toBeNull();

    copyButton!.click();
    await tick();

    expect(mocks.getMarkdownExportUrl).toHaveBeenCalledWith("sess-123");
    expect(mocks.copyToClipboard).toHaveBeenCalledWith(
      "http://localhost:3000/api/v1/sessions/sess-123/md",
    );
  });

  it("copies active session source path from export menu", async () => {
    sessions.sessions = [
      testSession({
        file_path: "/tmp/agentsview/sessions/session-123.jsonl",
      }),
    ];

    component = mount(AppHeader, { target: document.body });
    await tick();

    const exportButton = document.querySelector<HTMLButtonElement>(
      'button[aria-label="Export session"]',
    );
    expect(exportButton).not.toBeNull();

    exportButton!.click();
    await tick();

    const copyPathButton = Array.from(
      document.querySelectorAll<HTMLButtonElement>("button"),
    ).find((button) =>
      button.textContent?.includes("Copy source file path"),
    );
    expect(copyPathButton).toBeDefined();

    copyPathButton!.click();
    await tick();

    expect(mocks.copyToClipboard).toHaveBeenCalledWith(
      "/tmp/agentsview/sessions/session-123.jsonl",
    );
  });

  it("toggles follow latest from the session header", async () => {
    component = mount(AppHeader, { target: document.body });
    await tick();

    const followButton = document.querySelector<HTMLButtonElement>(
      'button[aria-label="Follow latest messages"]',
    );
    expect(followButton).not.toBeNull();
    expect(followButton!.classList.contains("active")).toBe(false);

    followButton!.click();
    await tick();

    expect(ui.followLatest).toBe(true);
    expect(followButton!.classList.contains("active")).toBe(true);

    followButton!.click();
    await tick();

    expect(ui.followLatest).toBe(false);
    expect(followButton!.classList.contains("active")).toBe(false);
  });

  it("labels compact title-bar actions with hover hints", async () => {
    component = mount(AppHeader, { target: document.body });
    await tick();

    const sidebarButton = document.querySelector<HTMLButtonElement>(
      'button[aria-label="Toggle sidebar"]',
    );
    const shortcutsButton = document.querySelector<HTMLButtonElement>(
      'button[aria-label="Keyboard shortcuts"]',
    );

    expect(sidebarButton).not.toBeNull();
    expect(sidebarButton?.title).toBe("Toggle sidebar (b)");
    expect(shortcutsButton).not.toBeNull();
    expect(shortcutsButton?.title).toBe("Keyboard shortcuts (?)");
  });

  it("renders every route as a primary-nav tab", async () => {
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
      "Pinned",
      "Insights",
      "Trash",
    ]) {
      expect(labels).toContain(expected);
    }
  });

  it("distinguishes global sync from page refresh controls", async () => {
    component = mount(AppHeader, { target: document.body });
    await tick();

    const syncButton = document.querySelector<HTMLButtonElement>(
      'button[aria-label="Sync sessions"]',
    );

    expect(syncButton).not.toBeNull();
    expect(syncButton?.textContent?.trim()).toBe("Sync");
    expect(
      syncButton?.querySelector("svg.lucide-database-backup"),
    ).not.toBeNull();
  });

  it("labels read-only global refresh with the refresh action", async () => {
    sync.serverVersion = {
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
    expect(
      refreshButton?.querySelector("svg.lucide-database-backup"),
    ).not.toBeNull();
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
