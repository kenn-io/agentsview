// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi, type MockInstance } from "vite-plus/test";
import { mount, tick, unmount } from "svelte";
// @ts-ignore
import HourOfWeekHeatmap from "./HourOfWeekHeatmap.svelte";
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
      cells: Array.from({ length: 168 }, (_, index) => ({
        day_of_week: Math.floor(index / 24),
        hour: index % 24,
        messages: index === 144 ? 9 : index === 1 ? 3 : 0,
      })),
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
    const data = analytics.hourOfWeek;
    analytics.hourOfWeek = null;
    await tick();
    expect(document.querySelector(".loading")).not.toBeNull();
    analytics.hourOfWeek = data;
    await tick();
    expect(document.querySelector(".loading")).toBeNull();
    analytics.errors.hourOfWeek = "request failed";
    await tick();
    expect(document.querySelector(".error")).not.toBeNull();
    expect(document.querySelector(".how-chart")).not.toBeNull();

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

    analytics.hourOfWeek = null;
    await tick();
    expect(document.querySelector(".how-chart")).toBeNull();

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
});
