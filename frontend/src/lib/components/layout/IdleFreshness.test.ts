// @vitest-environment jsdom
import { afterEach, expect, it, vi } from "vite-plus/test";
import { sync } from "../../stores/sync.svelte.js";
import { getApiV1Search } from "../../api/generated/search/search.js";

afterEach(() => {
  vi.unstubAllGlobals();
  sync.syncPending = false;
});
it("marks catch-up pending when a search response carries the stale header", async () => {
  sync.syncPending = false;
  vi.stubGlobal(
    "fetch",
    vi.fn().mockResolvedValue(
      new Response('{"results":[]}', {
        headers: { "Content-Type": "application/json", "X-Agentsview-Archive-Stale": "true" },
      }),
    ),
  );
  await getApiV1Search({ q: "overnight" });
  expect(sync.syncPending).toBe(true);
});
