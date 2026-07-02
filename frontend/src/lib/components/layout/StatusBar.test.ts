// @vitest-environment jsdom
import {
  afterEach,
  beforeEach,
  describe,
  expect,
  it,
  vi,
} from "vite-plus/test";
import { mount, tick, unmount } from "svelte";
// @ts-ignore
import StatusBar from "./StatusBar.svelte";
import { sync } from "../../stores/sync.svelte.js";

describe("StatusBar", () => {
  beforeEach(() => {
    vi.useFakeTimers();
    vi.setSystemTime(new Date("2026-04-08T05:00:00Z"));
    sync.syncing = false;
    sync.progress = null;
    sync.lastSync = "2026-04-08T05:00:00Z";
    sync.stats = null;
    sync.serverVersion = null;
    sync.versionMismatch = false;
    sync.remoteUnreachable = false;
    sync.backendDegraded = false;
    sync.backendDegradedMessage = null;
  });

  afterEach(() => {
    document.body.innerHTML = "";
    vi.useRealTimers();
    sync.lastSync = null;
    sync.stats = null;
    sync.serverVersion = null;
    sync.versionMismatch = false;
    sync.remoteUnreachable = false;
    sync.backendDegraded = false;
    sync.backendDegradedMessage = null;
    sync.progress = null;
    sync.syncing = false;
  });

  it("refreshes the sync label as time passes", async () => {
    const component = mount(StatusBar, {
      target: document.body,
    });

    await tick();
    const syncLabel = document.querySelector(
      ".kit-status-bar__section--right span[title]",
    );
    const expectedTitle = new Date(sync.lastSync!).toLocaleString(
      undefined,
      {
        month: "short",
        day: "numeric",
        hour: "2-digit",
        minute: "2-digit",
      },
    );

    expect(document.body.textContent).toContain(
      "synced just now",
    );
    expect(syncLabel?.getAttribute("title")).toBe(expectedTitle);

    await vi.advanceTimersByTimeAsync(70_000);
    await tick();

    expect(document.body.textContent).toContain(
      "synced 1m ago",
    );

    unmount(component);
  });

  it("shows a remote-unreachable indicator only when flagged", async () => {
    sync.remoteUnreachable = true;
    const component = mount(StatusBar, {
      target: document.body,
    });
    await tick();
    expect(document.body.textContent).toContain(
      "remote server unreachable",
    );

    sync.remoteUnreachable = false;
    await tick();
    expect(document.body.textContent).not.toContain(
      "remote server unreachable",
    );

    unmount(component);
  });

  it("shows a sync-not-ready indicator when backend is degraded", async () => {
    sync.backendDegraded = true;
    sync.backendDegradedMessage = "sync not ready";
    const component = mount(StatusBar, {
      target: document.body,
    });
    await tick();

    expect(document.body.textContent).toContain("sync not ready");
    expect(
      document.querySelector(".backend-warn")?.getAttribute("title"),
    ).toBe("sync not ready");

    sync.backendDegraded = false;
    await tick();
    expect(document.body.textContent).not.toContain("sync not ready");

    unmount(component);
  });

  it("renders detailed sync progress with a hint", async () => {
    sync.syncing = true;
    sync.progress = {
      phase: "rebuilding_search",
      detail: "Rebuilding search index",
      hint: "Rebuilding the search index may take a while on large archives.",
      resync: true,
      projects_total: 0,
      projects_done: 0,
      sessions_total: 0,
      sessions_done: 0,
      messages_indexed: 0,
    };

    const component = mount(StatusBar, {
      target: document.body,
    });
    await tick();

    const progress = document.querySelector(".sync-progress");
    expect(progress?.textContent).toContain("Rebuilding search index");
    expect(progress?.getAttribute("title")).toContain("may take a while");

    unmount(component);
  });
});
