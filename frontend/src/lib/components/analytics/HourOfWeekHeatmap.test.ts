// @vitest-environment jsdom
import {
  afterEach,
  describe,
  expect,
  it,
  vi,
  type MockInstance,
} from "vitest";
import { mount, tick, unmount } from "svelte";
// @ts-ignore
import HourOfWeekHeatmap from "./HourOfWeekHeatmap.svelte";
import { analytics } from "../../stores/analytics.svelte.js";

describe("HourOfWeekHeatmap", () => {
  afterEach(() => {
    analytics.hourOfWeek = null;
    analytics.selectedDow = null;
    analytics.selectedHour = null;
    // @ts-ignore
    analytics.errors = {
      ...analytics.errors,
      hourOfWeek: null,
    };
    document.body.innerHTML = "";
    vi.restoreAllMocks();
  });

  function mountWithData() {
    analytics.hourOfWeek = {
      cells: [
        { day_of_week: 6, hour: 0, messages: 9 },
        { day_of_week: 0, hour: 1, messages: 3 },
      ],
    };
    // @ts-ignore
    analytics.errors = {
      ...analytics.errors,
      hourOfWeek: null,
    };

    return mount(HourOfWeekHeatmap, { target: document.body });
  }

  function stubFetches(): MockInstance[] {
    return [
      vi.spyOn(analytics, "fetchSummary").mockResolvedValue(),
      vi.spyOn(analytics, "fetchActivity").mockResolvedValue(),
      vi.spyOn(analytics, "fetchHeatmap").mockResolvedValue(),
      vi.spyOn(analytics, "fetchProjects").mockResolvedValue(),
      vi.spyOn(analytics, "fetchSessionShape").mockResolvedValue(),
      vi.spyOn(analytics, "fetchVelocity").mockResolvedValue(),
      vi.spyOn(analytics, "fetchTools").mockResolvedValue(),
      vi.spyOn(analytics, "fetchTopSessions").mockResolvedValue(),
      vi.spyOn(analytics, "fetchSignals").mockResolvedValue(),
    ];
  }

  it("renders Sunday first while preserving Monday-zero filter values", async () => {
    const component = mountWithData();
    await tick();

    const dayLabels = Array.from(
      document.querySelectorAll(".day-label"),
    ).map((el) => el.textContent?.trim());
    expect(dayLabels).toEqual([
      "Sun",
      "Mon",
      "Tue",
      "Wed",
      "Thu",
      "Fri",
      "Sat",
    ]);

    const fetchSpies = stubFetches();
    const firstCell = document.querySelector(".how-cell");
    expect(firstCell).toBeTruthy();
    firstCell!.dispatchEvent(
      new MouseEvent("click", { bubbles: true }),
    );
    await tick();

    expect(analytics.selectedDow).toBe(6);
    expect(analytics.selectedHour).toBe(0);
    for (const spy of fetchSpies) {
      expect(spy).toHaveBeenCalledOnce();
    }

    unmount(component);
  });
});
