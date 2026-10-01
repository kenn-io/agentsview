<script lang="ts">
  import { Area, Chart, Layer, Line, Rect, Text } from "layerchart";
  import {
    Button,
    SegmentedControl,
    type SegmentedControlOption,
  } from "@kenn-io/kit-ui";
  import { scaleLinear } from "d3-scale";
  import { curveLinear, curveMonotoneX } from "d3-shape";
  import {
    usage,
    type GroupBy,
    type TimeSeriesView,
  } from "../../stores/usage.svelte.js";
  import { formatDateTime, m } from "../../i18n/index.js";
  import { formatMoney, moneyFromMicrodollars } from "../../money.js";
  import { sumSelectedTokens } from "../../stores/usageTokenTypes.js";
  import { addDays } from "../../utils/dates.js";
  import type {
    DbDailyUsageEntry,
    UsageSummaryResponse,
  } from "../../api/generated/index";

  interface Props {
    colorMap: ReadonlyMap<string, string>;
  }

  let { colorMap }: Props = $props();

  // Plot geometry matches the Activity timeline.
  const TOP_PAD = 8;
  const PLOT_H = 160;
  const X_LABEL_H = 18;
  const MIN_Y_LABEL_W = 32;
  const Y_LABEL_CHAR_W = 6;
  const Y_LABEL_GAP = 6;
  const RIGHT_PAD = 16;
  const TICK_TARGET = 4;
  const PLOT_BOTTOM = TOP_PAD + PLOT_H;
  const SVG_H = PLOT_BOTTOM + X_LABEL_H;
  const DAY_MS = 86_400_000;
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
    return !usage.isModelExcluded(key);
  }

  function fillMissingDailyEntries(
    summary: UsageSummaryResponse,
  ): DbDailyUsageEntry[] {
    const entriesByDate = new Map(
      summary.daily.map((entry) => [entry.date, entry]),
    );
    const daily: DbDailyUsageEntry[] = [];
    for (let date = summary.from; date <= summary.to;) {
      daily.push(entriesByDate.get(date) ?? {
        date,
        inputTokens: 0,
        outputTokens: 0,
        cacheCreationTokens: 0,
        cacheReadTokens: 0,
        totalCost: { microdollars: 0 },
        modelsUsed: [],
        projectBreakdowns: [],
        modelBreakdowns: [],
        agentBreakdowns: [],
        machineBreakdowns: [],
      });
      const nextDate = addDays(date, 1);
      if (!nextDate) break;
      date = nextDate;
    }
    return daily;
  }

  const seriesData = $derived.by((): {
    points: Point[];
    keys: string[];
    maxY: number;
    labels: Record<string, string>;
  } => {
    const summary = usage.timeSeriesSummary;
    if (!summary || summary.daily.length === 0) {
      return { points: [], keys: [], maxY: 0, labels: {} };
    }
    const daily = fillMissingDailyEntries(summary);

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

  const view = $derived(usage.toggles.timeSeries.view);
  const viewOptions = $derived<SegmentedControlOption[]>([
    { value: "smooth", label: m.usage_chart_style_smooth() },
    { value: "lines", label: m.usage_chart_style_lines() },
    { value: "bars", label: m.usage_chart_style_bars() },
  ]);
  const groupByOptions = $derived<SegmentedControlOption[]>([
    { value: "project", label: m.analytics_col_project() },
    { value: "model", label: m.usage_model() },
    { value: "agent", label: m.analytics_col_agent() },
  ]);
  // A single day has no neighbor to draw an area toward.
  const drawBars = $derived(
    view === "bars" || seriesData.points.length === 1,
  );

  // niceScale picks a step from the 1/2/5 × 10ⁿ set so the
  // chosen max is an integer multiple of the step and every
  // tick lands on a round value.
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
      maxLength = Math.max(maxLength, [...fmtYLabel(value)].length);
    }
    return Math.max(
      MIN_Y_LABEL_W,
      maxLength * Y_LABEL_CHAR_W + Y_LABEL_GAP,
    );
  });

  let containerEl: HTMLDivElement | undefined = $state();
  let containerWidth = $state(600);

  $effect(() => {
    if (!containerEl) return;
    const ro = new ResizeObserver((entries) => {
      const entry = entries[0];
      if (entry) containerWidth = Math.floor(entry.contentRect.width);
    });
    ro.observe(containerEl);
    return () => ro.disconnect();
  });

  const plotWidth = $derived(
    Math.max(containerWidth - yLabelWidth - RIGHT_PAD, 100),
  );
  const rangeStartMs = $derived(seriesData.points[0]?.time ?? 0);
  const rangeEndMs = $derived(
    (seriesData.points.at(-1)?.time ?? 0) + DAY_MS,
  );

  // Each day owns the cell [day start, next day start) on the x axis.
  function xForMs(ms: number): number {
    return yLabelWidth +
      ((ms - rangeStartMs) / (rangeEndMs - rangeStartMs)) * plotWidth;
  }

  function yForValue(value: number): number {
    return PLOT_BOTTOM - (value / scale.max) * PLOT_H;
  }

  function seriesColor(key: string): string {
    return key === "__other__"
      ? "var(--text-muted)"
      : colorMap.get(key) ?? "var(--text-muted)";
  }

  function seriesLabel(key: string): string {
    return key === "__other__"
      ? m.shared_other()
      : seriesData.labels[key] ?? key;
  }

  const cells = $derived(seriesData.points.map((point, idx) => {
    const cellX = xForMs(point.time);
    const cellW = plotWidth / seriesData.points.length;
    const gap = Math.min(cellW * 0.2, 2);
    let stacked = 0;
    const segments = seriesData.keys.flatMap((key) => {
      const value = point.values[key] ?? 0;
      if (value <= 0) return [];
      const y = yForValue(stacked + value);
      const height = yForValue(stacked) - y;
      stacked += value;
      return [{ key, y, height, color: seriesColor(key) }];
    });
    return {
      idx,
      cellX,
      cellW,
      x: cellX + gap / 2,
      w: Math.max(cellW - gap, 1),
      segments,
      total: stacked,
    };
  }));

  // Area vertices sit at day centers so they line up with the bars.
  const areaLayers = $derived.by(() => {
    const below = seriesData.points.map(() => 0);
    return seriesData.keys.map((key) => ({
      key,
      color: seriesColor(key),
      data: seriesData.points.map((point, i) => {
        const y0 = below[i] ?? 0;
        const y1 = y0 + (point.values[key] ?? 0);
        below[i] = y1;
        return { time: point.time + DAY_MS / 2, y0, y1 };
      }),
    }));
  });

  // Monotone curves never swing past a day's value or below zero.
  const areaCurve = $derived(view === "lines" ? curveLinear : curveMonotoneX);

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
    const last = pts.length - 1;
    const step = Math.max(Math.ceil(pts.length / 6), 1);
    type XTick = { x: number; label: string; anchor: "start" | "middle" | "end" };
    return cells.flatMap((cell): XTick[] => {
      const index = cell.idx;
      const keep = index === 0 ||
        index === last ||
        (index % step === 0 && last - index >= step / 2);
      if (!keep) return [];
      const label = dateLabel(pts[index]!.date);
      // Narrow edge cells anchor their label inside the plot.
      if (cell.cellW < 48 && last > 0 && index === 0) {
        return [{ x: cell.cellX, label, anchor: "start" }];
      }
      if (cell.cellW < 48 && last > 0 && index === last) {
        return [{ x: cell.cellX + cell.cellW, label, anchor: "end" }];
      }
      return [{ x: cell.cellX + cell.cellW / 2, label, anchor: "middle" }];
    });
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

  function fmtYLabel(v: number): string {
    return isTokenMode ? fmtTokenYLabel(v) : fmtCostYLabel(v);
  }

  function tooltipRows(point: Point) {
    return seriesData.keys
      .map((key) => ({
        key,
        label: seriesLabel(key),
        value: point.values[key] ?? 0,
        color: seriesColor(key),
      }))
      .sort((a, b) => b.value - a.value);
  }

  let tooltip = $state<{ x: number; y: number; idx: number } | null>(null);
  let tooltipEl = $state<HTMLDivElement>();
  let tooltipPos = $state<{ left: number; top: number } | null>(null);
  const TIP_PAD = 8;

  // Clamp the measured tooltip inside the viewport, as the Activity
  // timeline does. It renders hidden for one frame while this measures it.
  $effect(() => {
    if (!tooltip || !tooltipEl) {
      tooltipPos = null;
      return;
    }
    const w = tooltipEl.offsetWidth;
    const h = tooltipEl.offsetHeight;
    const left = Math.min(
      Math.max(tooltip.x - w / 2, TIP_PAD),
      Math.max(window.innerWidth - w - TIP_PAD, TIP_PAD),
    );
    const top = Math.max(tooltip.y - h, TIP_PAD);
    tooltipPos = { left, top };
  });

  function showDayTip(event: MouseEvent, idx: number) {
    const rect = (event.currentTarget as Element).getBoundingClientRect();
    tooltip = { x: rect.left + rect.width / 2, y: rect.top - 4, idx };
  }

  function hideTip() {
    tooltip = null;
  }

  // Day hits are keyed by index; drop a hover captured against old data.
  $effect.pre(() => {
    void seriesData;
    void view;
    hideTip();
  });

  const tooltipPoint = $derived(
    tooltip ? seriesData.points[tooltip.idx] ?? null : null,
  );

  const selectedRange = $derived.by(() => {
    const selection = usage.selectedTimeRange;
    if (!selection) return null;
    const pts = seriesData.points;
    const start = pts.findIndex((point) => point.date >= selection.from);
    const end = pts.findLastIndex((point) => point.date <= selection.to);
    if (start < 0 || end < start) return null;
    return { start, end: end + 1 };
  });

  let dragStart = $state<number | null>(null);
  let dragEnd = $state<number | null>(null);
  let keyboardAnchorIndex = $state<number | null>(null);

  const activeRange = $derived.by(() => {
    if (dragStart !== null) {
      const end = dragEnd ?? dragStart;
      return {
        start: Math.min(dragStart, end),
        end: Math.max(dragStart, end) + 1,
      };
    }
    return selectedRange;
  });

  const selectionBounds = $derived.by(() => {
    if (!activeRange) return null;
    const first = cells[activeRange.start];
    const last = cells[activeRange.end - 1];
    if (!first || !last) return null;
    return {
      x: first.cellX,
      width: last.cellX + last.cellW - first.cellX,
    };
  });

  function inRange(idx: number): boolean {
    return activeRange !== null &&
      idx >= activeRange.start &&
      idx < activeRange.end;
  }

  // The summary range needs two distinct days, so one-day picks do nothing.
  function selectRange(startIndex: number, endIndex: number) {
    const from = seriesData.points[Math.min(startIndex, endIndex)]?.date;
    const to = seriesData.points[Math.max(startIndex, endIndex)]?.date;
    if (from && to && from !== to) usage.setTimeRange(from, to);
  }

  function beginRangeDrag(event: PointerEvent, idx: number) {
    if (event.button !== 0) return;
    event.preventDefault();
    dragStart = idx;
    dragEnd = idx;
  }

  function extendRangeDrag(idx: number) {
    if (dragStart === null) return;
    dragEnd = idx;
  }

  function moveRangeDrag(event: PointerEvent) {
    if (dragStart === null || !containerEl || cells.length === 0) return;
    const x = event.clientX - containerEl.getBoundingClientRect().left;
    const first = cells[0]!;
    const last = cells.at(-1)!;
    if (x <= first.cellX) {
      dragEnd = 0;
      return;
    }
    if (x >= last.cellX + last.cellW) {
      dragEnd = cells.length - 1;
      return;
    }
    const idx = cells.findIndex((cell) => x < cell.cellX + cell.cellW);
    if (idx >= 0) dragEnd = idx;
  }

  function finishRangeDrag() {
    if (dragStart === null) return;
    const start = dragStart;
    const end = dragEnd ?? dragStart;
    dragStart = null;
    dragEnd = null;
    selectRange(start, end);
  }

  function cancelRangeDrag() {
    dragStart = null;
    dragEnd = null;
  }

  function onDayKey(event: KeyboardEvent, idx: number) {
    if (event.key === "Escape" && usage.selectedTimeRange) {
      event.preventDefault();
      usage.clearTimeRange();
      keyboardAnchorIndex = null;
      return;
    }
    if (event.key === "Enter" || event.key === " ") {
      event.preventDefault();
      keyboardAnchorIndex = idx;
      return;
    }
    if (event.key !== "ArrowLeft" && event.key !== "ArrowRight") return;
    event.preventDefault();
    const next = Math.max(
      0,
      Math.min(cells.length - 1, idx + (event.key === "ArrowRight" ? 1 : -1)),
    );
    if (event.shiftKey) {
      const anchor = keyboardAnchorIndex ?? idx;
      keyboardAnchorIndex = anchor;
      selectRange(anchor, next);
    } else {
      keyboardAnchorIndex = next;
    }
    queueMicrotask(() => {
      containerEl?.querySelector<SVGElement>(
        `[data-cost-day-index="${next}"]`,
      )?.focus();
    });
  }

  function handleGroupByChange(g: GroupBy) {
    usage.setTimeSeriesGroupBy(g);
  }
