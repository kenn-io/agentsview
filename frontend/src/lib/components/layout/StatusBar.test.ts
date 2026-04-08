// @vitest-environment jsdom
import {
  afterEach,
  beforeEach,
  describe,
  expect,
  it,
  vi,
} from "vitest";
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
  });

  afterEach(() => {
    document.body.innerHTML = "";
    vi.useRealTimers();
    sync.lastSync = null;
    sync.stats = null;
    sync.serverVersion = null;
    sync.versionMismatch = false;
    sync.progress = null;
    sync.syncing = false;
  });

  it("refreshes the sync label as time passes", async () => {
    const component = mount(StatusBar, {
      target: document.body,
    });

    await tick();
    const syncLabel = document.querySelector(
      ".status-right span:last-of-type",
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
});
