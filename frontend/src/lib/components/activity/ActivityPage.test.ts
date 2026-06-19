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

describe("ActivityPage date yoke controls", () => {
  it("updates shared yoke state from the unified range picker", () => {
    expect(source).toContain("<RangePicker");
    expect(source).toContain("seedActivityYoke");
    expect(source).toContain("yokedDates.updateFromPanel");
  });

  it("yokes week and month selections using resolved period starts", () => {
    expect(source).toContain("startOfIsoWeek(activity.date)");
    expect(source).toContain("startOfMonth(activity.date)");
    expect(source).not.toContain(
      "panelDateState(activity.date, addDays(activity.date, 6)",
    );
    expect(source).not.toContain(
      "panelDateState(activity.date, endOfMonth(activity.date)",
    );
  });

  it("preserves relative range selections as rolling yoke state", () => {
    const applyIndex = source.indexOf("function applyRange");
    const helperIndex = source.indexOf("function yokeStateForSelection");
    const applyBlock = source.slice(applyIndex, helperIndex);

    expect(helperIndex).toBeGreaterThan(applyIndex);
    expect(source).toContain('mode: "rolling"');
    expect(source).toContain("windowDays: sel.days");
    expect(applyBlock).toContain("yokeStateForSelection(sel, range)");
    expect(applyBlock).toContain("lastActivityDateSignature = dateSignature");
  });

  it("preserves rolling window intent in activity URLs", () => {
    expect(source).toContain("activity.rollingWindowDays");
    expect(source).toContain("activity.setCustomRange");
    expect(source).toContain("params.window_days");
    expect(source).toContain('mode: "relative", days: activity.rollingWindowDays');
  });
});
