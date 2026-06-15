import { describe, expect, it } from "vite-plus/test";
import source from "./AnalyticsPage.svelte?raw";

describe("AnalyticsPage refresh behavior", () => {
  it("does not refresh analytical scans from SSE updates", () => {
    expect(source).not.toContain("subscribeDebounced");
  });

  it("keeps automatic refresh bounded to five minutes", () => {
    expect(source).toContain("REFRESH_INTERVAL_MS = 5 * 60 * 1000");
    expect(source).toContain("setInterval");
  });

  it("shows last-updated and new-data refresh hints", () => {
    expect(source).toContain("analytics.lastUpdatedAt");
    expect(source).toContain("analytics.hasNewData");
    expect(source).toContain("New data");
  });
});
