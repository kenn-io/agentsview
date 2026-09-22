import { describe, expect, it } from "vite-plus/test";
import { LiveQuery } from "./liveQuery.svelte.js";

describe("LiveQuery", () => {
  it("settles a running step in place with its measured timing", () => {
    const live = new LiveQuery();
    live.begin(1000);
    const summary = live.start("summary", 1000);
    live.start("tools", 1010);
    live.settle(summary, { name: "summary", startMs: 0, durationMs: 40 });

    expect(live.steps).toEqual([
      { name: "summary", startMs: 0, durationMs: 40 },
      { name: "tools", startMs: 10, durationMs: 0, running: true },
    ]);
  });

  it("keeps a retried step when the replaced request stops afterwards", () => {
    const live = new LiveQuery();
    live.begin(0);
    const first = live.start("tools", 0);
    const retry = live.start("tools", 5);
    // The replaced request is aborted and cleans up after the retry began.
    live.abandon(first);
    live.settle(first, { name: "tools", startMs: 0, durationMs: 1 });

    expect(live.steps).toEqual([{ name: "tools", startMs: 5, durationMs: 0, running: true }]);
    live.settle(retry, { name: "tools", startMs: 5, durationMs: 20 });
    expect(live.steps).toEqual([{ name: "tools", startMs: 5, durationMs: 20 }]);
  });

  it("drops a step that stopped without data but keeps settled ones", () => {
    const live = new LiveQuery();
    live.begin(0);
    const summary = live.start("summary", 0);
    const tools = live.start("tools", 0);
    live.settle(summary, { name: "summary", startMs: 0, durationMs: 30 });
    live.abandon(summary);
    live.abandon(tools);

    expect(live.steps).toEqual([{ name: "summary", startMs: 0, durationMs: 30 }]);
  });

  it("lets only the current query end itself", () => {
    const live = new LiveQuery();
    const first = live.begin(0);
    live.begin(100);
    live.end(first);
    expect(live.startedAt).toBe(100);
  });
});
