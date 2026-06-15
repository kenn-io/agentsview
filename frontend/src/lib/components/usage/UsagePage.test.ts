import { describe, expect, it } from "vite-plus/test";
import source from "./UsagePage.svelte?raw";

describe("UsagePage refresh behavior", () => {
  it("does not auto-refresh usage scans from SSE or a timer", () => {
    expect(source).not.toContain("subscribeDebounced");
    expect(source).not.toContain("setInterval");
    expect(source).not.toContain("REFRESH_MS");
  });

  it("shows last-updated and new-data refresh hints", () => {
    expect(source).toContain("usage.lastUpdatedAt");
    expect(source).toContain("usage.hasNewData");
    expect(source).toContain("New data");
  });
});
