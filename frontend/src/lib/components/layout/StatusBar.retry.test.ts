// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vite-plus/test";
import { mount, tick, unmount } from "svelte";
// @ts-ignore
import StatusBar from "./StatusBar.svelte";
import { sync } from "../../stores/sync.svelte.js";

describe("StatusBar degraded backend retry", () => {
  beforeEach(() => {
    sync.backendDegraded = true;
    sync.backendDegradedMessage = "Session data unavailable. Click to retry.";
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
    const loadStats = vi.spyOn(sync, "loadStats").mockResolvedValue(undefined);
    const component = mount(StatusBar, {
      target: document.body,
    });
    await tick();

    document.querySelector<HTMLButtonElement>(".backend-warn")?.click();

    expect(loadStats).toHaveBeenCalledOnce();

    unmount(component);
  });
});

describe("StatusBar failed status reads", () => {
  afterEach(() => {
    document.body.innerHTML = "";
    vi.restoreAllMocks();
    sync.statusError = null;
    sync.lastSync = null;
  });

  it("reports a failed local status read and clears it after Retry succeeds", async () => {
    const { MetadataService, SyncService } = await import("../../api/generated/index");
    const status = vi
      .spyOn(SyncService, "getApiV1SyncStatus")
      .mockRejectedValueOnce(new TypeError("Failed to fetch"));
    vi.spyOn(MetadataService, "getApiV1Version").mockResolvedValue({
      version: "test",
      commit: "test",
    } as Awaited<ReturnType<typeof MetadataService.getApiV1Version>>);
    vi.spyOn(MetadataService, "getApiV1Stats").mockResolvedValue(
      {} as Awaited<ReturnType<typeof MetadataService.getApiV1Stats>>,
    );
    await sync.loadStatus();

    const component = mount(StatusBar, { target: document.body });
    await tick();
    const indicator = Array.from(document.querySelectorAll<HTMLButtonElement>("button")).find(
      (button) => button.textContent?.trim() === "status unavailable",
    );
    expect(indicator?.title).toBe("Failed to fetch");

    status.mockResolvedValueOnce({ last_sync: "2026-04-08T05:00:00Z" } as Awaited<
      ReturnType<typeof SyncService.getApiV1SyncStatus>
    >);
    indicator!.click();
    await vi.waitFor(() => expect(document.body.textContent).not.toContain("status unavailable"));
    expect(sync.statusError).toBeNull();

    unmount(component);
  });
});
