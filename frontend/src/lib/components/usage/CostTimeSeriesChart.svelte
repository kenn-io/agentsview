<script lang="ts">
  import { usage, type GroupBy } from "../../stores/usage.svelte.js";
  import { projectColor } from "../../utils/projectColor.js";

  const CHART_H = 180;
  const X_LABEL_H = 20;
  const Y_LABEL_W = 40;
  // Reserved headroom at the top of the plot area so the
  // maximum bar, its grid line, and the top y-axis label's
  // ascenders do not clip against the SVG viewBox edge.
  const TOP_PAD = 10;
  const MAX_SERIES = 5;

  const OTHER_COLOR = "var(--text-muted)";

  let containerEl: HTMLDivElement | undefined = $state();
  let containerWidth = $state(600);

  $effect(() => {
    if (!containerEl) return;
    const ro = new ResizeObserver((entries) => {
      const entry = entries[0];
      if (entry) {
        containerWidth = Math.floor(entry.contentRect.width);
      }
    });
    ro.observe(containerEl);
    return () => ro.disconnect();
  });

  interface Point {
    date: string;
    values: Record<string, number>;
  }

  const groupBy = $derived(usage.toggles.timeSeries.groupBy);

  const seriesData = $derived.by((): {
    points: Point[];
    keys: string[];
    maxY: number;
  } => {
    const daily = usage.summary?.daily;
    if (!daily || daily.length === 0) {
      return { points: [], keys: [], maxY: 0 };
    }

    // Sum cost per key across the whole range to find top N.
    const totals = new Map<string, number>();
    for (const day of daily) {
      if (groupBy === "project" && day.projectBreakdowns) {
        for (const b of day.projectBreakdowns) {
          totals.set(b.project,
            (totals.get(b.project) ?? 0) + b.cost);
        }
      } else if (groupBy === "model" && day.modelBreakdowns) {
        for (const b of day.modelBreakdowns) {
          totals.set(b.modelName,
            (totals.get(b.modelName) ?? 0) + b.cost);
        }
      } else if (groupBy === "agent" && day.agentBreakdowns) {
        for (const b of day.agentBreakdowns) {
          totals.set(b.agent,
            (totals.get(b.agent) ?? 0) + b.cost);
        }
      }
    }

    // If only one key or few keys, no need for "Other".
    if (totals.size === 0) {
      const points = daily.map((d) => ({
        date: d.date,
        values: { total: d.totalCost },
      }));
      let maxY = 0;
      for (const pt of points) {
        if (pt.values.total > maxY) maxY = pt.values.total;
      }
      return { points, keys: ["total"], maxY: maxY || 1 };
    }

    // Pick top N by total cost, group the rest as "Other".
    const ranked = [...totals.entries()]
      .sort((a, b) => b[1] - a[1]);
    const topKeys = new Set(
      ranked.slice(0, MAX_SERIES).map(([k]) => k),
    );
    const hasOther = ranked.length > MAX_SERIES;

    const points: Point[] = [];
    for (const day of daily) {
      const values: Record<string, number> = {};
      let items: Array<{ key: string; cost: number }> = [];

      if (groupBy === "project" && day.projectBreakdowns) {
        items = day.projectBreakdowns.map((b) => ({
          key: b.project, cost: b.cost,
        }));
      } else if (groupBy === "model" && day.modelBreakdowns) {
        items = day.modelBreakdowns.map((b) => ({
          key: b.modelName, cost: b.cost,
        }));
      } else if (groupBy === "agent" && day.agentBreakdowns) {
        items = day.agentBreakdowns.map((b) => ({
          key: b.agent, cost: b.cost,
        }));
      }

      for (const { key, cost } of items) {
        if (topKeys.has(key)) {
          values[key] = (values[key] ?? 0) + cost;
        } else {
          values["__other__"] =
            (values["__other__"] ?? 0) + cost;
        }
      }
      points.push({ date: day.date, values });
    }

    // Build ordered key list: top N by cost desc, then
    // __other__ (displayed as "Other" in legend/labels).
    const keys = ranked
      .slice(0, MAX_SERIES)
      .map(([k]) => k);
    if (hasOther) keys.push("__other__");

    let maxY = 0;
    for (const pt of points) {
      let stack = 0;
      for (const k of keys) {
        stack += pt.values[k] ?? 0;
      }
      if (stack > maxY) maxY = stack;
    }

    return { points, keys, maxY: maxY || 1 };
  });

  const chartWidth = $derived(
    Math.max(containerWidth - Y_LABEL_W - 8, 100),
  );

  const BAR_WIDTH = 40;

  // TICK_TARGET is the number of y-axis intervals we aim
  // for. niceScale picks a step from the 1/2/5 × 10ⁿ set so
  // the chosen max is always an integer multiple of the step
  // and every tick lands on a round value. Actual interval
  // count may come out as target ± 1 depending on where maxY
  // falls.
  const TICK_TARGET = 5;

  function niceScale(
    maxY: number,
  ): { step: number; max: number } {
    if (!Number.isFinite(maxY) || maxY <= 0) {
      return { step: 0.25, max: 1 };
    }
    const rough = maxY / TICK_TARGET;
    const exp = Math.floor(Math.log10(rough));
    const base = Math.pow(10, exp);
    const normalized = rough / base;
    let mult: number;
    if (normalized <= 1) mult = 1;
    else if (normalized <= 2) mult = 2;
    else if (normalized <= 5) mult = 5;
    else mult = 10;
    const step = mult * base;
    const max = Math.ceil(maxY / step) * step;
    return { step, max };
  }

  const scale = $derived(niceScale(seriesData.maxY));

  // scaleY maps a data value in [0, niceMax] onto the plot
  // area [TOP_PAD, h], inverted so 0 is at the bottom. Kept
  // as a function so both buildPaths and yTicks use identical
  // math and the top tick lines up with the highest bar.
  function scaleY(val: number, max: number, h: number): number {
    const plotH = h - TOP_PAD;
    return h - (val / max) * plotH;
  }

  function buildPaths(
    points: Point[],
    keys: string[],
    maxY: number,
    w: number,
    h: number,
  ): Array<{ key: string; d: string; color: string }> {
    if (points.length === 0) return [];

    // Single data point: render stacked vertical bars instead
    // of degenerate zero-width areas.
    if (points.length === 1) {
      const cx = Y_LABEL_W + w / 2;
      const x0 = cx - BAR_WIDTH / 2;
      const result: Array<{
        key: string;
        d: string;
        color: string;
      }> = [];
      let baseline = 0;
      for (const key of keys) {
        const val = points[0]!.values[key] ?? 0;
        const top = scaleY(baseline + val, maxY, h);
        const bot = scaleY(baseline, maxY, h);
        const d =
          `M${x0},${bot}` +
          `L${x0},${top}` +
          `L${x0 + BAR_WIDTH},${top}` +
          `L${x0 + BAR_WIDTH},${bot}Z`;
        const color = key === "__other__"
          ? "var(--text-muted)"
          : projectColor(key);
        result.push({ key, d, color });
        baseline += val;
      }
      return result;
    }

    const xStep = w / Math.max(points.length - 1, 1);
    const result: Array<{
      key: string;
      d: string;
      color: string;
    }> = [];

    const baselines = new Float64Array(points.length);

    for (const key of keys) {
      let d = "";

      for (let i = 0; i < points.length; i++) {
        const x = Y_LABEL_W + i * xStep;
        const val = points[i]!.values[key] ?? 0;
        const top = scaleY(baselines[i]! + val, maxY, h);
        d += i === 0 ? `M${x},${top}` : `L${x},${top}`;
      }

      // Close area back along baseline
      for (let i = points.length - 1; i >= 0; i--) {
        const x = Y_LABEL_W + i * xStep;
        const base = scaleY(baselines[i]!, maxY, h);
        d += `L${x},${base}`;
      }
      d += "Z";

      const color = key === "__other__"
        ? "var(--text-muted)"
        : projectColor(key);
      result.push({ key, d, color });

      for (let i = 0; i < points.length; i++) {
        baselines[i] = baselines[i]! + (points[i]!.values[key] ?? 0);
      }
    }

    return result;
  }

  const paths = $derived(
    buildPaths(
      seriesData.points,
      seriesData.keys,
      scale.max,
      chartWidth,
      CHART_H,
    ),
  );

  function dateLabel(date: string): string {
    const d = new Date(date + "T00:00:00");
    return d.toLocaleDateString("en", {
      month: "short",
      day: "numeric",
    });
  }

  const xLabels = $derived.by(() => {
    const pts = seriesData.points;
    if (pts.length <= 1) {
      return pts.map((p, i) => ({ x: Y_LABEL_W, label: dateLabel(p.date), idx: i }));
    }
    const step = Math.max(
      Math.floor(pts.length / 6),
      1,
    );
    const xStep =
      chartWidth / Math.max(pts.length - 1, 1);
    const labels: Array<{
      x: number;
      label: string;
      idx: number;
    }> = [];
    for (let i = 0; i < pts.length; i += step) {
      labels.push({
        x: Y_LABEL_W + i * xStep,
        label: dateLabel(pts[i]!.date),
        idx: i,
      });
    }
    return labels;
  });

  function fmtYLabel(v: number): string {
    if (v >= 100) return `$${v.toFixed(0)}`;
    if (v >= 1) return `$${v.toFixed(1)}`;
    return `$${v.toFixed(2)}`;
  }

  const yTicks = $derived.by(() => {
    const { step, max } = scale;
    if (max <= 0 || step <= 0) return [];
    const ticks: Array<{ y: number; label: string }> = [];
    // Step-driven loop so every tick is an integer multiple
    // of step and the top tick equals max exactly. Rounding
    // guards against floating-point drift on sub-unit steps.
    const count = Math.round(max / step);
    for (let i = 0; i <= count; i++) {
      const val = step * i;
      ticks.push({
        y: scaleY(val, max, CHART_H),
        label: fmtYLabel(val),
      });
    }
    return ticks;
  });

  function handleGroupByChange(g: GroupBy) {
    usage.setTimeSeriesGroupBy(g);
  }
