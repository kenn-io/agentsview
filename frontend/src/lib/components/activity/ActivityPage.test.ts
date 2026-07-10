// @vitest-environment jsdom
import {
  afterEach,
  describe,
  expect,
  it,
  vi,
} from "vite-plus/test";
import { mount, tick, unmount } from "svelte";
import { activity } from "../../stores/activity.svelte.js";
import { router } from "../../stores/router.svelte.js";
import { yokedDates } from "../../stores/yokedDates.svelte.js";
import source from "./ActivityPage.svelte?raw";
// @ts-ignore
import ActivityPage from "./ActivityPage.svelte";

async function flushEffects() {
  await tick();
  await Promise.resolve();
  await tick();
}

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

describe("ActivityPage date yoke integration", () => {
  let component: ReturnType<typeof mount> | undefined;

  afterEach(() => {
    if (component) {
      unmount(component);
      component = undefined;
    }
    vi.restoreAllMocks();
    vi.unstubAllGlobals();
    document.body.innerHTML = "";
    window.history.replaceState(null, "", "/");
    router.route = "sessions";
    router.params = {};
    activity.preset = "day";
    activity.from = "";
    activity.to = "";
    activity.rollingWindowDays = null;
    yokedDates.setEnabled(false);
    localStorage.clear();
  });

  it("seeds bare Activity from an enabled representable fixed range", async () => {
    const loadStates: Array<{
      preset: string;
      from: string;
      to: string;
    }> = [];
    vi.stubGlobal(
      "ResizeObserver",
      class {
        observe() {}
        disconnect() {}
      },
    );
    vi.spyOn(activity, "attach").mockReturnValue(() => {});
    vi.spyOn(activity, "loadFilterOptions").mockResolvedValue();
    vi.spyOn(activity, "load").mockImplementation(() => {
      loadStates.push({
        preset: activity.preset,
        from: activity.from,
        to: activity.to,
      });
      return Promise.resolve();
    });
    router.route = "activity";
    router.params = {};
    activity.preset = "day";
    activity.from = "";
    activity.to = "";
    activity.rollingWindowDays = null;
    yokedDates.setEnabled(true);
    yokedDates.updateFromPanel({
      from: "2026-06-01",
      to: "2026-06-07",
      mode: "fixed",
    });

    component = mount(ActivityPage, { target: document.body });
    await flushEffects();

    expect(activity.preset).toBe("custom");
    expect(activity.from).toBe("2026-06-01");
    expect(activity.to).toBe("2026-06-07");
    expect(loadStates[0]).toEqual({
      preset: "custom",
      from: "2026-06-01",
      to: "2026-06-07",
    });
  });
});
