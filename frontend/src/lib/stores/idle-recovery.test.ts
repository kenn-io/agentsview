import { afterEach, expect, it, vi } from "vite-plus/test";
import { sync } from "./sync.svelte.js";
import { searchStore } from "./search.svelte.js";

vi.mock("../utils/telemetry.js", () => ({
  reportSyncFailed: vi.fn(),
  reportTelemetry: vi.fn(),
  reportErrorShown: vi.fn(),
}));

afterEach(() => {
  sync.stopPolling();
  searchStore.clear();
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
  vi.useRealTimers();
});

it("refreshes a search whose catch-up completed between status polls", async () => {
  vi.useFakeTimers();
  vi.spyOn(document, "hidden", "get").mockReturnValue(false);
  let complete = false;
  vi.stubGlobal(
    "fetch",
    vi.fn(async (url: string) => {
      if (url.endsWith("/api/v1/sync/status"))
        return Response.json({
          last_sync: complete ? "2026-11-05T12:00:03Z" : "2026-11-04T12:00:00Z",
          sync_pending: false,
          archive_stale: false,
        });
      if (url.endsWith("/api/v1/version"))
        return Response.json({ version: "dev", read_only: false });
      if (url.includes("/api/v1/stats"))
        return Response.json({ session_count: complete ? 1 : 0, message_count: complete ? 1 : 0 });
      if (url.includes("/api/v1/search?")) {
        if (!complete) {
          setTimeout(() => {
            complete = true;
          }, 3000);
          await new Promise((resolve) => setTimeout(resolve, 2000));
          return Response.json(
            { results: [], count: 0 },
            { headers: { "X-Agentsview-Archive-Stale": "true" } },
          );
        }
        return Response.json({
          results: [
            {
              session_id: "gemini:overnight",
              project: "alpha",
              agent: "gemini",
              ordinal: 0,
              session_ended_at: "2026-11-05T12:00:00Z",
              snippet: "overnight",
              rank: 1,
            },
          ],
          count: 1,
        });
      }
      throw new Error(`unexpected URL ${url}`);
    }),
  );
  sync.syncPending = false;
  await sync.loadStatus();
  sync.startPolling();
  searchStore.search("overnight");
  await vi.advanceTimersByTimeAsync(2300);
  expect(searchStore.results).toEqual([]);
  await vi.advanceTimersByTimeAsync(8700);
  expect(complete).toBe(true);
  expect(searchStore.results.map((result) => result.session_id)).toEqual(["gemini:overnight"]);
});
