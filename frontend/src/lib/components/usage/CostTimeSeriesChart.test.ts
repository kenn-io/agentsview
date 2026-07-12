// @vitest-environment jsdom
import {
  afterEach,
  beforeEach,
  describe,
  expect,
  it,
} from "vitest";
import { mount, tick, unmount } from "svelte";
// @ts-ignore
import CostTimeSeriesChart from "./CostTimeSeriesChart.svelte";
import { usage } from "../../stores/usage.svelte.js";
import type {
  DailyUsageEntry,
  UsageSummaryResponse,
} from "../../api/types/usage.js";
import { projectColor } from "../../utils/projectColor.js";

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
    totalCost: 10,
    modelsUsed: ["model"],
    projectBreakdowns: [
      {
		project_key: "pl1:sha256:agentsview",
        project: "agentsview",
        inputTokens: 100,
        outputTokens: 50,
        cacheCreationTokens: 0,
        cacheReadTokens: 0,
        cost: 10,
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
      totalCost: 150,
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
        cost: 150,
      },
    ],
    modelTotals: [],
    agentTotals: [],
    branchTotals: [],
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
      savingsVsUncached: 0,
    },
  };
}

function modelDailyEntry(
  index: number,
  models: Array<{ modelName: string; cost: number }>,
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

describe("CostTimeSeriesChart", () => {
  beforeEach(() => {
    globalThis.ResizeObserver =
      ImmediateResizeObserver as typeof ResizeObserver;
    usage.summary = usageSummary();
    usage.toggles.timeSeries.groupBy = "project";
  });

  afterEach(() => {
    usage.summary = null;
    document.body.innerHTML = "";
  });

  it("keeps the rightmost date label inside the SVG viewBox", async () => {
    const component = mount(CostTimeSeriesChart, {
      target: document.body,
    });
    await tick();

    const svg = document.querySelector("svg.chart-svg");
    expect(svg).toBeTruthy();
    const viewBox = svg!.getAttribute("viewBox")!.split(" ").map(Number);
    const viewBoxRight = viewBox[2]!;

    const labels = Array.from(
      document.querySelectorAll<SVGTextElement>("text.x-label"),
    );
    const lastLabel = labels.at(-1);
    expect(lastLabel).toBeTruthy();

    const x = Number(lastLabel!.getAttribute("x"));
    const textWidthEstimate = lastLabel!.textContent!.length * 5;

    expect(x + textWidthEstimate / 2).toBeLessThanOrEqual(viewBoxRight);

    unmount(component);
  });

  it("keeps projects with the same display label as distinct series", async () => {
    usage.summary = usageSummary();
    usage.summary.daily = [dailyEntry(0)];
    usage.summary.daily[0]!.projectBreakdowns = [
      { ...usage.summary.daily[0]!.projectBreakdowns![0]!, cost: 6 },
      {
        ...usage.summary.daily[0]!.projectBreakdowns![0]!,
        project_key: "pl1:sha256:other-archive",
        cost: 4,
      },
    ];

    const component = mount(CostTimeSeriesChart, { target: document.body });
    await tick();

    expect(document.querySelectorAll("path[opacity='0.7']")).toHaveLength(2);
    expect(document.querySelectorAll(".legend-item")).toHaveLength(2);
    unmount(component);
  });

  it("renders branch legend labels, not raw tokens", async () => {
    const summary = usageSummary();
    for (const day of summary.daily) {
      day.branchBreakdowns = [
        {
          project: "agentsview",
          branch: "main",
          inputTokens: 80,
          outputTokens: 40,
          cacheCreationTokens: 0,
          cacheReadTokens: 0,
          cost: 8,
        },
        {
          project: "agentsview",
          branch: "",
          inputTokens: 20,
          outputTokens: 10,
          cacheCreationTokens: 0,
          cacheReadTokens: 0,
          cost: 2,
        },
      ];
    }
    usage.summary = summary;
    usage.toggles.timeSeries.groupBy = "branch";

    const component = mount(CostTimeSeriesChart, {
      target: document.body,
    });
    await tick();

    const legendText = Array.from(
      document.querySelectorAll(".legend-item"),
    ).map((el) => el.textContent!.trim());
    expect(legendText).toContain("agentsview/main");
    expect(legendText).toContain("agentsview/(no branch)");
    expect(document.body.textContent).not.toContain("\u001f");

    unmount(component);
  });

  it("uses distinct active model colors for paths and legend dots", async () => {
    usage.summary = usageSummary();
    usage.toggles.timeSeries.groupBy = "model";
    usage.summary.daily = [
      modelDailyEntry(0, [
        { modelName: "claude-sonnet-5", cost: 6 },
        { modelName: "claude-opus-4-8", cost: 4 },
      ]),
      modelDailyEntry(1, [
        { modelName: "claude-sonnet-5", cost: 3 },
        { modelName: "claude-opus-4-8", cost: 2 },
      ]),
    ];

    const component = mount(CostTimeSeriesChart, { target: document.body });
    await tick();

    const paths = Array.from(
      document.querySelectorAll<SVGPathElement>("path[opacity='0.7']"),
    ).map((path) => path.getAttribute("fill"));
    const pathData = Array.from(
      document.querySelectorAll<SVGPathElement>("path[opacity='0.7']"),
    ).map((path) => path.getAttribute("d"));
    const dots = Array.from(
      document.querySelectorAll<HTMLElement>(".legend-dot"),
    ).map((dot) => dot.style.background);
    expect(new Set(paths).size).toBe(2);
    expect(pathData.every((d) => d?.startsWith("M40,"))).toBe(true);
    expect(dots).toEqual(paths);
    unmount(component);
  });

  it("keeps a single rendered model series on its established color", async () => {
    usage.summary = usageSummary();
    usage.toggles.timeSeries.groupBy = "model";
    usage.summary.daily = [
      modelDailyEntry(0, [{ modelName: "single-model", cost: 6 }]),
      modelDailyEntry(1, [{ modelName: "single-model", cost: 3 }]),
    ];

    const component = mount(CostTimeSeriesChart, { target: document.body });
    await tick();

    const paths = document.querySelectorAll<SVGPathElement>(
      "path[opacity='0.7']",
    );
    expect(paths).toHaveLength(1);
    expect(paths[0]!.getAttribute("fill")).toBe(projectColor("single-model"));
    expect(document.querySelectorAll(".legend-item")).toHaveLength(0);
    unmount(component);
  });

  it("keeps rendered other-series output muted", async () => {
    usage.summary = usageSummary();
    usage.toggles.timeSeries.groupBy = "model";
    const models = Array.from({ length: 6 }, (_, index) => ({
      modelName: `model-${index}`,
      cost: 6 - index,
    }));
    usage.summary.daily = [modelDailyEntry(0, models)];

    const component = mount(CostTimeSeriesChart, { target: document.body });
    await tick();

    const paths = Array.from(
      document.querySelectorAll<SVGPathElement>("path[opacity='0.7']"),
    );
    const dots = Array.from(
      document.querySelectorAll<HTMLElement>(".legend-dot"),
    );
    expect(paths).toHaveLength(6);
    expect(dots).toHaveLength(6);
    expect(paths.at(-1)!.getAttribute("fill")).toBe("var(--text-muted)");
    expect(dots.at(-1)!.style.background).toBe("var(--text-muted)");
    unmount(component);
  });

  // Unattributable cost (e.g. imported Cursor usage) carries no branch,
  // so branch grouping must show the empty state rather than a "total"
  // series the branch attribution panel says does not exist.
  it("shows the empty state in branch mode when no cost is branch-attributable", async () => {
    const summary = usageSummary();
    for (const day of summary.daily) {
      day.branchBreakdowns = [];
    }
    usage.summary = summary;
    usage.toggles.timeSeries.groupBy = "branch";

    const component = mount(CostTimeSeriesChart, {
      target: document.body,
    });
    await tick();

    expect(document.querySelector(".empty")).toBeTruthy();
    expect(document.querySelector("svg.chart-svg")).toBeNull();
    unmount(component);
  });

  it("keeps the total fallback for non-branch groupings without breakdowns", async () => {
    const summary = usageSummary();
    for (const day of summary.daily) {
      day.projectBreakdowns = [];
    }
    usage.summary = summary;
    usage.toggles.timeSeries.groupBy = "project";

    const component = mount(CostTimeSeriesChart, {
      target: document.body,
    });
    await tick();

    expect(document.querySelector(".empty")).toBeNull();
    expect(document.querySelector("svg.chart-svg")).toBeTruthy();
    unmount(component);
  });
});
