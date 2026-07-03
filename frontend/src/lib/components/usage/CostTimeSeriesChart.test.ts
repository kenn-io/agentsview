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
});
