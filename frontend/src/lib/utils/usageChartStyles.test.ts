import { describe, expect, it } from "vite-plus/test";
import type { UsageSummaryResponse } from "../api/generated/index";
import { testMoney } from "../test/money.js";
import { usageChartStyleMaps } from "./usageChartStyles.js";
import { calmSeriesColor } from "./calmSeriesColor.js";

function oklab(hex: string): [number, number, number] {
  const value = Number.parseInt(hex.slice(1), 16);
  const [r, g, b] = [16, 8, 0].map((shift) => {
    const channel = ((value >> shift) & 0xff) / 255;
    return channel <= 0.04045 ? channel / 12.92 : ((channel + 0.055) / 1.055) ** 2.4;
  }) as [number, number, number];
  const l = Math.cbrt(0.4122214708 * r + 0.5363325363 * g + 0.0514459929 * b);
  const m = Math.cbrt(0.2119034982 * r + 0.6806995451 * g + 0.1073969566 * b);
  const s = Math.cbrt(0.0883024619 * r + 0.2817188376 * g + 0.6299787005 * b);
  return [
    0.2104542553 * l + 0.793617785 * m - 0.0040720468 * s,
    1.9779984951 * l - 2.428592205 * m + 0.4505937099 * s,
    0.0259040371 * l + 0.7827717662 * m - 0.808675766 * s,
  ];
}

function oklabDistance(left: string, right: string): number {
  const [a, b] = [oklab(left), oklab(right)];
  return Math.hypot(a[0] - b[0], a[1] - b[1], a[2] - b[2]);
}

function tenModelSummary(
  models = [
    "model-alpha",
    "model-bravo",
    "model-charlie",
    "model-delta",
    "model-echo",
    "model-foxtrot",
    "model-golf",
    "model-hotel",
    "model-india",
    "model-zulu",
  ],
): UsageSummaryResponse {
  const modelTotals = models.map((model, index) => ({
    model,
    inputTokens: 10,
    outputTokens: 5,
    cacheCreationTokens: 0,
    cacheReadTokens: 0,
    cost: testMoney(index + 1),
  }));
  return {
    from: "2026-07-01",
    to: "2026-07-01",
    projects: {},
    totals: {
      inputTokens: 100,
      outputTokens: 50,
      cacheCreationTokens: 0,
      cacheReadTokens: 0,
      totalCost: testMoney(55),
      cacheSavings: testMoney(0),
    },
    daily: [
      {
        date: "2026-07-01",
        inputTokens: 100,
        outputTokens: 50,
        cacheCreationTokens: 0,
        cacheReadTokens: 0,
        totalCost: testMoney(55),
        modelsUsed: models,
        modelBreakdowns: models.map((modelName, index) => ({
          modelName,
          inputTokens: 10,
          outputTokens: 5,
          cacheCreationTokens: 0,
          cacheReadTokens: 0,
          cost: testMoney(index + 1),
        })),
        projectBreakdowns: [],
        agentBreakdowns: [],
        machineBreakdowns: [],
      },
    ],
    projectTotals: [],
    modelTotals,
    agentTotals: [],
    sessionCounts: { total: 10, byProject: {}, byAgent: {} },
    cacheStats: {
      cacheReadTokens: 0,
      cacheCreationTokens: 0,
      uncachedInputTokens: 100,
      outputTokens: 50,
      hitRate: 0,
      savingsVsUncached: testMoney(0),
    },
  };
}

