import { afterEach, beforeEach, expect, it, vi } from "vite-plus/test";
import { createSearchStore } from "./search.svelte.js";

vi.mock("../utils/telemetry.js", () => ({ reportTelemetry: vi.fn(), reportErrorShown: vi.fn() }));

beforeEach(() => vi.useFakeTimers());
afterEach(() => {
  vi.useRealTimers();
  vi.unstubAllGlobals();
});

it("does not repeat a search when full-text search is unavailable", async () => {
  const fetch = vi
    .fn()
    .mockResolvedValue(
      new Response(JSON.stringify({ error: "search unavailable" }), {
        status: 501,
        headers: { "Content-Type": "application/json" },
      }),
    );
  vi.stubGlobal("fetch", fetch);
  const store = createSearchStore(null);
  store.search("needle");
  await vi.advanceTimersByTimeAsync(300);
  expect(fetch).toHaveBeenCalledTimes(1);
  expect(store.error).toEqual({ detail: "search unavailable", kind: "generic" });
});