</script>

<div class="chart-container">
  <div class="chart-header">
    <h3 class="chart-title">Cost Over Time</h3>
    <div class="segment-toggle">
      <button
        class="toggle-btn"
        class:active={groupBy === "project"}
        onclick={() => handleGroupByChange("project")}
      >
        Project
      </button>
      <button
        class="toggle-btn"
        class:active={groupBy === "model"}
        onclick={() => handleGroupByChange("model")}
      >
        Model
      </button>
      <button
        class="toggle-btn"
        class:active={groupBy === "agent"}
        onclick={() => handleGroupByChange("agent")}
      >
        Agent
      </button>
    </div>
  </div>

  {#if seriesData.points.length === 0}
    <div class="empty">No data for this period</div>
  {:else}
    <div class="chart-scroll" bind:this={containerEl}>
      <svg
        width="100%"
        height={CHART_H + X_LABEL_H}
        viewBox="0 0 {chartWidth + Y_LABEL_W + 8} {CHART_H + X_LABEL_H}"
        preserveAspectRatio="xMidYMid meet"
        class="chart-svg"
      >
        {#each yTicks as tick}
          <line
            x1={Y_LABEL_W}
            y1={tick.y}
            x2={chartWidth + Y_LABEL_W}
            y2={tick.y}
            class="grid-line"
          />
          <text
            x={Y_LABEL_W - 4}
            y={tick.y + 3}
            class="y-label"
            text-anchor="end"
          >
            {tick.label}
          </text>
        {/each}

        {#each paths as p (p.key)}
          <path d={p.d} fill={p.color} opacity="0.7" />
        {/each}

        {#each xLabels as lbl (lbl.idx)}
          <text
            x={lbl.x}
            y={CHART_H + 14}
            class="x-label"
            text-anchor="middle"
          >
            {lbl.label}
          </text>
        {/each}
      </svg>
    </div>

    {#if seriesData.keys.length > 1}
      <div class="legend">
        {#each seriesData.keys as key}
          <span class="legend-item">
            <span
              class="legend-dot"
              style="background: {key === '__other__' ? 'var(--text-muted)' : projectColor(key)}"
            ></span>
            {key === "__other__" ? "Other" : key}
          </span>
        {/each}
      </div>
    {/if}
  {/if}
</div>

<style>
  .chart-container {
    flex: 1;
    display: flex;
    flex-direction: column;
  }

  .chart-header {
    display: flex;
    align-items: center;
    justify-content: space-between;
    margin-bottom: 8px;
  }

  .chart-title {
    font-size: 12px;
    font-weight: 600;
    color: var(--text-primary);
  }

  .segment-toggle {
    display: flex;
    gap: 2px;
    background: var(--bg-inset);
    border-radius: var(--radius-sm);
    padding: 1px;
  }

  .toggle-btn {
    padding: 2px 8px;
    font-size: 10px;
    border-radius: var(--radius-sm);
    color: var(--text-muted);
    cursor: pointer;
    transition: background 0.1s, color 0.1s;
  }

  .toggle-btn.active {
    background: var(--bg-surface);
    color: var(--text-primary);
    font-weight: 500;
  }

  .toggle-btn:hover:not(.active) {
    color: var(--text-secondary);
  }

  .chart-scroll {
    overflow-x: auto;
    padding-bottom: 4px;
  }

  .chart-svg {
    display: block;
  }

  .grid-line {
    stroke: var(--border-muted);
    stroke-width: 1;
    stroke-dasharray: 2 2;
  }

  .y-label {
    font-size: 9px;
    fill: var(--text-muted);
    font-family: var(--font-mono);
  }

  .x-label {
    font-size: 9px;
    fill: var(--text-muted);
    font-family: var(--font-sans);
  }

  .legend {
    display: flex;
    gap: 12px;
    flex-wrap: wrap;
    margin-top: 8px;
    padding-left: 40px;
  }

  .legend-item {
    display: flex;
    align-items: center;
    gap: 4px;
    font-size: 10px;
    color: var(--text-muted);
  }

  .legend-dot {
    width: 8px;
    height: 8px;
    border-radius: 50%;
    flex-shrink: 0;
  }

  .empty {
    color: var(--text-muted);
    font-size: 12px;
    padding: 24px;
    text-align: center;
  }
</style>