describe("usageChartStyleMaps", () => {
  it("assigns calmed Matplotlib colors by aggregate cost descending", () => {
    const styles = usageChartStyleMaps(tenModelSummary(), "matplotlib").model;

    expect(styles.size).toBe(10);
    // Matplotlib #1f77b4, #aec7e8, and #c5b0d5 with lower chroma.
    expect(styles.get("model-zulu")?.color).toBe("#3977a7");
    expect(styles.get("model-india")?.color).toBe("#b3c7e1");
    expect(styles.get("model-alpha")?.color).toBe("#c3b2d0");
  });

  it("keeps exact Matplotlib colors when calming is off", () => {
    const styles = usageChartStyleMaps(tenModelSummary(), "matplotlib", { calm: false }).model;

    expect(styles.get("model-zulu")?.color).toBe("#1f77b4");
    expect(styles.get("model-india")?.color).toBe("#aec7e8");
  });

  it("calms theme variables through the resolver and keeps unresolved ones", () => {
    const themed = usageChartStyleMaps(tenModelSummary(), "agentsview", {
      resolveColor: (color) => (color === "var(--accent-blue)" ? "#3b82f6" : color),
    }).model;
    const colors = [...themed.values()].map((style) => style.color);

    expect(colors).toContain("#4e85e0");
    expect(colors).not.toContain("var(--accent-blue)");
    expect(colors).toContain("var(--accent-purple)");
  });

  it("keeps calmed light teal and green series apart", () => {
    const light: Record<string, string> = {
      "var(--accent-teal)": "#0d9488",
      "var(--accent-green)": "#059669",
      "var(--usage-series-teal)": "#20acb3",
    };
    const resolveColor = (color: string) => light[color] ?? color;
    // Cost order puts the fourth palette color (teal) on model-golf and the
    // sixth (green) on model-echo.
    const separated = usageChartStyleMaps(tenModelSummary(), "agentsview", { resolveColor }).model;
    const shared = usageChartStyleMaps(tenModelSummary(), "agentsview", {
      resolveColor: (color) => (color === "var(--usage-series-teal)" ? color : resolveColor(color)),
    }).model;

    const green = separated.get("model-echo")!.color;
    const sharedTeal = shared.get("model-golf")!.color;
    expect(oklabDistance(sharedTeal, green)).toBeLessThan(0.08);
    expect(oklabDistance(separated.get("model-golf")!.color, green)).toBeGreaterThan(0.1);
  });

  it("keeps calmed light amber and orange series apart", () => {
    const light: Record<string, string> = {
      "var(--accent-amber)": "#d97706",
      "var(--accent-orange)": "#e09040",
      "var(--usage-series-amber)": "#a48b08",
    };
    const resolveColor = (color: string) => light[color] ?? color;
    const seriesFrom = (styles: ReadonlyMap<string, { color: string }>, hex: string) =>
      [...styles.values()].map((style) => style.color).find((c) => c === calmSeriesColor(hex));

    const separated = usageChartStyleMaps(tenModelSummary(), "agentsview", { resolveColor }).model;
    const shared = usageChartStyleMaps(tenModelSummary(), "agentsview", {
      resolveColor: (color) =>
        color === "var(--usage-series-amber)" ? color : resolveColor(color),
    }).model;

    const orange = seriesFrom(separated, "#e09040")!;
    const gold = seriesFrom(separated, "#a48b08")!;
    const sharedAmber = seriesFrom(shared, "#d97706")!;
    expect(orange).toBeDefined();
    expect(gold).toBeDefined();
    expect(sharedAmber).toBeDefined();
    expect(oklabDistance(gold, orange)).toBeGreaterThan(oklabDistance(sharedAmber, orange) * 1.5);
  });

  it("separates the closest dark pastel series more than chroma calming did", () => {
    // Dark accents from the shipped theme, in palette order.
    const dark: Record<string, string> = {
      "var(--accent-blue)": "#60a5fa",
      "var(--accent-purple)": "#a78bfa",
      "var(--accent-amber)": "#fbbf24",
      "var(--usage-series-teal)": "#2dd4bf",
      "var(--accent-teal)": "#2dd4bf",
      "var(--accent-rose)": "#fb7185",
      "var(--accent-green)": "#4ade80",
      "var(--accent-indigo)": "#818cf8",
      "var(--accent-orange)": "#f0a050",
      "var(--accent-sky)": "#38bdf8",
      "var(--accent-pink)": "#f472b6",
    };
    const resolveColor = (color: string) => dark[color] ?? color;
    const closestPair = (styles: ReadonlyMap<string, { color: string }>) => {
      const colors = [...styles.values()].map((style) => style.color);
      let min = Infinity;
      for (let i = 0; i < colors.length; i++) {
        for (let j = i + 1; j < colors.length; j++) {
          min = Math.min(min, oklabDistance(colors[i]!, colors[j]!));
        }
      }
      return min;
    };

    const calmedOnly = usageChartStyleMaps(tenModelSummary(), "agentsview", { resolveColor }).model;
    const darkStyles = usageChartStyleMaps(tenModelSummary(), "agentsview", {
      resolveColor,
      dark: true,
    }).model;

    expect(closestPair(calmedOnly)).toBeLessThan(0.05);
    expect(closestPair(darkStyles)).toBeGreaterThan(0.06);
  });

  it("keeps the color-blind safe palette exact", () => {
    const calmed = usageChartStyleMaps(tenModelSummary(), "colorblind").model;
    const exact = usageChartStyleMaps(tenModelSummary(), "colorblind", { calm: false }).model;

    expect([...calmed.values()]).toEqual([...exact.values()]);
  });

  it("keeps Matplotlib colors distinct when a filter exposes more models", () => {
    const models = Array.from({ length: 11 }, (_, index) => `model-${index}`);
    const styles = usageChartStyleMaps(tenModelSummary(models), "matplotlib").model;

    expect(new Set([...styles.values()].map((style) => style.color)).size).toBe(11);
  });

  it("hatches color-blind safe series past the tenth color instead of repeating it", () => {
    const models = Array.from({ length: 11 }, (_, index) => `model-${index}`);
    const styles = [...usageChartStyleMaps(tenModelSummary(models), "colorblind").model.values()];

    expect(styles.slice(0, 10).every((style) => style.fill === "solid")).toBe(true);
    expect(styles[10]).toMatchObject({ color: styles[0]!.color, fill: "hatch" });
  });
});
