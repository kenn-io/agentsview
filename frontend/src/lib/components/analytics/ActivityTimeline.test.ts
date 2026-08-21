// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from "vite-plus/test";
import { mount, tick, unmount } from "svelte";
import { analytics } from "../../stores/analytics.svelte.js";
import ActivityTimeline from "./ActivityTimeline.svelte";

const originalClientWidth = Object.getOwnPropertyDescriptor(HTMLElement.prototype, "clientWidth");

describe("ActivityTimeline", () => {
  afterEach(() => {
    analytics.activity = null;
    analytics.from = "2025-08-21";
    analytics.to = "2026-08-20";
    analytics.granularity = "day";
    analytics.selectedActivityRange = null;
    analytics.errors.activity = null;
    document.body.innerHTML = "";
    vi.restoreAllMocks();
    if (originalClientWidth) {
      Object.defineProperty(HTMLElement.prototype, "clientWidth", originalClientWidth);
    }
  });

  it("fits the densest supported daily range to the available width", async () => {
    Object.defineProperty(HTMLElement.prototype, "clientWidth", {
      configurable: true,
      get: () => 800,
    });
    analytics.granularity = "day";
    analytics.from = "2025-01-01";
    analytics.to = "2025-04-30";
    analytics.activity = {
      granularity: "day",
      series: Array.from({ length: 120 }, (_, index) => {
        const date = new Date(Date.UTC(2025, 0, 1 + index)).toISOString().slice(0, 10);
        return {
          date,
          sessions: 1,
          messages: 2,
          user_messages: 1,
          assistant_messages: 1,
          tool_calls: 0,
          thinking_messages: 0,
          by_agent: {},
        };
      }),
    };

    const component = mount(ActivityTimeline, { target: document.body });
    await tick();

    const chart = document.querySelector<SVGSVGElement>("svg");
    expect(chart).not.toBeNull();
    expect(Number(chart!.getAttribute("width"))).toBe(800);
    const bars = document.querySelectorAll<SVGRectElement>("rect.bar");
    expect(bars).toHaveLength(120);
    expect(Number(bars[0]!.getAttribute("width"))).toBeGreaterThan(0);

    unmount(component);
  });

  it("disables Day and switches to Week for longer ranges", async () => {
    analytics.from = "2025-01-01";
    analytics.to = "2025-12-31";
    analytics.granularity = "day";
    const fetchSpy = vi.spyOn(analytics, "fetchActivity").mockResolvedValue("ok");

    const component = mount(ActivityTimeline, { target: document.body });
    await tick();

    const dayButton = [
      ...document.querySelectorAll<HTMLButtonElement>(".granularity-toggle button"),
    ].find((button) => button.textContent?.trim() === "Day");
    expect(dayButton?.disabled).toBe(true);
    expect(analytics.granularity).toBe("week");
    expect(fetchSpy).toHaveBeenCalledOnce();

    unmount(component);
  });

  it("keeps daily date labels readable in a two-week range", async () => {
    Object.defineProperty(HTMLElement.prototype, "clientWidth", {
      configurable: true,
      get: () => 600,
    });
    analytics.from = "2026-08-01";
    analytics.to = "2026-08-14";
    analytics.activity = {
      granularity: "day",
      series: Array.from({ length: 14 }, (_, index) => ({
        date: `2026-08-${String(index + 1).padStart(2, "0")}`,
        sessions: 1,
        messages: 2,
        user_messages: 1,
        assistant_messages: 1,
        tool_calls: 0,
        thinking_messages: 0,
        by_agent: {},
      })),
    };

    const component = mount(ActivityTimeline, { target: document.body });
    await tick();

    const labels = [...document.querySelectorAll<SVGTextElement>(".x-label")].map((label) =>
      label.textContent?.trim(),
    );
    expect(labels.length).toBeLessThanOrEqual(7);
    expect(new Set(labels).size).toBe(labels.length);

    unmount(component);
  });

  it("shows every day in the selected range, including inactive days", async () => {
    analytics.from = "2026-08-01";
    analytics.to = "2026-08-30";
    analytics.activity = {
      granularity: "day",
      series: [
        {
          date: "2026-08-03",
          sessions: 2,
          messages: 4,
          user_messages: 2,
          assistant_messages: 2,
          tool_calls: 0,
          thinking_messages: 0,
          by_agent: {},
        },
        {
          date: "2026-08-19",
          sessions: 1,
          messages: 2,
          user_messages: 1,
          assistant_messages: 1,
          tool_calls: 0,
          thinking_messages: 0,
          by_agent: {},
        },
      ],
    };

    const component = mount(ActivityTimeline, { target: document.body });
    await tick();

    expect(document.querySelectorAll("rect.bar")).toHaveLength(30);

    unmount(component);
  });

  it("commits a snapped date range from the LayerChart brush", async () => {
    Object.defineProperty(HTMLElement.prototype, "clientWidth", {
      configurable: true,
      get: () => 600,
    });
    analytics.from = "2026-08-01";
    analytics.to = "2026-08-10";
    analytics.activity = {
      granularity: "day",
      series: Array.from({ length: 10 }, (_, index) => ({
        date: `2026-08-${String(index + 1).padStart(2, "0")}`,
        sessions: 1,
        messages: 2,
        user_messages: 1,
        assistant_messages: 1,
        tool_calls: 0,
        thinking_messages: 0,
        by_agent: {},
      })),
    };
    const onRangeSelect = vi.fn();

    const component = mount(ActivityTimeline, {
      target: document.body,
      props: { onRangeSelect },
    });
    await tick();
    await tick();

    await vi.waitFor(() => {
      expect(document.querySelector(".lc-brush-context")).not.toBeNull();
    });
    const brush = document.querySelector<HTMLElement>(".lc-brush-context");
    expect(brush).not.toBeNull();
    vi.spyOn(brush!, "getBoundingClientRect").mockReturnValue({
      x: 0,
      y: 0,
      left: 0,
      top: 0,
      right: 552,
      bottom: 124,
      width: 552,
      height: 124,
      toJSON: () => ({}),
    });

    brush!.dispatchEvent(
      new MouseEvent("pointerdown", {
        bubbles: true,
        clientX: 120,
        clientY: 40,
      }),
    );
    window.dispatchEvent(
      new MouseEvent("pointermove", {
        bubbles: true,
        clientX: 300,
        clientY: 40,
      }),
    );
    brush!.dispatchEvent(
      new MouseEvent("pointerup", {
        bubbles: true,
        clientX: 300,
        clientY: 40,
      }),
    );
    await tick();

    expect(onRangeSelect).toHaveBeenCalledExactlyOnceWith("2026-08-03", "2026-08-06");
    expect(document.querySelector(".activity-brush-range")).not.toBeNull();

    unmount(component);
  });

  it("exposes a visible clear action for a brushed selection", async () => {
    analytics.from = "2026-08-01";
    analytics.to = "2026-08-10";
    analytics.selectedActivityRange = { from: "2026-08-03", to: "2026-08-06" };
    analytics.activity = {
      granularity: "day",
      series: Array.from({ length: 10 }, (_, index) => ({
        date: `2026-08-${String(index + 1).padStart(2, "0")}`,
        sessions: 1,
        messages: 2,
        user_messages: 1,
        assistant_messages: 1,
        tool_calls: 0,
        thinking_messages: 0,
        by_agent: {},
      })),
    };
    const onRangeClear = vi.fn();

    const component = mount(ActivityTimeline, {
      target: document.body,
      props: { onRangeClear },
    });
    await tick();

    const clear = [...document.querySelectorAll<HTMLButtonElement>("button")].find(
      (button) => button.textContent?.trim() === "Clear selection",
    );
    expect(clear).toBeDefined();
    clear!.click();
    expect(onRangeClear).toHaveBeenCalledOnce();

    unmount(component);
  });

  it("extends a keyboard selection with Shift+ArrowRight", async () => {
    analytics.from = "2026-08-01";
    analytics.to = "2026-08-03";
    analytics.activity = {
      granularity: "day",
      series: Array.from({ length: 3 }, (_, index) => ({
        date: `2026-08-0${index + 1}`,
        sessions: 1,
        messages: 2,
        user_messages: 1,
        assistant_messages: 1,
        tool_calls: 0,
        thinking_messages: 0,
        by_agent: {},
      })),
    };
    const onRangeSelect = vi.fn();

    const component = mount(ActivityTimeline, {
      target: document.body,
      props: { onRangeSelect },
    });
    await tick();

    const bars = document.querySelectorAll<SVGRectElement>("rect.bar");
    bars[0]!.dispatchEvent(
      new KeyboardEvent("keydown", {
        key: "ArrowRight",
        shiftKey: true,
        bubbles: true,
      }),
    );
    await tick();

    expect(onRangeSelect).toHaveBeenCalledWith("2026-08-01", "2026-08-02");
    unmount(component);
  });

  it("clips a keyboard week selection to a partial leading window", async () => {
    analytics.from = "2026-08-05";
    analytics.to = "2026-08-16";
    analytics.granularity = "week";
    analytics.activity = {
      granularity: "week",
      series: [
        {
          date: "2026-08-03",
          sessions: 1,
          messages: 2,
          user_messages: 1,
          assistant_messages: 1,
          tool_calls: 0,
          thinking_messages: 0,
          by_agent: {},
        },
        {
          date: "2026-08-10",
          sessions: 1,
          messages: 2,
          user_messages: 1,
          assistant_messages: 1,
          tool_calls: 0,
          thinking_messages: 0,
          by_agent: {},
        },
      ],
    };
    const onRangeSelect = vi.fn();

    const component = mount(ActivityTimeline, {
      target: document.body,
      props: { onRangeSelect },
    });
    await tick();

    document
      .querySelector<SVGRectElement>("rect.bar")!
      .dispatchEvent(new KeyboardEvent("keydown", { key: "Enter", bubbles: true }));

    expect(onRangeSelect).toHaveBeenCalledWith("2026-08-05", "2026-08-09");
    unmount(component);
  });

  it("keeps a full-width selection visible after the range reloads", async () => {
    analytics.from = "2026-08-03";
    analytics.to = "2026-08-06";
    analytics.selectedActivityRange = {
      from: "2026-08-03",
      to: "2026-08-06",
    };
    analytics.activity = {
      granularity: "day",
      series: Array.from({ length: 4 }, (_, index) => ({
        date: `2026-08-0${index + 3}`,
        sessions: 1,
        messages: 2,
        user_messages: 1,
        assistant_messages: 1,
        tool_calls: 0,
        thinking_messages: 0,
        by_agent: {},
      })),
    };

    const component = mount(ActivityTimeline, { target: document.body });
    await tick();

    expect(document.querySelector(".activity-selection-overlay")).not.toBeNull();

    unmount(component);
  });
});
