// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vite-plus/test";
import { mount, tick, unmount } from "svelte";
import { sync } from "../../stores/sync.svelte.js";
import { SyncService } from "../../api/generated/index.js";
// @ts-ignore
import ResyncModal from "./ResyncModal.svelte";

describe("ResyncModal", () => {
  let component: ReturnType<typeof mount> | undefined;

  beforeEach(() => {
    document.body.innerHTML = "";
    sync.progress = null;
    sync.serverVersion = null;
    vi.spyOn(sync, "triggerResync").mockReturnValue(true);
  });

  afterEach(() => {
    if (component) {
      unmount(component);
      component = undefined;
    }
    vi.restoreAllMocks();
    sync.progress = null;
    sync.serverVersion = null;
    document.body.innerHTML = "";
  });

  async function openProgress() {
    component = mount(ResyncModal, { target: document.body });
    await tick();
    const startButton = Array.from(document.querySelectorAll("button")).find((button) =>
      button.textContent?.includes("Start Full Resync"),
    );
    expect(startButton).toBeDefined();
    startButton!.click();
    await tick();
  }

  it("does not request resync in an archive-only database", async () => {
    sync.serverVersion = { archive_only: true } as NonNullable<typeof sync.serverVersion>;
    vi.mocked(sync.triggerResync).mockRestore();
    const request = vi.spyOn(SyncService, "postApiV1Resync");
    await openProgress();
    expect(request).not.toHaveBeenCalled();
    expect(document.querySelector(".progress-view")).toBeNull();
  });

  it("shows an uncounted finalization detail without a zero-value bar", async () => {
    sync.progress = {
      phase: "finalizing",
      detail: "Finalizing sync: checking database-backed sessions",
      resync: true,
      projects_total: 0,
      projects_done: 0,
      sessions_total: 0,
      sessions_done: 0,
      messages_indexed: 0,
    };

    await openProgress();

    expect(document.body.textContent).toContain(
      "Finalizing sync: checking database-backed sessions",
    );
    expect(document.body.textContent).not.toContain("Syncing 0 / 0 sessions");
    expect(document.querySelector(".progress-bar-track")).toBeNull();
  });

  it("keeps the counted session label and progress bar", async () => {
    sync.progress = {
      phase: "syncing",
      detail: "Syncing sessions into rebuilt database",
      resync: true,
      projects_total: 0,
      projects_done: 0,
      sessions_total: 10,
      sessions_done: 4,
      messages_indexed: 40,
    };

    await openProgress();

    expect(document.body.textContent).toContain("Syncing 4 / 10 sessions");
    expect(document.querySelector(".progress-bar-track")).not.toBeNull();
  });
});
