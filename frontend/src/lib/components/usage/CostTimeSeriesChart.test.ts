// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vite-plus/test";
import { mount, tick, unmount } from "svelte";
// @ts-ignore
import CostTimeSeriesChart from "./CostTimeSeriesChart.svelte";
import { usage } from "../../stores/usage.svelte.js";
import { testMoney } from "../../test/money.js";
import type { Money } from "../../money.js";
import { settings } from "../../stores/settings.svelte.js";
import type { DbDailyUsageEntry, UsageSummaryResponse } from "../../api/generated/index";
import { usageChartColorMaps } from "../../utils/usageChartColors.js";
import { setLocale } from "../../i18n/index.js";

const OBSERVED_WIDTH = 1648;

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

function dailyEntry(index: number): DbDailyUsageEntry {
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
    modelBreakdowns: [],
    agentBreakdowns: [],
    machineBreakdowns: [],
  };
}

function usageSummary(
  daily = Array.from({ length: 15 }, (_, index) => dailyEntry(index)),
): UsageSummaryResponse {
  return {
    from: daily[0]!.date,
    to: daily.at(-1)!.date,
    projects: {},
    totals: {
      inputTokens: 1500,
      outputTokens: 750,
      cacheCreationTokens: 0,
      cacheReadTokens: 0,
      totalCost: testMoney(150),
      cacheSavings: testMoney(0),
    },
    daily,
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
): DbDailyUsageEntry {
  const entry = dailyEntry(index);
  entry.projectBreakdowns = [];
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
    usage.toggles.timeSeries.view = "smooth";
    settings.chartPalette = "agentsview";
    setLocale("en");
  });

  afterEach(() => {
    vi.restoreAllMocks();
    usage.summary = null;
    usage.selectedTimeRange = null;
    usage.excludedProjectKeys = "";
    usage.excludedAgents = "";
    usage.excludedModels = "";
    usage.mode = "cost";
    usage.setSelectedTokenTypes(["input", "cache_write", "cache_read", "output"]);
    settings.chartPalette = "agentsview";
    setLocale("en");
    document.body.innerHTML = "";
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
    usage.summary = usageSummary([dailyEntry(0)]);

    const component = mountChart();
    await tick();

    const bar = document.querySelector<SVGRectElement>("rect.cost-seg");
    expect(bar).not.toBeNull();
    expect(Number(bar!.getAttribute("width"))).toBeGreaterThan(0);
    expect(Number(bar!.getAttribute("height"))).toBeGreaterThan(0);
    expect(document.querySelector("path.lc-area-path")).toBeNull();

    unmount(component);
  });

  it("renders stacked areas without dimming their colors", async () => {
    const component = mountChart();
    await tick();

    const area = document.querySelector<SVGPathElement>("path.lc-area-path");
    expect(area).not.toBeNull();
    expect(Number(area!.getAttribute("opacity") ?? 1)).toBe(1);

    unmount(component);
  });

  it("renders a zero-usage day between populated dates", async () => {
    usage.summary = usageSummary();
    usage.summary.from = "2026-06-04";
    usage.summary.to = "2026-06-06";
    usage.summary.daily = [dailyEntry(0), dailyEntry(2)];

    const component = mountChart();
    await tick();

    const labels = Array.from(document.querySelectorAll<SVGTextElement>("text.x-label")).map(
      (label) => label.textContent?.trim(),
    );
    expect(labels).toContain("Jun 5");
    const hits = document.querySelectorAll<SVGRectElement>(".slot-hit");
    expect(hits).toHaveLength(3);
    hits[1]!.dispatchEvent(new MouseEvent("mouseenter", { bubbles: true }));
    await tick();
    const tooltip = document.querySelector(".tooltip")!;
    expect(tooltip.querySelector(".tooltip-date")?.textContent).toContain("Jun 5, 2026");
    expect(tooltip.querySelector(".tooltip-row")?.textContent).toContain("$0.00");

    unmount(component);
  });

  it("draws a zero-usage day as an empty bar slot in bars mode", async () => {
    usage.toggles.timeSeries.view = "bars";
    usage.summary = usageSummary();
    usage.summary.from = "2026-06-04";
    usage.summary.to = "2026-06-06";
    usage.summary.daily = [dailyEntry(0), dailyEntry(2)];

    const component = mountChart();
    await tick();

    const days = Array.from(document.querySelectorAll<SVGGElement>("[data-cost-bar]"));
    expect(days).toHaveLength(3);
    expect(days.map((day) => day.querySelectorAll("rect.cost-seg").length)).toEqual([1, 0, 1]);
    expect(document.querySelector("path.lc-area-path")).toBeNull();

    unmount(component);
  });

  it("draws smooth areas by default without dipping below zero or above the peak", async () => {
    usage.summary = usageSummary(
      Array.from({ length: 6 }, (_, index) => {
        const entry = dailyEntry(index);
        const cost = index % 2 === 0 ? testMoney(0) : testMoney(10);
        entry.projectBreakdowns = [{ ...entry.projectBreakdowns![0]!, cost }];
        entry.totalCost = cost;
        return entry;
      }),
    );

    const component = mountChart();
    await tick();

    const path = document.querySelector<SVGPathElement>("path.lc-area-path")!;
    const d = path.getAttribute("d")!;
    expect(d).toContain("C");
    const ys = [...d.matchAll(/(-?[\d.]+),(-?[\d.]+)/g)].map((match) => Number(match[2]));
    const peakY = Math.min(...ys);
    const baselineY = Math.max(...ys);
    // The plot spans y 8..168; a 10-dollar peak on a 10-dollar scale reaches the top.
    expect(baselineY).toBeCloseTo(168, 5);
    expect(peakY).toBeCloseTo(8, 5);
    expect(document.querySelectorAll("rect.cost-seg")).toHaveLength(0);

    unmount(component);
  });

  it("draws straight area segments in lines mode", async () => {
    usage.toggles.timeSeries.view = "lines";

    const component = mountChart();
    await tick();

    const d = document.querySelector<SVGPathElement>("path.lc-area-path")!.getAttribute("d")!;
    expect(d).not.toContain("C");
    expect(d).toContain("L");

    unmount(component);
  });

  it("switches chart style from the segmented control and remembers it", async () => {
    const setView = vi.spyOn(usage, "setTimeSeriesView");
    const component = mountChart();
    await tick();

    const group = document.querySelector<HTMLElement>(
      '[role="radiogroup"][aria-label="Chart style"]',
    )!;
    const radios = Array.from(group.querySelectorAll<HTMLButtonElement>('[role="radio"]'));
    expect(radios.map((radio) => radio.textContent?.trim())).toEqual(["Smooth", "Lines", "Bars"]);
    expect(radios[0]!.getAttribute("aria-checked")).toBe("true");

    radios[2]!.click();
    await tick();

    expect(setView).toHaveBeenCalledWith("bars");
    expect(usage.toggles.timeSeries.view).toBe("bars");
    expect(document.querySelectorAll("rect.cost-seg").length).toBeGreaterThan(0);
    expect(document.querySelector("path.lc-area-path")).toBeNull();

    unmount(component);
  });

  it("keeps the no-data state when the usage response has no daily entries", async () => {
    usage.summary = usageSummary();
    usage.summary.daily = [];

    const component = mountChart();
    await tick();

    expect(document.querySelector(".empty")?.textContent).toContain("No data for this period");
    expect(document.querySelector(".chart-svg")).toBeNull();

    unmount(component);
  });

  it("marks a selected range and exposes a clear-selection action", async () => {
    const component = mountChart();
    await tick();

    expect(document.querySelector(".range-selection")).toBeNull();
    usage.selectedTimeRange = { from: "2026-06-07", to: "2026-06-10" };
    await tick();
    const clear = [...document.querySelectorAll<HTMLButtonElement>("button")].find(
      (button) => button.textContent?.trim() === "Clear selection",
    );
    expect(clear).toBeDefined();
    expect(document.querySelector(".range-selection")).not.toBeNull();
    const pressed = Array.from(document.querySelectorAll(".slot-hit")).map((hit) =>
      hit.getAttribute("aria-pressed"),
    );
    expect(pressed.filter((value) => value === "true")).toHaveLength(4);
    expect(pressed[3]).toBe("true");
    expect(pressed[6]).toBe("true");

    usage.selectedTimeRange = null;
    await tick();
    expect(document.querySelector(".range-selection")).toBeNull();

    unmount(component);
  });

  it("drags across days to select a date range", async () => {
    const setTimeRange = vi.spyOn(usage, "setTimeRange").mockImplementation(() => {});
    const component = mountChart();
    await tick();

    const hits = document.querySelectorAll<SVGRectElement>(".slot-hit");
    hits[3]!.dispatchEvent(new MouseEvent("pointerdown", { bubbles: true, button: 0 }));
    await tick();
    const endX = Number(hits[6]!.getAttribute("x"));
    const endWidth = Number(hits[6]!.getAttribute("width"));
    document
      .querySelector(".chart-body")!
      .dispatchEvent(
        new MouseEvent("pointermove", { bubbles: true, clientX: endX + endWidth / 2 }),
      );
    await tick();
    expect(document.querySelector(".range-selection")).not.toBeNull();
    window.dispatchEvent(new MouseEvent("pointerup", { bubbles: true }));
    await tick();

    expect(setTimeRange).toHaveBeenCalledExactlyOnceWith("2026-06-07", "2026-06-10");
    unmount(component);
  });

  it("ignores a single-day click", async () => {
    const setTimeRange = vi.spyOn(usage, "setTimeRange").mockImplementation(() => {});
    const component = mountChart();
    await tick();

    const hit = document.querySelectorAll<SVGRectElement>(".slot-hit")[2]!;
    hit.dispatchEvent(new MouseEvent("pointerdown", { bubbles: true, button: 0 }));
    window.dispatchEvent(new MouseEvent("pointerup", { bubbles: true }));
    await tick();

    expect(setTimeRange).not.toHaveBeenCalled();
    unmount(component);
  });

  it("selects and clears a range from the keyboard", async () => {
    const setTimeRange = vi.spyOn(usage, "setTimeRange").mockImplementation(() => {});
    const clearTimeRange = vi.spyOn(usage, "clearTimeRange").mockImplementation(() => {});
    const component = mountChart();
    await tick();

    const hits = document.querySelectorAll<SVGRectElement>(".slot-hit");
    hits[3]!.dispatchEvent(new KeyboardEvent("keydown", { key: "Enter", bubbles: true }));
    hits[3]!.dispatchEvent(
      new KeyboardEvent("keydown", { key: "ArrowRight", shiftKey: true, bubbles: true }),
    );
    expect(setTimeRange).toHaveBeenCalledExactlyOnceWith("2026-06-07", "2026-06-08");

    usage.selectedTimeRange = { from: "2026-06-07", to: "2026-06-08" };
    await tick();
    hits[4]!.dispatchEvent(new KeyboardEvent("keydown", { key: "Escape", bubbles: true }));
    expect(clearTimeRange).toHaveBeenCalledOnce();

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
    // Output tokens peak at 50 a day; a four-step scale tops out at 60.
    expect(labels.at(-1)).toBe("60");
    expect(labels).not.toContain("150");

    unmount(component);
  });

  it("keeps projects with the same display label as distinct series", async () => {
    usage.summary = usageSummary([dailyEntry(0)]);
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

    expect(document.querySelectorAll(".chart-svg rect.cost-seg")).toHaveLength(2);
    expect(document.querySelectorAll(".legend-item")).toHaveLength(2);
    unmount(component);
  });

  it("uses distinct active model colors for paths and legend dots", async () => {
    usage.toggles.timeSeries.groupBy = "model";
    usage.summary = usageSummary([
      modelDailyEntry(0, [
        { modelName: "claude-sonnet-5", cost: testMoney(6) },
        { modelName: "claude-opus-4-8", cost: testMoney(4) },
      ]),
      modelDailyEntry(1, [
        { modelName: "claude-sonnet-5", cost: testMoney(3) },
        { modelName: "claude-opus-4-8", cost: testMoney(2) },
      ]),
    ]);

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
    usage.toggles.timeSeries.groupBy = "model";
    usage.summary = usageSummary([
      modelDailyEntry(0, [{ modelName: "single-model", cost: testMoney(6) }]),
      modelDailyEntry(1, [{ modelName: "single-model", cost: testMoney(3) }]),
    ]);

    const component = mountChart();
    await tick();

    const paths = document.querySelectorAll<SVGPathElement>("path.lc-area-path");
    expect(paths).toHaveLength(1);
    expect(paths[0]!.getAttribute("fill")).toBe("var(--accent-blue)");
    expect(document.querySelectorAll(".legend-item")).toHaveLength(0);
    unmount(component);
  });

  it("renders ten named series before rolling the rest into Other", async () => {
    usage.toggles.timeSeries.groupBy = "model";
    const models = Array.from({ length: 11 }, (_, index) => ({
      modelName: `model-${index}`,
      cost: testMoney(11 - index),
    }));
    usage.summary = usageSummary([modelDailyEntry(0, models)]);

    const component = mountChart();
    await tick();

    const marks = Array.from(document.querySelectorAll<SVGElement>(".chart-svg rect.cost-seg"));
    const dots = Array.from(document.querySelectorAll<HTMLElement>(".legend-dot"));
    expect(marks).toHaveLength(11);
    expect(dots).toHaveLength(11);
    expect(marks.at(-1)!.getAttribute("fill")).toBe("var(--text-muted)");
    expect(dots.at(-1)!.style.background).toBe("var(--text-muted)");
    unmount(component);
  });

  it("shows hovered series in descending value order", async () => {
    usage.toggles.timeSeries.groupBy = "model";
    usage.summary = usageSummary([
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
    ]);

    const component = mountChart();
    await tick();
    document
      .querySelector(".slot-hit")!
      .dispatchEvent(new MouseEvent("mouseenter", { bubbles: true }));
    await tick();

    const tooltip = document.querySelector(".tooltip")!;
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
    usage.toggles.timeSeries.groupBy = "model";
    usage.summary = usageSummary([
      modelDailyEntry(0, [{ modelName: "model", cost: testMoney(1) }]),
      modelDailyEntry(1, [{ modelName: "model", cost: testMoney(2) }]),
      modelDailyEntry(2, [{ modelName: "model", cost: testMoney(3) }]),
    ]);

    const component = mountChart();
    await tick();
    const hit = document.querySelectorAll(".slot-hit")[2]!;
    hit.dispatchEvent(new MouseEvent("mouseenter", { bubbles: true }));
    await tick();

    expect(document.querySelector(".tooltip .tooltip-date")?.textContent).toContain("Jun 6, 2026");
    hit.dispatchEvent(new MouseEvent("mouseleave", { bubbles: true }));
    await tick();
    expect(document.querySelector(".tooltip")).toBeNull();
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

  it("hides and restores model series in the cached chart range", async () => {
    usage.toggles.timeSeries.groupBy = "model";
    usage.summary!.daily = [
      modelDailyEntry(0, [
        { modelName: "model-alpha", cost: testMoney(3) },
        { modelName: "model-bravo", cost: testMoney(2) },
      ]),
      modelDailyEntry(1, [
        { modelName: "model-alpha", cost: testMoney(3) },
        { modelName: "model-bravo", cost: testMoney(2) },
      ]),
    ];
    usage.excludedModels = "model-alpha";
    const component = mountChart();
    await tick();
    try {
      expect(document.querySelectorAll("path.lc-area-path")).toHaveLength(1);
      usage.excludedModels = "model-alpha,model-bravo";
      await tick();
      expect(document.querySelectorAll("path.lc-area-path")).toHaveLength(0);
      usage.excludedModels = "";
      await tick();
      expect(document.querySelectorAll("path.lc-area-path")).toHaveLength(2);
    } finally {
      await unmount(component);
    }
  });

  it("uses aggregate-cost-ranked Matplotlib colors for model paths and legend dots", async () => {
    settings.chartPalette = "matplotlib";
    usage.toggles.timeSeries.groupBy = "model";
    usage.summary = usageSummary([
      modelDailyEntry(0, [
        { modelName: "gpt-5.6-sol", cost: testMoney(8) },
        { modelName: "claude-opus-5", cost: testMoney(4) },
      ]),
      modelDailyEntry(1, [
        { modelName: "gpt-5.6-sol", cost: testMoney(3) },
        { modelName: "claude-opus-5", cost: testMoney(2) },
      ]),
    ]);

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
