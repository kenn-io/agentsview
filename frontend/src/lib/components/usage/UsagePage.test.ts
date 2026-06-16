import { describe, expect, it } from "vite-plus/test";
import source from "./UsagePage.svelte?raw";

describe("UsagePage refresh behavior", () => {
  it("does not auto-refresh usage scans from SSE updates", () => {
    expect(source).not.toContain("subscribeDebounced");
    expect(source).not.toContain("REFRESH_MS");
  });

  it("shows relative last-updated and new-data refresh hints", () => {
    expect(source).toContain("usage.lastUpdatedAt");
    expect(source).toContain("REFRESH_LABEL_INTERVAL_MS = 60 * 1000");
    expect(source).toContain("formatRefreshAge");
    expect(source).not.toContain("formatUpdatedAt");
    expect(source).toContain("usage.hasNewData");
    expect(source).toContain("New data");
  });
});
