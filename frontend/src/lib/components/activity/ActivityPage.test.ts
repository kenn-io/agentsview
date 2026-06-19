import { describe, expect, it } from "vite-plus/test";
import source from "./ActivityPage.svelte?raw";

describe("ActivityPage filter controls", () => {
  it("uses explicit chevrons instead of browser-native select arrows", () => {
    const selectWrapCount = source.match(/class="filter-select-wrap"/g)?.length ?? 0;
    const selectChevronCount = source.match(/class="filter-select-chevron"/g)?.length ?? 0;
    const filterSelectStyles = source.match(/\.filter-select\s*{[^}]+}/)?.[0] ?? "";

    expect(selectWrapCount).toBe(3);
    expect(selectChevronCount).toBe(3);
    expect(filterSelectStyles).toContain("appearance: none");
  });
});

describe("ActivityPage refresh control layout", () => {
  it("keeps the shared refresh control inline with the toolbar filters", () => {
    expect(source).toContain("<RefreshControl");
    expect(source).toContain("activity.lastUpdatedAt");
    expect(source).not.toContain("refresh-slot");
    expect(source).not.toContain("margin-left: auto");
  });
});
