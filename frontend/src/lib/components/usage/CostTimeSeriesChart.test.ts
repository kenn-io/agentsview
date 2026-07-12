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
});
