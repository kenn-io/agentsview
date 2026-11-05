// @vitest-environment jsdom
import { afterEach, expect, it, vi } from "vite-plus/test";
import { mount, tick, unmount } from "svelte";
import StatusBar from "./StatusBar.svelte";
import { sync } from "../../stores/sync.svelte.js";
import { getApiV1Search } from "../../api/generated/search/search.js";

const { cloud } = vi.hoisted(() => ({ cloud: vi.fn().mockResolvedValue([]) }));
vi.mock("../../api/generated/index", async (original) => ({
  ...(await original<typeof import("../../api/generated/index")>()),
  CodexCloudService: { getApiV1CodexCloudStatus: cloud },
}));
afterEach(() => {
  vi.unstubAllGlobals();
  document.body.innerHTML = "";
});
it("warns on the search result returned while idle catch-up is pending", async () => {
  sync.archiveStale = false;
  sync.syncPending = false;
  sync.serverVersion = null;
  sync.versionMismatch = false;
  sync.lastSync = null;
  vi.stubGlobal(
    "fetch",
    vi.fn().mockResolvedValue(
      new Response('{"results":[]}', {
        headers: { "Content-Type": "application/json", "X-Agentsview-Archive-Stale": "true" },
      }),
    ),
  );
  const component = mount(StatusBar, { target: document.body });
  try {
    await getApiV1Search({ q: "overnight" });
    await tick();
    expect(document.body.textContent).toContain("Archive results may be stale.");
  } finally {
    await unmount(component);
  }
});
