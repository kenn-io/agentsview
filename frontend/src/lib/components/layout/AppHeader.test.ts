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
import { ui } from "../../stores/ui.svelte.js";

// @ts-ignore
import AppHeader from "./AppHeader.svelte";

describe("AppHeader export actions", () => {
  let component: ReturnType<typeof mount> | undefined;

  beforeEach(() => {
    vi.clearAllMocks();
    sessions.activeSessionId = "sess-123";
    ui.isMobileViewport = false;
    ui.followLatest = false;
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

    const moreButton = document.querySelector<HTMLButtonElement>(
      'button[aria-label="More navigation"]',
    );
    const shortcutsButton = document.querySelector<HTMLButtonElement>(
      'button[aria-label="Keyboard shortcuts"]',
    );

    expect(moreButton).not.toBeNull();
    expect(moreButton?.title).toBe("More navigation");
    expect(shortcutsButton).not.toBeNull();
    expect(shortcutsButton?.title).toBe("Keyboard shortcuts (?)");
  });
});
