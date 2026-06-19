import { describe, expect, it } from "vite-plus/test";
import source from "./UsagePage.svelte?raw";

describe("UsagePage refresh behavior", () => {
  it("does not auto-refresh usage scans from SSE updates", () => {
    expect(source).not.toContain("subscribeDebounced");
    expect(source).not.toContain("REFRESH_MS");
    // SSE only flags new data; the periodic refetch lives in RefreshControl.
    expect(source).toContain("usage.markNewData");
    expect(source).toContain("events.subscribe");
  });

  it("delegates the refresh affordance and scheduler to RefreshControl", () => {
    expect(source).toContain("<RefreshControl");
    expect(source).toContain("usage.lastUpdatedAt");
    expect(source).toContain('label="Refresh usage data"');
    expect(source).toContain('title="Refresh"');
    // The scheduler, label tick, and icon now live in the shared component.
    expect(source).not.toContain("REFRESH_LABEL_INTERVAL_MS");
    expect(source).not.toContain("formatRefreshAge");
    expect(source).not.toContain("RefreshCwIcon");
    expect(source).not.toContain("setInterval");
  });

  it("shows relative last-updated status without ambiguous badges", () => {
    expect(source).not.toContain("formatUpdatedAt");
    expect(source).not.toContain("usage.hasNewData");
    expect(source).not.toContain("New data");
    expect(source).not.toContain(".new-data");
  });

  it("treats termination as a usage URL session filter", () => {
    expect(source).toContain('"termination",');
    expect(source).toContain("filtersToParams(sessions.filters)");
  });

  it("keeps refresh progress out of content layout flow", () => {
    const queryProgress =
      source.match(/\.query-progress\s*{[^}]+}/)?.[0] ?? "";

    expect(queryProgress).toContain("position: absolute");
    expect(queryProgress).toContain("left: 0;");
    expect(queryProgress).toContain("right: 0;");
    expect(queryProgress).not.toContain("position: sticky");
    expect(queryProgress).not.toContain("margin:");
  });
});
