<script lang="ts">
  import { Area, Bar, Chart, Layer, Tooltip } from "layerchart";
  import { Button } from "@kenn-io/kit-ui";
  import { scaleBand, scalePoint } from "d3-scale";
  import LargeChartFrame from "../shared/LargeChartFrame.svelte";
  import { usage, type GroupBy } from "../../stores/usage.svelte.js";
  import { formatDateTime, m } from "../../i18n/index.js";
  import { formatMoney, moneyFromMicrodollars } from "../../money.js";
  import { sumSelectedTokens } from "../../stores/usageTokenTypes.js";

  interface Props {
    colorMap: ReadonlyMap<string, string>;
  }

  let { colorMap }: Props = $props();

  const CHART_H = 180;
  const MIN_Y_LABEL_W = 40;
  const Y_LABEL_CHAR_W = 6;
  const Y_LABEL_GAP = 4;
  // Reserved headroom at the top of the plot area so the
  // maximum bar, its grid line, and the top y-axis label's
  // ascenders do not clip against the SVG viewBox edge.
  const MAX_SERIES = 10;

  interface Point {
    date: string;
    time: number;
    values: Record<string, number>;
  }

  function dateTime(date: string): number {
    return Date.parse(`${date}T00:00:00Z`);
  }

  const groupBy = $derived(usage.toggles.timeSeries.groupBy);
  const isTokenMode = $derived(usage.mode === "token");
  const chartTitle = $derived(
    isTokenMode
      ? m.usage_tokens_over_time_title()
      : m.usage_cost_over_time_title(),
  );

  function breakdownTokens(b: {
    inputTokens: number;
    outputTokens: number;
    cacheCreationTokens: number;
    cacheReadTokens: number;
  }): number {
    return sumSelectedTokens(b, usage.selectedTokenTypes);
  }

  function isSeriesVisible(key: string): boolean {
    if (groupBy === "project") return !usage.isProjectKeyExcluded(key);
    if (groupBy === "agent") return !usage.isAgentExcluded(key);
    if (!usage.selectedModels) return true;
    return usage.selectedModels.split(",").includes(key);
  }

  const seriesData = $derived.by((): {
    points: Point[];
    keys: string[];
    maxY: number;
    labels: Record<string, string>;
  } => {
    const daily = usage.timeSeriesSummary?.daily;
    if (!daily || daily.length === 0) {
      return { points: [], keys: [], maxY: 0, labels: {} };
    }

    // Sum the selected value per key across the whole range to find top N.
    const totals = new Map<string, number>();
    const labels: Record<string, string> = {};
    let hasBreakdownData = false;
    for (const day of daily) {
      if (groupBy === "project" && day.projectBreakdowns) {
        hasBreakdownData ||= day.projectBreakdowns.length > 0;
        for (const b of day.projectBreakdowns) {
          if (!isSeriesVisible(b.project_key)) continue;
          labels[b.project_key] = b.project;
          const value = isTokenMode
            ? breakdownTokens(b)
            : b.cost.microdollars;
          totals.set(
            b.project_key,
            (totals.get(b.project_key) ?? 0) + value,
          );
        }
      } else if (groupBy === "model" && day.modelBreakdowns) {
        hasBreakdownData ||= day.modelBreakdowns.length > 0;
        for (const b of day.modelBreakdowns) {
          if (!isSeriesVisible(b.modelName)) continue;
          const value = isTokenMode
            ? breakdownTokens(b)
            : b.cost.microdollars;
          totals.set(
            b.modelName,
            (totals.get(b.modelName) ?? 0) + value,
          );
          labels[b.modelName] = b.modelName;
        }
      } else if (groupBy === "agent" && day.agentBreakdowns) {
        hasBreakdownData ||= day.agentBreakdowns.length > 0;
        for (const b of day.agentBreakdowns) {
          if (!isSeriesVisible(b.agent)) continue;
          const value = isTokenMode
            ? breakdownTokens(b)
            : b.cost.microdollars;
          totals.set(
            b.agent,
            (totals.get(b.agent) ?? 0) + value,
          );
          labels[b.agent] = b.agent;
        }
      }
    }

    // If only one key or few keys, no need for "Other".
    if (totals.size === 0) {
      if (hasBreakdownData) {
        return { points: [], keys: [], maxY: 0, labels };
      }
      const points = daily.map((d) => ({
        date: d.date,
        time: dateTime(d.date),
        values: {
          total: isTokenMode
            ? breakdownTokens(d)
            : d.totalCost.microdollars,
        },
      }));
      let maxY = 0;
      for (const pt of points) {
        if (pt.values.total > maxY) maxY = pt.values.total;
      }
      return { points, keys: ["total"], maxY: maxY || 1, labels };
    }

    // Pick top N by total value, group the rest as "Other".
    const ranked = [...totals.entries()]
      .sort((a, b) => b[1] - a[1]);
    const topKeys = new Set(
      ranked.slice(0, MAX_SERIES).map(([k]) => k),
    );
    const hasOther = ranked.length > MAX_SERIES;

    const points: Point[] = [];
    for (const day of daily) {
      const values: Record<string, number> = {};
      let items: Array<{ key: string; value: number }> = [];

      if (groupBy === "project" && day.projectBreakdowns) {
        items = day.projectBreakdowns
          .filter((b) => isSeriesVisible(b.project_key))
          .map((b) => ({
            key: b.project_key,
            value: isTokenMode ? breakdownTokens(b) : b.cost.microdollars,
          }));
      } else if (groupBy === "model" && day.modelBreakdowns) {
        items = day.modelBreakdowns
          .filter((b) => isSeriesVisible(b.modelName))
          .map((b) => ({
            key: b.modelName,
            value: isTokenMode ? breakdownTokens(b) : b.cost.microdollars,
          }));
      } else if (groupBy === "agent" && day.agentBreakdowns) {
        items = day.agentBreakdowns
          .filter((b) => isSeriesVisible(b.agent))
          .map((b) => ({
            key: b.agent,
            value: isTokenMode ? breakdownTokens(b) : b.cost.microdollars,
          }));
      }

      for (const { key, value } of items) {
        if (topKeys.has(key)) {
          values[key] = (values[key] ?? 0) + value;
        } else {
          values["__other__"] =
            (values["__other__"] ?? 0) + value;
        }
      }
      points.push({ date: day.date, time: dateTime(day.date), values });
    }

    // Build ordered key list: top N by value desc, then
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

    return { points, keys, maxY: maxY || 1, labels };
  });

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

  const yTickValues = $derived.by(() => {
    const { step, max } = scale;
    if (max <= 0 || step <= 0) return [];
    const count = Math.round(max / step);
    return Array.from({ length: count + 1 }, (_, i) => step * i);
  });

  const yLabelWidth = $derived.by(() => {
    let maxLength = 0;
    for (const value of yTickValues) {
      const label = isTokenMode
        ? fmtTokenYLabel(value)
        : fmtCostYLabel(value);
      maxLength = Math.max(maxLength, [...label].length);
    }
    return Math.max(
      MIN_Y_LABEL_W,
      maxLength * Y_LABEL_CHAR_W + Y_LABEL_GAP,
    );
  });

  function dateLabel(date: string): string {
    return formatDateTime(`${date}T00:00:00`, {
      month: "short",
      day: "numeric",
    });
  }

  function tooltipDateLabel(date: string): string {
    return formatDateTime(`${date}T00:00:00`, {
      year: "numeric",
      month: "short",
      day: "numeric",
    });
  }

  const xTicks = $derived.by(() => {
    const pts = seriesData.points;
    const step = Math.max(Math.ceil(pts.length / 6), 1);
    return pts
      .filter((_, index) =>
        index === 0 || index === pts.length - 1 || index % step === 0
      )
      .map((point) => point.time);
  });

  function fmtCostYLabel(v: number): string {
    return formatMoney(moneyFromMicrodollars(v));
  }

  function fmtTokenYLabel(v: number): string {
    if (v >= 1_000_000_000) return `${(v / 1_000_000_000).toFixed(1)}B`;
    if (v >= 1_000_000) return `${(v / 1_000_000).toFixed(1)}M`;
    if (v >= 1_000) return `${(v / 1_000).toFixed(1)}K`;
    return String(Math.round(v));
  }

  const stackSeries = $derived(
    seriesData.keys.map((key) => ({
      key,
      value: (point: Point) => point.values[key] ?? 0,
      color: key === "__other__"
        ? "var(--text-muted)"
        : colorMap.get(key) ?? "var(--text-muted)",
    })),
  );

  function seriesColor(key: string): string {
    return key === "__other__"
      ? "var(--text-muted)"
      : colorMap.get(key) ?? "var(--text-muted)";
  }

  function tooltipRows(point: Point) {
    return seriesData.keys
      .map((key) => ({
        key,
        label: key === "__other__"
          ? m.shared_other()
          : seriesData.labels[key] ?? key,
        value: point.values[key] ?? 0,
        color: seriesColor(key),
      }))
      .sort((a, b) => b.value - a.value);
  }

  function handleGroupByChange(g: GroupBy) {
    usage.setTimeSeriesGroupBy(g);
  }

  const selectedBrushDomain = $derived(
    usage.selectedTimeRange
      ? [
          dateTime(usage.selectedTimeRange.from),
          dateTime(usage.selectedTimeRange.to),
        ]
      : [null, null],
  );

  function handleBrushEnd(event: {
    brush: {
      active: boolean | undefined;
      x: Array<number | Date | string | null>;
    };
  }) {
    if (!event.brush.active) return;
    const first = event.brush.x[0];
    const last = event.brush.x[1];
    if (typeof first !== "number" || typeof last !== "number") return;
    const fromTime = Math.min(first, last);
    const toTime = Math.max(first, last);
    const from = seriesData.points.find((point) => point.time === fromTime)?.date;
    const to = seriesData.points.find((point) => point.time === toTime)?.date;
    if (from && to) usage.setTimeRange(from, to);
  }

  function handleKeyboardRangeSubmit(event: SubmitEvent) {
    event.preventDefault();
    const form = event.currentTarget as HTMLFormElement;
    const data = new FormData(form);
    const from = String(data.get("from") ?? "");
    const to = String(data.get("to") ?? "");
    if (
      from < to &&
      seriesData.points.some((point) => point.date === from) &&
      seriesData.points.some((point) => point.date === to)
    ) {
      usage.setTimeRange(from, to);
    }
  }
