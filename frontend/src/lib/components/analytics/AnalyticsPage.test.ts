import { describe, expect, it } from "vite-plus/test";
import source from "./AnalyticsPage.svelte?raw";

describe("AnalyticsPage refresh behavior", () => {
  it("does not refresh analytical scans from SSE updates", () => {
    expect(source).not.toContain("subscribeDebounced");
  });

  it("keeps automatic refresh bounded to five minutes", () => {
    expect(source).toContain("REFRESH_INTERVAL_MS = 5 * 60 * 1000");
    expect(source).toContain("createRefreshScheduler");
    expect(source).toContain("refreshScheduler.refreshNow()");
    expect(source).not.toContain("setInterval");
  });

  it("shows relative last-updated refresh status without ambiguous badges", () => {
    expect(source).toContain("analytics.lastUpdatedAt");
    expect(source).toContain("REFRESH_LABEL_INTERVAL_MS = 60 * 1000");
    expect(source).toContain("formatRefreshAge");
    expect(source).not.toContain("formatUpdatedAt");
    expect(source).not.toContain("analytics.hasNewData");
    expect(source).not.toContain("New data");
    expect(source).not.toContain(".new-data");
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
