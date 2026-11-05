// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi, type MockInstance } from "vite-plus/test";
import { mount, tick, unmount } from "svelte";
// @ts-ignore
import HourOfWeekHeatmap from "./HourOfWeekHeatmap.svelte";
import Heatmap from "./Heatmap.svelte";
import { analytics } from "../../stores/analytics.svelte.js";

const originalClientWidth = Object.getOwnPropertyDescriptor(HTMLElement.prototype, "clientWidth");

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
    if (originalClientWidth) {
      Object.defineProperty(HTMLElement.prototype, "clientWidth", originalClientWidth);
    }
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
      vi.spyOn(analytics, "fetchSummary").mockResolvedValue("ok"),
      vi.spyOn(analytics, "fetchActivity").mockResolvedValue("ok"),
      vi.spyOn(analytics, "fetchHeatmap").mockResolvedValue("ok"),
      vi.spyOn(analytics, "fetchProjects").mockResolvedValue("ok"),
      vi.spyOn(analytics, "fetchSessionShape").mockResolvedValue("ok"),
      vi.spyOn(analytics, "fetchVelocity").mockResolvedValue("ok"),
      vi.spyOn(analytics, "fetchTools").mockResolvedValue("ok"),
      vi.spyOn(analytics, "fetchSkills").mockResolvedValue("ok"),
      vi.spyOn(analytics, "fetchTopSessions").mockResolvedValue("ok"),
      vi.spyOn(analytics, "fetchSignals").mockResolvedValue("ok"),
    ];
  }

  it("renders Sunday first while preserving Monday-zero filter values", async () => {
    const component = mountWithData();
    await tick();

    const dayLabels = Array.from(document.querySelectorAll(".day-label")).map((el) =>
      el.textContent?.trim(),
    );
    expect(dayLabels).toEqual(["Sun", "Mon", "Tue", "Wed", "Thu", "Fri", "Sat"]);

    const fetchSpies = stubFetches();
    const firstCell = document.querySelector(".how-cell");
    expect(firstCell).toBeTruthy();
    firstCell!.dispatchEvent(new MouseEvent("click", { bubbles: true }));
    await tick();

    expect(analytics.selectedDow).toBe(6);
    expect(analytics.selectedHour).toBe(0);
    for (const spy of fetchSpies) {
      expect(spy).toHaveBeenCalledOnce();
    }

    unmount(component);
  });

  it("fits all 24 hour columns inside the available width", async () => {
    Object.defineProperty(HTMLElement.prototype, "clientWidth", {
      configurable: true,
      get: () => 480,
    });
    const component = mountWithData();
    await tick();

    const svg = document.querySelector<SVGSVGElement>("svg");
    expect(Number(svg?.getAttribute("width"))).toBe(480);
    const cells = document.querySelectorAll<SVGRectElement>(".how-cell");
    expect(cells).toHaveLength(7 * 24);
    const last = cells[cells.length - 1]!;
    expect(Number(last.getAttribute("x")) + Number(last.getAttribute("width"))).toBeLessThanOrEqual(
      480,
    );

    unmount(component);
  });

  it("steps by quartile and shares the calendar heatmap's colors", async () => {
    // Monday hours 0-7 carry 1..8 messages: quartiles put 1-3 on the first
    // step, 4-5 on the second, 6-7 on the third and 8 on the top step.
    analytics.hourOfWeek = {
      cells: Array.from({ length: 8 }, (_, hour) => ({
        day_of_week: 0,
        hour,
        messages: hour + 1,
      })),
    };
    analytics.heatmap = {
      metric: "messages",
      entries_from: "2026-01-05",
      levels: { l1: 1, l2: 2, l3: 3, l4: 4 },
      entries: Array.from({ length: 5 }, (_, level) => ({
        date: `2026-01-0${5 + level}`,
        value: level,
        level,
      })),
    };
    const hours = mount(HourOfWeekHeatmap, { target: document.body });
    const calendarHost = document.createElement("div");
    document.body.append(calendarHost);
    const calendar = mount(Heatmap, { target: calendarHost });
    await tick();

    const howCells = Array.from(document.querySelectorAll<SVGRectElement>(".how-cell"));
    // Rows run Sunday first, so Monday is the second row of 24 cells.
    const monday = howCells.slice(24, 32).map((cell) => cell.getAttribute("fill"));
    const calendarFills = Array.from(
      calendarHost.querySelectorAll<SVGRectElement>(".heatmap-cell"),
    )
      .slice(-5)
      .map((cell) => cell.getAttribute("fill"));

    expect(new Set(calendarFills).size).toBe(5);
    const [empty, step1, step2, step3, step4] = calendarFills;
    expect(monday).toEqual([step1, step1, step1, step2, step2, step3, step3, step4]);
    expect(howCells[0]!.getAttribute("fill")).toBe(empty);

    unmount(calendar);
    unmount(hours);
    analytics.heatmap = null;
  });
});