</script>

<div class="chart-container">
  <div class="chart-header">
    <h3 class="chart-title">
      {chartTitle}
    </h3>
    <div class="chart-actions">
      <form
        class="keyboard-range kit-sr-only"
        aria-label={m.shared_range_select_date_range()}
        onsubmit={handleKeyboardRangeSubmit}
      >
        <label>
          <span>{m.shared_range_from()}</span>
          <!-- kit-ui-check-ignore: focus-only keyboard fallback for the chart brush; DateRangePicker would duplicate the page range control and alter chart chrome. -->
          <input type="date"
            name="from"
            min={seriesData.points[0]?.date}
            max={seriesData.points.at(-1)?.date}
            value={usage.selectedTimeRange?.from ?? seriesData.points[0]?.date ?? ""}
            required
          />
        </label>
        <label>
          <span>{m.shared_range_to()}</span>
          <!-- kit-ui-check-ignore: focus-only keyboard fallback for the chart brush; DateRangePicker would duplicate the page range control and alter chart chrome. -->
          <input type="date"
            name="to"
            min={seriesData.points[0]?.date}
            max={seriesData.points.at(-1)?.date}
            value={usage.selectedTimeRange?.to ?? seriesData.points.at(-1)?.date ?? ""}
            required
          />
        </label>
        <Button
          type="submit"
          size="sm"
          surface="soft"
          label={m.shared_range_select_date_range()}
        />
      </form>
      {#if usage.selectedTimeRange}
        <Button
          size="sm"
          surface="soft"
          label={m.sidebar_clear_selection()}
          onclick={() => usage.clearTimeRange()}
        />
      {/if}
      <div class="segment-toggle">
        <button
          class="toggle-btn"
          class:active={groupBy === "project"}
          onclick={() => handleGroupByChange("project")}
        >
          {m.analytics_col_project()}
        </button>
        <button
          class="toggle-btn"
          class:active={groupBy === "model"}
          onclick={() => handleGroupByChange("model")}
        >
          {m.usage_model()}
        </button>
        <button
          class="toggle-btn"
          class:active={groupBy === "agent"}
          onclick={() => handleGroupByChange("agent")}
        >
          {m.analytics_col_agent()}
        </button>
      </div>
    </div>
  </div>

  {#if seriesData.points.length === 0}
    <div class="empty">{m.shared_no_data_for_period()}</div>
  {:else}
    <div class="chart-scroll">
      <Chart
        data={seriesData.points}
        x="time"
        y={(point) => Math.max(...seriesData.keys.map((key) => point.values[key] ?? 0))}
        xScale={seriesData.points.length === 1
          ? scaleBand().padding(0.5)
          : scalePoint().padding(0.05)}
        yDomain={[0, scale.max]}
        series={stackSeries}
        seriesLayout="stack"
        padding={{ top: 10, right: 24, bottom: 20, left: yLabelWidth }}
        height={CHART_H + 20}
        brush={{
          axis: "x",
          zoomOnBrush: false,
          x: selectedBrushDomain,
          clickToReset: false,
          onBrushEnd: handleBrushEnd,
          classes: { range: "usage-brush-range" },
        }}
        tooltipContext={{ mode: "bisect-x" }}
      >
        <Layer class="chart-svg" title={chartTitle}>
          <LargeChartFrame
            xTicks={xTicks}
            yTicks={yTickValues}
            formatX={(value) => dateLabel(
              new Date(Number(value)).toISOString().slice(0, 10),
            )}
            formatY={(value) => isTokenMode
              ? fmtTokenYLabel(Number(value))
              : fmtCostYLabel(Number(value))}
          >
            {#each stackSeries as item (item.key)}
              {#if seriesData.points.length === 1}
                <Bar
                  data={seriesData.points[0]!}
                  seriesKey={item.key}
                  fill={item.color}
                  radius={1}
                />
              {:else}
                <Area
                  seriesKey={item.key}
                  fill={item.color}
                />
              {/if}
            {/each}
          </LargeChartFrame>
        </Layer>
        <Tooltip.Root variant="none" fadeDuration={0}>
          {#snippet children({ data })}
            <div class="usage-series-tooltip" role="status">
              <div class="tooltip-date">{tooltipDateLabel(data.date)}</div>
              {#each tooltipRows(data) as row (row.key)}
                <div class="tooltip-row">
                  <span class="tooltip-dot" style="background: {row.color}"></span>
                  <span class="tooltip-name">{row.label}</span>
                  <span class="tooltip-value">
                    {isTokenMode
                      ? fmtTokenYLabel(row.value)
                      : formatMoney(moneyFromMicrodollars(row.value))}
                  </span>
                </div>
              {/each}
            </div>
          {/snippet}
        </Tooltip.Root>
      </Chart>
    </div>

    {#if seriesData.keys.length > 1}
      <div class="legend" style:padding-left="{yLabelWidth}px">
        {#each seriesData.keys as key}
          <span class="legend-item">
            <span
              class="legend-dot"
              style="background: {colorMap.get(key) ?? 'var(--text-muted)'}"
            ></span>
            {key === "__other__" ? m.shared_other() : (seriesData.labels[key] ?? key)}
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
    position: relative;
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

  .chart-actions {
    display: flex;
    align-items: center;
    gap: 8px;
  }

  .chart-actions :global(.kit-button.kit-button--sm) {
    height: 20px;
    min-height: 20px;
    padding: 0 8px;
    font-size: 10px;
  }

  .keyboard-range:focus-within {
    z-index: 3;
    top: calc(100% + 4px);
    right: 0;
    width: auto;
    height: auto;
    margin: 0;
    padding: 8px;
    overflow: visible;
    clip-path: none;
    display: flex;
    align-items: end;
    gap: 8px;
    white-space: normal;
    background: var(--bg-surface);
    border: 1px solid var(--border-muted);
    border-radius: var(--radius-sm);
    box-shadow: var(--shadow-md);
  }

  .keyboard-range label {
    display: grid;
    gap: var(--space-2);
    font-size: 10px;
    color: var(--text-muted);
  }

  /* kit-ui-check-ignore: native date input for the focus-only brush fallback; Card is not a form-control replacement. */
  .keyboard-range input {
    min-height: 24px;
    padding: 2px 6px;
    font: inherit;
    color: var(--text-primary);
    background: var(--bg-inset);
    border: 1px solid var(--border-muted);
    border-radius: var(--radius-sm);
  }

  .keyboard-range input:focus-visible {
    outline: 2px solid var(--accent-blue);
    outline-offset: 1px;
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
    overflow-x: hidden;
    padding-bottom: 4px;
  }

  .chart-container :global(.usage-brush-range) {
    background: color-mix(
      in srgb,
      var(--accent-blue) 16%,
      transparent
    );
    border-left: 1px solid var(--accent-blue);
    border-right: 1px solid var(--accent-blue);
  }

  .chart-container :global(.chart-svg) {
    display: block;
  }

  .usage-series-tooltip {
    min-width: 180px;
    max-width: 300px;
    padding: 8px 10px;
    color: var(--text-primary);
    background: var(--bg-surface);
    border: 1px solid var(--border-muted);
    border-radius: var(--radius-sm);
    box-shadow: var(--shadow-md);
    font-size: 11px;
  }

  .tooltip-date {
    margin-bottom: 6px;
    color: var(--text-secondary);
    font-weight: 600;
  }

  .tooltip-row {
    display: grid;
    grid-template-columns: 7px minmax(0, 1fr) auto;
    align-items: center;
    gap: var(--space-4);
    min-height: 20px;
  }

  .tooltip-dot {
    width: 7px;
    height: 7px;
    border-radius: 50%;
  }

  .tooltip-name {
    overflow: hidden;
    color: var(--text-secondary);
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  .tooltip-value {
    color: var(--text-primary);
    font-variant-numeric: tabular-nums;
    text-align: right;
  }

  .legend {
    display: flex;
    gap: 12px;
    flex-wrap: wrap;
    margin-top: 8px;
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