</script>

<svelte:window
  onpointerup={finishRangeDrag}
  onpointercancel={cancelRangeDrag}
/>

<div class="chart-container">
  <div class="chart-header">
    <h3 class="chart-title">
      {chartTitle}
    </h3>
    <div class="chart-actions">
      {#if usage.selectedTimeRange}
        <Button
          size="sm"
          surface="soft"
          label={m.sidebar_clear_selection()}
          onclick={() => usage.clearTimeRange()}
        />
      {/if}
      <SegmentedControl
        options={viewOptions}
        value={view}
        ariaLabel={m.usage_chart_style()}
        onchange={(value) => usage.setTimeSeriesView(value as TimeSeriesView)}
      />
      <SegmentedControl
        options={groupByOptions}
        value={groupBy}
        ariaLabel={m.trends_group_by()}
        onchange={(value) => handleGroupByChange(value as GroupBy)}
      />
    </div>
  </div>

  {#if seriesData.points.length === 0}
    <div class="empty">{m.shared_no_data_for_period()}</div>
  {:else}
    {#if seriesData.keys.length > 1}
      <div class="legend">
        {#each seriesData.keys as key (key)}
          <span class="legend-item">
            <span
              class="legend-dot"
              style="background: {seriesColor(key)}"
            ></span>
            {seriesLabel(key)}
          </span>
        {/each}
      </div>
    {/if}

    <div
      class="chart-body"
      role="group"
      aria-label={chartTitle}
      bind:this={containerEl}
      onpointermove={moveRangeDrag}
    >
      <Chart
        data={seriesData.points}
        x="time"
        y={(datum: { y1?: number }) => datum.y1 ?? 0}
        xScale={scaleLinear()}
        xDomain={[rangeStartMs, rangeEndMs]}
        yDomain={[0, scale.max]}
        xRange={[yLabelWidth, yLabelWidth + plotWidth]}
        yRange={[PLOT_BOTTOM, TOP_PAD]}
        padding={0}
        height={SVG_H}
      >
        <Layer class="chart-svg" title={chartTitle}>
          {#each yTickValues as value (value)}
            <Line
              x1={yLabelWidth}
              y1={yForValue(value)}
              x2={yLabelWidth + plotWidth}
              y2={yForValue(value)}
              class="grid-line"
            />
            <Text
              value={fmtYLabel(value)}
              x={yLabelWidth - 4}
              y={yForValue(value) + 3}
              class="y-label"
              textAnchor="end"
            />
          {/each}

          {#if drawBars}
            {#each cells as cell (cell.idx)}
              <g class="cost-bar" data-cost-bar={cell.idx}>
                {#each cell.segments as seg (seg.key)}
                  <Rect
                    class={`cost-seg${inRange(cell.idx) ? " selected" : ""}`}
                    x={cell.x}
                    y={seg.y}
                    width={cell.w}
                    height={seg.height}
                    fill={seg.color}
                  />
                {/each}
              </g>
            {/each}
          {:else}
            {#each areaLayers as layer (layer.key)}
              <Area
                data={layer.data}
                x="time"
                y0="y0"
                y1="y1"
                fill={layer.color}
                curve={areaCurve}
              />
            {/each}
          {/if}

          {#each xTicks as tick (tick.x)}
            <Text
              value={tick.label}
              x={tick.x}
              y={SVG_H - 4}
              class="x-label"
              textAnchor={tick.anchor}
            />
          {/each}

          {#if selectionBounds}
            <Rect
              class="range-selection"
              x={selectionBounds.x}
              y={TOP_PAD}
              width={selectionBounds.width}
              height={PLOT_H}
            />
          {/if}

          {#each cells as cell (cell.idx)}
            {@const point = seriesData.points[cell.idx]!}
            <Rect
              class="slot-hit"
              data-cost-day-index={cell.idx}
              x={cell.cellX}
              y={TOP_PAD}
              width={cell.cellW}
              height={PLOT_H}
              role="button"
              tabindex={0}
              aria-pressed={inRange(cell.idx)}
              aria-label={m.usage_chart_day_label({
                date: tooltipDateLabel(point.date),
                value: fmtYLabel(cell.total),
              })}
              onmouseenter={(event: MouseEvent) => showDayTip(event, cell.idx)}
              onmouseleave={hideTip}
              onpointerdown={(event: PointerEvent) => beginRangeDrag(event, cell.idx)}
              onpointerenter={() => extendRangeDrag(cell.idx)}
              onkeydown={(event: KeyboardEvent) => onDayKey(event, cell.idx)}
            />
          {/each}
        </Layer>
      </Chart>

      {#if tooltip && tooltipPoint}
        <div
          bind:this={tooltipEl}
          class="tooltip"
          role="status"
          style={tooltipPos
            ? `left: ${tooltipPos.left}px; top: ${tooltipPos.top}px;`
            : "visibility: hidden;"}
        >
          <div class="tooltip-date">{tooltipDateLabel(tooltipPoint.date)}</div>
          <dl class="tooltip-metrics">
            {#each tooltipRows(tooltipPoint) as row (row.key)}
              <div class="tooltip-row">
                <dt>
                  <span class="tooltip-swatch" style="background: {row.color}"></span>
                  <span class="tooltip-name">{row.label}</span>
                </dt>
                <dd>
                  {isTokenMode
                    ? fmtTokenYLabel(row.value)
                    : formatMoney(moneyFromMicrodollars(row.value))}
                </dd>
              </div>
            {/each}
          </dl>
        </div>
      {/if}
    </div>
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
    flex-wrap: wrap;
    gap: 8px;
    margin-bottom: 8px;
  }

  .chart-title {
    font-size: 12px;
    font-weight: 600;
    color: var(--text-primary);
  }

  .chart-actions {
    display: flex;
    align-items: center;
    flex-wrap: wrap;
    gap: 12px;
  }

  .chart-actions :global(.kit-button.kit-button--sm) {
    height: 22px;
    min-height: 22px;
    padding: 0 8px;
    font-size: 10px;
  }

  .legend {
    display: flex;
    align-items: center;
    flex-wrap: wrap;
    gap: var(--space-5);
    margin-bottom: 4px;
  }

  .legend-item {
    display: inline-flex;
    align-items: center;
    gap: 4px;
    font-size: 10px;
    color: var(--text-muted);
  }

  .legend-dot {
    width: 8px;
    height: 8px;
    border-radius: 2px;
    flex-shrink: 0;
  }

  .chart-body {
    width: 100%;
  }

  .chart-container :global(.chart-svg) {
    display: block;
  }

  .chart-container :global(.grid-line) {
    stroke: var(--border-muted);
    stroke-width: 1;
    stroke-dasharray: 2 2;
  }

  .chart-container :global(.y-label),
  .chart-container :global(.x-label) {
    font-size: 9px;
    fill: var(--text-muted);
    font-family: var(--font-mono);
  }

  .chart-container :global(.cost-seg) {
    opacity: 0.75;
    /* Surface-colored seam so stacked segments read as separate parts. */
    stroke: var(--bg-surface);
    stroke-width: 1;
  }

  .chart-container :global(.cost-seg.selected) {
    opacity: 1;
  }

  .chart-container :global(.range-selection) {
    fill: var(--accent-blue);
    fill-opacity: 0.16;
    stroke: var(--accent-blue);
    stroke-opacity: 1;
    stroke-width: 1.5;
    pointer-events: none;
  }

  .chart-container :global(.slot-hit) {
    fill: transparent;
    cursor: pointer;
  }

  .chart-container :global(.slot-hit:hover) {
    fill: var(--accent-blue);
    opacity: 0.08;
  }

  .chart-container :global(.slot-hit:focus-visible) {
    outline: 1px solid var(--accent-blue);
    outline-offset: -1px;
  }

  .tooltip {
    position: fixed;
    min-width: 168px;
    max-width: 320px;
    padding: 8px 10px;
    background: var(--text-primary);
    color: var(--bg-primary);
    font-size: 11px;
    border-radius: var(--radius-sm);
    white-space: nowrap;
    pointer-events: none;
    z-index: var(--z-tooltip);
  }

  .tooltip-date {
    padding-bottom: 6px;
    border-bottom: 1px solid color-mix(in srgb, currentColor 18%, transparent);
    font-weight: 600;
  }

  .tooltip-metrics {
    display: grid;
    gap: 4px;
    margin: 6px 0 0;
  }

  .tooltip-row {
    display: grid;
    grid-template-columns: minmax(0, 1fr) auto;
    gap: 16px;
    align-items: baseline;
  }

  .tooltip-row dt {
    display: flex;
    align-items: center;
    gap: 6px;
    min-width: 0;
  }

  .tooltip-swatch {
    width: 8px;
    height: 8px;
    border-radius: 2px;
    flex-shrink: 0;
  }

  .tooltip-name {
    overflow: hidden;
    text-overflow: ellipsis;
    opacity: 0.7;
  }

  .tooltip-row dd {
    margin: 0;
    font-family: var(--font-mono);
    font-weight: 600;
    text-align: right;
  }

  .empty {
    color: var(--text-muted);
    font-size: 12px;
    padding: 24px;
    text-align: center;
  }
</style>
