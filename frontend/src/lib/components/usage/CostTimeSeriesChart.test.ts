// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vite-plus/test";
import { mount, tick, unmount } from "svelte";
// @ts-ignore
import CostTimeSeriesChart from "./CostTimeSeriesChart.svelte";
import { usage } from "../../stores/usage.svelte.js";
import { testMoney } from "../../test/money.js";
import type { Money } from "../../money.js";
import { settings } from "../../stores/settings.svelte.js";
import type { DailyUsageEntry, UsageSummaryResponse } from "../../api/types/usage.js";
import { usageChartColorMaps } from "../../utils/usageChartColors.js";
import { setLocale } from "../../i18n/index.js";

const OBSERVED_WIDTH = 1648;
const originalClientWidth = Object.getOwnPropertyDescriptor(HTMLElement.prototype, "clientWidth");

class ImmediateResizeObserver implements ResizeObserver {
  private readonly callback: ResizeObserverCallback;

  constructor(callback: ResizeObserverCallback) {
    this.callback = callback;
  }

  observe(target: Element): void {
    this.callback(
      [
        {
          target,
          contentRect: {
            width: OBSERVED_WIDTH,
            height: 200,
            x: 0,
            y: 0,
            top: 0,
            right: OBSERVED_WIDTH,
            bottom: 200,
            left: 0,
            toJSON: () => ({}),
          },
        } as ResizeObserverEntry,
      ],
      this,
    );
  }

  unobserve(): void {}
  disconnect(): void {}
}

function dailyEntry(index: number): DailyUsageEntry {
  const date = new Date("2026-06-04T00:00:00");
  date.setDate(date.getDate() + index);
  const isoDate = date.toISOString().slice(0, 10);

  return {
    date: isoDate,
    inputTokens: 100,
    outputTokens: 50,
    cacheCreationTokens: 0,
    cacheReadTokens: 0,
    totalCost: testMoney(10),
    modelsUsed: ["model"],
    projectBreakdowns: [
      {
        project_key: "pl1:sha256:agentsview",
        project: "agentsview",
        inputTokens: 100,
        outputTokens: 50,
        cacheCreationTokens: 0,
        cacheReadTokens: 0,
        cost: testMoney(10),
      },
    ],
  };
}

function usageSummary(): UsageSummaryResponse {
  return {
    from: "2026-06-04",
    to: "2026-06-18",
    totals: {
      inputTokens: 1500,
      outputTokens: 750,
      cacheCreationTokens: 0,
      cacheReadTokens: 0,
      totalCost: testMoney(150),
    },
    daily: Array.from({ length: 15 }, (_, i) => dailyEntry(i)),
    projectTotals: [
      {
        project_key: "pl1:sha256:agentsview",
        project: "agentsview",
        inputTokens: 1500,
        outputTokens: 750,
        cacheCreationTokens: 0,
        cacheReadTokens: 0,
        cost: testMoney(150),
      },
    ],
    modelTotals: [],
    agentTotals: [],
    sessionCounts: {
      total: 15,
      byProject: { agentsview: 15 },
      byAgent: {},
    },
    cacheStats: {
      cacheReadTokens: 0,
      cacheCreationTokens: 0,
      uncachedInputTokens: 1500,
      outputTokens: 750,
      hitRate: 0,
      savingsVsUncached: testMoney(0),
    },
  };
}

function modelDailyEntry(
  index: number,
  models: Array<{ modelName: string; cost: Money }>,
): DailyUsageEntry {
  const entry = dailyEntry(index);
  entry.projectBreakdowns = undefined;
  entry.modelBreakdowns = models.map(({ modelName, cost }) => ({
    modelName,
    inputTokens: 60,
    outputTokens: 30,
    cacheCreationTokens: 0,
    cacheReadTokens: 0,
    cost,
  }));
  return entry;
}

function mountChart() {
  const groupBy = usage.toggles.timeSeries.groupBy;
  return mount(CostTimeSeriesChart, {
    target: document.body,
    props: {
      colorMap: usageChartColorMaps(usage.summary, settings.chartPalette)[groupBy],
    },
  });
}

