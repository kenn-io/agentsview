import { beforeEach, describe, expect, it } from "vite-plus/test";
import { createPerfStore } from "./perf.svelte.js";

describe("PerfStore", () => {
  let perf: ReturnType<typeof createPerfStore>;

  beforeEach(() => {
    perf = createPerfStore({ maxEntries: 3 });
  });

  it("records recent API and panel timings newest first", () => {
    perf.recordApi({
      method: "GET",
      path: "/api/v1/stats",
      status: 200,
      durationMs: 12.4,
      route: "sessions",
    });
    perf.recordPanel({
      route: "usage",
      name: "summary",
      durationMs: 98.7,
      status: "ok",
    });

    expect(perf.entries.map((entry) => entry.kind)).toEqual(["panel", "api"]);
    expect(perf.entries[0]).toMatchObject({
      kind: "panel",
      route: "usage",
      name: "summary",
      durationMs: 98.7,
      status: "ok",
    });
  });

  it("bounds entry history and can clear it", () => {
    for (let i = 0; i < 5; i++) {
      perf.recordPanel({
        route: "usage",
        name: `panel-${i}`,
        durationMs: i,
        status: "ok",
      });
    }

    expect(perf.entries.map((entry) => entry.name)).toEqual(["panel-4", "panel-3", "panel-2"]);

    perf.clear();
    expect(perf.entries).toEqual([]);
  });
});
