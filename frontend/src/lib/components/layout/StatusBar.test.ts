// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vite-plus/test";
import { mount, tick, unmount } from "svelte";
// @ts-ignore
import StatusBar from "./StatusBar.svelte";
import { sync } from "../../stores/sync.svelte.js";
import { formatTimestamp } from "../../utils/format.js";
import { ApiError } from "../../api/runtime.js";

const { getCodexCloudStatus } = vi.hoisted(() => ({
  getCodexCloudStatus: vi.fn(),
}));

vi.mock("../../api/generated/index", async (importOriginal) => ({
  ...(await importOriginal<typeof import("../../api/generated/index")>()),
  CodexCloudService: { getApiV1CodexCloudStatus: getCodexCloudStatus },
}));

describe("StatusBar", () => {
  beforeEach(() => {
    getCodexCloudStatus.mockReset().mockResolvedValue([]);
    vi.useFakeTimers();
    vi.setSystemTime(new Date("2026-04-08T05:00:00Z"));
    sync.archiveStale = false;
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
    sync.archiveStale = false;
  });

  it("shows stale results guidance until the archive is current", async () => {
    sync.archiveStale = true;
    const component = mount(StatusBar, { target: document.body });
    await tick();
    expect(document.body.textContent).toContain("Archive results may be stale.");
    expect(document.body.textContent).not.toContain("Changes are paused");
    sync.syncing = true;
    sync.progress = {
      phase: "preparing_resync",
      resync: true,
      projects_total: 0,
      projects_done: 0,
      sessions_total: 0,
      sessions_done: 0,
      messages_indexed: 0,
    };
    await tick();
    expect(document.body.textContent).toContain(
      "Archive results may be stale. Changes are paused until the rebuild finishes.",
    );
    sync.syncing = false;
    sync.progress = null;
    await tick();
    expect(document.body.textContent).toContain("Archive results may be stale.");
    expect(document.body.textContent).not.toContain("Changes are paused");
    sync.archiveStale = false;
    await tick();
    expect(document.body.textContent).not.toContain("Archive results may be stale.");
    await unmount(component);
  });

  it("refreshes the sync label as time passes", async () => {
    const component = mount(StatusBar, {
      target: document.body,
    });

    await tick();
    const syncLabel = document.querySelector(".kit-status-bar__section--right span[title]");
    // Compute the expected title through the same i18n path the component
    // uses. toLocaleString(undefined, ...) resolves to the OS locale, which
    // diverges from the Paraglide app locale on non-English systems (#1195).
    const expectedTitle = formatTimestamp(sync.lastSync!);

    expect(document.body.textContent).toContain("synced just now");
    expect(syncLabel?.getAttribute("title")).toBe(expectedTitle);

    await vi.advanceTimersByTimeAsync(70_000);
    await tick();

    expect(document.body.textContent).toContain("synced 1m ago");

    unmount(component);
  });

  it("shows a remote-unreachable indicator only when flagged", async () => {
    sync.remoteUnreachable = true;
    const component = mount(StatusBar, {
      target: document.body,
    });
    await tick();
    expect(document.body.textContent).toContain("remote server unreachable");

    sync.remoteUnreachable = false;
    await tick();
    expect(document.body.textContent).not.toContain("remote server unreachable");

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
    expect(document.querySelector(".backend-warn")?.getAttribute("title")).toBe("sync not ready");

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

  describe("codex-cloud status", () => {
    const upToDate = {
      key: "account-a",
      homes: ["/synthetic/codex-home"],
      state: "up_to_date",
      last_success: "2026-04-08T04:55:00Z",
      live: "Live tokens: watching 1 running threads",
      plan_usage: "Plan usage: not checked yet",
    };

    async function mountWithRows(): Promise<ReturnType<typeof mount>> {
      const component = mount(StatusBar, { target: document.body });
      await vi.advanceTimersByTimeAsync(0);
      await tick();
      return component;
    }

    it("shows nothing when the source is not configured", async () => {
      const component = await mountWithRows();

      expect(getCodexCloudStatus).toHaveBeenCalled();
      expect(document.querySelector(".codex-cloud-status")).toBeNull();
      void unmount(component);
    });

    it("shows the state with each line in its tooltip", async () => {
      getCodexCloudStatus.mockResolvedValue([upToDate]);
      const component = await mountWithRows();

      const line = document.querySelector(".codex-cloud-status");
      expect(line?.textContent?.trim()).toBe(
        `Codex Cloud: up to date as of ${formatTimestamp("2026-04-08T04:55:00Z")}`,
      );
      expect(line?.classList.contains("codex-cloud-status--warn")).toBe(false);
      expect(line?.getAttribute("title")?.split("\n")).toEqual([
        "Codex homes: /synthetic/codex-home",
        `Codex Cloud: up to date as of ${formatTimestamp("2026-04-08T04:55:00Z")}`,
        "Live tokens: watching 1 running threads",
        "Plan usage: not checked yet",
      ]);
      void unmount(component);
    });

    it("summarizes the row that needs attention", async () => {
      getCodexCloudStatus.mockResolvedValue([
        upToDate,
        {
          key: "account-b",
          homes: ["/synthetic/other-home"],
          state: "retrying",
          retry_count: 2,
          oldest_failure: "2026-04-08T03:00:00Z",
          message: "Can't reach OpenAI. AgentsView retries automatically.",
        },
      ]);
      const component = await mountWithRows();

      const line = document.querySelector(".codex-cloud-status");
      expect(line?.textContent?.trim()).toBe("Codex Cloud: 2 threads could not be imported");
      expect(line?.classList.contains("codex-cloud-status--warn")).toBe(true);
      expect(line?.getAttribute("title")).toContain(
        "Codex homes: /synthetic/other-home\n" +
          "Codex Cloud: 2 threads could not be imported\n" +
          `Oldest failure: ${formatTimestamp("2026-04-08T03:00:00Z")}\n` +
          "Can't reach OpenAI. AgentsView retries automatically.\n" +
          "AgentsView keeps retrying them.",
      );
      void unmount(component);
    });

    // Keep this test last: the store remembers the 404 for the page's life.
    it("stops asking a server without the status route", async () => {
      getCodexCloudStatus.mockRejectedValue(new ApiError(404, "not found"));
      const component = await mountWithRows();
      const calls = getCodexCloudStatus.mock.calls.length;

      await vi.advanceTimersByTimeAsync(30_000);

      expect(getCodexCloudStatus.mock.calls.length).toBe(calls);
      expect(document.querySelector(".codex-cloud-status")).toBeNull();
      void unmount(component);
    });
  });
});
