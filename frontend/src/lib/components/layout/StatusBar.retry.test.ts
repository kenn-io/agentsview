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

describe("StatusBar degraded backend retry", () => {
  beforeEach(() => {
    sync.backendDegraded = true;
    sync.backendDegradedMessage = "sync not ready";
    sync.lastSync = "2026-04-08T05:00:00Z";
    sync.remoteUnreachable = false;
  });

  afterEach(() => {
    document.body.innerHTML = "";
    vi.restoreAllMocks();
    sync.backendDegraded = false;
    sync.backendDegradedMessage = null;
    sync.lastSync = null;
    sync.remoteUnreachable = false;
  });

  it("retries stats when sync-not-ready indicator is clicked", async () => {
    const loadStats = vi
      .spyOn(sync, "loadStats")
      .mockResolvedValue(undefined);
    const component = mount(StatusBar, {
      target: document.body,
    });
    await tick();

    document.querySelector<HTMLButtonElement>(".backend-warn")?.click();

    expect(loadStats).toHaveBeenCalledOnce();

    unmount(component);
  });
});