describe("CostTimeSeriesChart", () => {
  beforeEach(() => {
    globalThis.ResizeObserver = ImmediateResizeObserver as typeof ResizeObserver;
    usage.summary = usageSummary();
    usage.selectedTimeRange = null;
    usage.toggles.timeSeries.groupBy = "project";
    settings.chartPalette = "agentsview";
    setLocale("en");
  });

  afterEach(() => {
    vi.restoreAllMocks();
    usage.summary = null;
    usage.selectedTimeRange = null;
    usage.excludedProjectKeys = "";
    usage.excludedAgents = "";
    usage.selectedModels = "";
    usage.mode = "cost";
    usage.setSelectedTokenTypes(["input", "cache_write", "cache_read", "output"]);
    settings.chartPalette = "agentsview";
    setLocale("en");
    document.body.innerHTML = "";
    if (originalClientWidth) {
      Object.defineProperty(HTMLElement.prototype, "clientWidth", originalClientWidth);
    }
  });

  it("renders localized French currency labels", async () => {
    setLocale("fr");
    const component = mountChart();
    await tick();

    const labels = Array.from(document.querySelectorAll<SVGTextElement>("text.y-label"));
    expect(labels.some((label) => label.textContent?.includes("$US"))).toBe(true);

    unmount(component);
  });

  it("renders the first and last date labels", async () => {
    const component = mountChart();
    await tick();

    const svg = document.querySelector("svg.chart-svg");
    expect(svg).toBeTruthy();
    const labels = Array.from(document.querySelectorAll<SVGTextElement>("text.x-label"));
    expect(labels[0]?.textContent).toContain("Jun 4");
    expect(labels.at(-1)?.textContent).toContain("Jun 18");

    unmount(component);
  });

  it("renders a visible stacked bar for a one-day range", async () => {
    usage.summary = usageSummary();
    usage.summary.daily = [dailyEntry(0)];

    const component = mountChart();
    await tick();

    const bar = document.querySelector<SVGRectElement>("rect.lc-bar");
    expect(bar).not.toBeNull();
    expect(Number(bar!.getAttribute("width"))).toBeGreaterThan(0);
    expect(Number(bar!.getAttribute("height"))).toBeGreaterThan(0);
    expect(Number(bar!.getAttribute("opacity") ?? 1)).toBe(1);

    unmount(component);
  });

  it("renders stacked area colors without dimming them", async () => {
    const component = mountChart();
    await tick();

    const area = document.querySelector<SVGPathElement>("path.lc-area-path");
    expect(area).not.toBeNull();
    expect(Number(area!.getAttribute("opacity") ?? 1)).toBe(1);

    unmount(component);
  });

  it("brushes a date range and exposes a clear-selection action", async () => {
    const component = mountChart();
    await tick();
    await tick();

    await vi.waitFor(() => {
      expect(document.querySelector(".lc-brush-context")).not.toBeNull();
    });
    const brush = document.querySelector<HTMLElement>(".lc-brush-context");
    expect(brush).not.toBeNull();
    usage.selectedTimeRange = { from: "2026-06-07", to: "2026-06-10" };
    await tick();
    const clear = [...document.querySelectorAll<HTMLButtonElement>("button")].find(
      (button) => button.textContent?.trim() === "Clear selection",
    );
    expect(clear).toBeDefined();
    await vi.waitFor(() => {
      expect(document.querySelector(".usage-brush-range")).not.toBeNull();
    });

    usage.selectedTimeRange = null;
    await tick();
    await vi.waitFor(() => {
      expect(document.querySelector(".usage-brush-range")).toBeNull();
    });

    unmount(component);
  });

  it("commits a pointer brush through the chart boundary", async () => {
    Object.defineProperty(HTMLElement.prototype, "clientWidth", {
      configurable: true,
      get: () => OBSERVED_WIDTH,
    });
    const setTimeRange = vi.spyOn(usage, "setTimeRange").mockImplementation(() => {});
    const component = mountChart();
    await tick();
    await tick();

    await vi.waitFor(() => {
      expect(document.querySelector(".lc-brush-context")).not.toBeNull();
    });
    const brush = document.querySelector<HTMLElement>(".lc-brush-context")!;
    expect(Number.parseFloat(brush.style.width)).toBeGreaterThan(0);
    vi.spyOn(brush, "getBoundingClientRect").mockReturnValue({
      x: 0,
      y: 0,
      left: 0,
      top: 0,
      right: OBSERVED_WIDTH,
      bottom: 180,
      width: OBSERVED_WIDTH,
      height: 180,
      toJSON: () => ({}),
    });

    brush.dispatchEvent(
      new MouseEvent("pointerdown", { bubbles: true, clientX: 390, clientY: 60 }),
    );
    window.dispatchEvent(
      new MouseEvent("pointermove", { bubbles: true, clientX: 730, clientY: 60 }),
    );
    await tick();
    expect(document.querySelector(".usage-brush-range")).not.toBeNull();
    brush.dispatchEvent(new MouseEvent("pointerup", { bubbles: true, clientX: 730, clientY: 60 }));
    await tick();

    expect(setTimeRange).toHaveBeenCalledOnce();
    expect(setTimeRange.mock.calls[0]?.[0]).not.toBe(setTimeRange.mock.calls[0]?.[1]);
    unmount(component);
  });

  it("commits a date range from keyboard-accessible controls", async () => {
    const setTimeRange = vi.spyOn(usage, "setTimeRange").mockImplementation(() => {});
    const component = mountChart();
    await tick();

    const form = document.querySelector<HTMLFormElement>("form.keyboard-range")!;
    const from = form.elements.namedItem("from") as HTMLInputElement;
    const to = form.elements.namedItem("to") as HTMLInputElement;
    from.value = "2026-06-07";
    to.value = "2026-06-10";
    form.dispatchEvent(new SubmitEvent("submit", { bubbles: true, cancelable: true }));

    expect(setTimeRange).toHaveBeenCalledExactlyOnceWith("2026-06-07", "2026-06-10");
    unmount(component);
  });

  it("scales token series from only the selected token types", async () => {
    usage.mode = "token";
    usage.setSelectedTokenTypes(["output"]);
    const component = mountChart();
    await tick();

    const labels = Array.from(document.querySelectorAll<SVGTextElement>("text.y-label")).map(
      (label) => label.textContent?.trim(),
    );
    expect(labels).toContain("50");
    expect(labels).not.toContain("150");

    unmount(component);
  });

  it("keeps projects with the same display label as distinct series", async () => {
    usage.summary = usageSummary();
    usage.summary.daily = [dailyEntry(0)];
    usage.summary.daily[0]!.projectBreakdowns = [
      { ...usage.summary.daily[0]!.projectBreakdowns![0]!, cost: testMoney(6) },
      {
        ...usage.summary.daily[0]!.projectBreakdowns![0]!,
        project_key: "pl1:sha256:other-archive",
        cost: testMoney(4),
      },
    ];

    const component = mountChart();
    await tick();

    expect(document.querySelectorAll(".chart-svg rect.lc-bar")).toHaveLength(2);
    expect(document.querySelectorAll(".legend-item")).toHaveLength(2);
    unmount(component);
  });

  it("uses distinct active model colors for paths and legend dots", async () => {
    usage.summary = usageSummary();
    usage.toggles.timeSeries.groupBy = "model";
    usage.summary.daily = [
      modelDailyEntry(0, [
        { modelName: "claude-sonnet-5", cost: testMoney(6) },
        { modelName: "claude-opus-4-8", cost: testMoney(4) },
      ]),
      modelDailyEntry(1, [
        { modelName: "claude-sonnet-5", cost: testMoney(3) },
        { modelName: "claude-opus-4-8", cost: testMoney(2) },
      ]),
    ];

    const component = mountChart();
    await tick();

    const paths = Array.from(document.querySelectorAll<SVGPathElement>("path.lc-area-path")).map(
      (path) => path.getAttribute("fill"),
    );
    const dots = Array.from(document.querySelectorAll<HTMLElement>(".legend-dot")).map(
      (dot) => dot.style.background,
    );
    expect(new Set(paths).size).toBe(2);
    expect(dots).toEqual(paths);
    unmount(component);
  });

  it("assigns the first usage color to a single rendered model series", async () => {
    usage.summary = usageSummary();
    usage.toggles.timeSeries.groupBy = "model";
    usage.summary.daily = [
      modelDailyEntry(0, [{ modelName: "single-model", cost: testMoney(6) }]),
      modelDailyEntry(1, [{ modelName: "single-model", cost: testMoney(3) }]),
    ];

    const component = mountChart();
    await tick();

    const paths = document.querySelectorAll<SVGPathElement>("path.lc-area-path");
    expect(paths).toHaveLength(1);
    expect(paths[0]!.getAttribute("fill")).toBe("var(--accent-blue)");
    expect(document.querySelectorAll(".legend-item")).toHaveLength(0);
    unmount(component);
  });

  it("renders ten named series before rolling the rest into Other", async () => {
    usage.summary = usageSummary();
    usage.toggles.timeSeries.groupBy = "model";
    const models = Array.from({ length: 11 }, (_, index) => ({
      modelName: `model-${index}`,
      cost: testMoney(11 - index),
    }));
    usage.summary.daily = [modelDailyEntry(0, models)];

    const component = mountChart();
    await tick();

    const marks = Array.from(document.querySelectorAll<SVGElement>(".chart-svg rect.lc-bar"));
    const dots = Array.from(document.querySelectorAll<HTMLElement>(".legend-dot"));
    expect(marks).toHaveLength(11);
    expect(dots).toHaveLength(11);
    expect(marks.at(-1)!.getAttribute("fill")).toBe("var(--text-muted)");
    expect(dots.at(-1)!.style.background).toBe("var(--text-muted)");
    unmount(component);
  });

  it("shows hovered series in descending value order", async () => {
    usage.summary = usageSummary();
    usage.toggles.timeSeries.groupBy = "model";
    usage.summary.daily = [
      modelDailyEntry(0, [
        { modelName: "small", cost: testMoney(1) },
        { modelName: "large", cost: testMoney(9) },
        { modelName: "medium", cost: testMoney(4) },
      ]),
      modelDailyEntry(1, [
        { modelName: "small", cost: testMoney(2) },
        { modelName: "large", cost: testMoney(3) },
        { modelName: "medium", cost: testMoney(8) },
      ]),
    ];

    const component = mountChart();
    await tick();
    const target = document.querySelector<HTMLElement>(".lc-tooltip-context")!;
    target.dispatchEvent(
      new MouseEvent("pointerenter", {
        bubbles: true,
        clientX: 0,
        clientY: 0,
      }),
    );
    target.dispatchEvent(
      new MouseEvent("pointermove", {
        bubbles: true,
        clientX: 0,
        clientY: 0,
      }),
    );
    await tick();

    const tooltip = document.querySelector(".usage-series-tooltip")!;
    expect(tooltip).toBeTruthy();
    expect(tooltip.querySelector(".tooltip-date")?.textContent).toContain("Jun 4, 2026");
    const rows = Array.from(tooltip.querySelectorAll(".tooltip-row"));
    expect(rows).toHaveLength(3);
    expect(rows[0]!.textContent).toContain("large");
    expect(rows[0]!.textContent).toContain("$9.00");
    expect(rows[1]!.textContent).toContain("medium");
    expect(rows[2]!.textContent).toContain("small");

    unmount(component);
  });

  it("shows the hovered non-first date", async () => {
    Object.defineProperty(HTMLElement.prototype, "clientWidth", {
      configurable: true,
      get: () => OBSERVED_WIDTH,
    });
    usage.summary = usageSummary();
    usage.toggles.timeSeries.groupBy = "model";
    usage.summary.daily = [
      modelDailyEntry(0, [{ modelName: "model", cost: testMoney(1) }]),
      modelDailyEntry(1, [{ modelName: "model", cost: testMoney(2) }]),
      modelDailyEntry(2, [{ modelName: "model", cost: testMoney(3) }]),
    ];

    const component = mountChart();
    await tick();
    const target = document.querySelector<HTMLElement>(".lc-tooltip-context")!;
    Object.defineProperty(target, "offsetWidth", {
      configurable: true,
      value: OBSERVED_WIDTH,
    });
    Object.defineProperty(target, "offsetHeight", {
      configurable: true,
      value: 180,
    });
    vi.spyOn(target, "getBoundingClientRect").mockReturnValue({
      x: 0,
      y: 0,
      left: 0,
      top: 0,
      right: OBSERVED_WIDTH,
      bottom: 180,
      width: OBSERVED_WIDTH,
      height: 180,
      toJSON: () => ({}),
    });
    target.dispatchEvent(
      new MouseEvent("pointerenter", {
        bubbles: true,
        clientX: OBSERVED_WIDTH - 30,
        clientY: 40,
      }),
    );
    target.dispatchEvent(
      new MouseEvent("pointermove", {
        bubbles: true,
        clientX: OBSERVED_WIDTH - 30,
        clientY: 40,
      }),
    );
    await tick();

    expect(document.querySelector(".usage-series-tooltip .tooltip-date")?.textContent).toContain(
      "Jun 6, 2026",
    );
    unmount(component);
  });

  it("does not restore the unfiltered total when every project is excluded", async () => {
    usage.summary = usageSummary();
    usage.selectedTimeRange = { from: "2026-06-04", to: "2026-06-18" };
    usage.excludedProjectKeys = "pl1:sha256:agentsview";

    const component = mountChart();
    await tick();

    expect(document.querySelector(".empty")).toBeTruthy();
    expect(document.querySelectorAll(".chart-svg path.lc-area-path")).toHaveLength(0);
    unmount(component);
  });

  it("uses aggregate-cost-ranked Matplotlib colors for model paths and legend dots", async () => {
    settings.chartPalette = "matplotlib";
    usage.summary = usageSummary();
    usage.toggles.timeSeries.groupBy = "model";
    usage.summary.daily = [
      modelDailyEntry(0, [
        { modelName: "gpt-5.6-sol", cost: testMoney(8) },
        { modelName: "claude-opus-5", cost: testMoney(4) },
      ]),
      modelDailyEntry(1, [
        { modelName: "gpt-5.6-sol", cost: testMoney(3) },
        { modelName: "claude-opus-5", cost: testMoney(2) },
      ]),
    ];

    const component = mountChart();
    await tick();

    const paths = Array.from(document.querySelectorAll<SVGPathElement>("path.lc-area-path")).map(
      (path) => path.getAttribute("fill"),
    );
    const dots = Array.from(document.querySelectorAll<HTMLElement>(".legend-dot")).map(
      (dot) => dot.style.background,
    );
    expect(paths).toEqual(["#1f77b4", "#ff7f0e"]);
    expect(dots).toEqual(["rgb(31, 119, 180)", "rgb(255, 127, 14)"]);
    unmount(component);
  });
});
