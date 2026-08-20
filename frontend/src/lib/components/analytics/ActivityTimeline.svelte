<script lang="ts">
  import { Axis, Bar, Chart, Grid, Layer } from "layerchart";
  import { scaleBand } from "d3-scale";
  import { analytics } from "../../stores/analytics.svelte.js";
  import { addDays, endOfMonth } from "../../utils/dates.js";
  import {
    formatDateTime,
    getLocale,
    m,
  } from "../../i18n/index.js";

  type Metric = "messages" | "sessions";
  interface Props {
    onDateRangeChange?: (from: string, to: string) => void;
  }

  let { onDateRangeChange }: Props = $props();

  let metric = $state<Metric>("messages");

  const chart = $derived.by(() => {
    const series = analytics.activity?.series;
    if (!series || series.length === 0) {
      return { bars: [], labels: [] as string[] };
    }

    const bars = series.map((entry) => ({
      value: metric === "messages" ? entry.messages : entry.sessions,
      date: entry.date,
      userMessages: entry.user_messages,
      assistantMessages: entry.assistant_messages,
    }));

    const labelStep = Math.max(
      1,
      Math.floor(series.length / 8),
    );
    const labels = series
      .filter((_, i) => i % labelStep === 0)
      .map((entry) => entry.date);

    return { bars, labels };
  });

  function formatDateLabel(date: string): string {
    return formatDateTime(`${date}T00:00:00`, {
      month: "short",
      day: "numeric",
    });
  }

  let tooltip = $state<{
    x: number;
    y: number;
    text: string;
  } | null>(null);

  function handleBarHover(
    e: MouseEvent,
    bar: (typeof chart.bars)[number],
  ) {
    const rect = (
      e.currentTarget as SVGElement
    ).getBoundingClientRect();
    const label = formatDateTime(`${bar.date}T00:00:00`, {
      month: "short",
      day: "numeric",
      year: "numeric",
    });
    const lines = [
      m.analytics_activity_timeline_tooltip_value({
        label,
        value: bar.value.toLocaleString(getLocale()),
        metric: metric === "messages"
          ? m.analytics_metric_messages()
          : m.analytics_metric_sessions(),
      }),
    ];
    if (metric === "messages") {
      lines.push(
        m.analytics_activity_timeline_tooltip_messages({
          user: bar.userMessages,
          assistant: bar.assistantMessages,
        }),
      );
    }
    tooltip = {
      x: rect.left + rect.width / 2,
      y: rect.top - 4,
      text: lines.join(" | "),
    };
  }

  function handleBarClick(
    bar: (typeof chart.bars)[number],
  ) {
    if (bar.value === 0) return;
    const g = analytics.granularity;
    if (g === "day") {
      analytics.selectDate(bar.date);
    } else if (g === "week") {
      commitDateRange(bar.date, addDays(bar.date, 6));
    } else if (g === "month") {
      commitDateRange(bar.date, endOfMonth(bar.date));
    }
  }

  function commitDateRange(from: string, to: string) {
    if (onDateRangeChange) {
      onDateRangeChange(from, to);
      return;
    }
    analytics.setDateRange(from, to);
  }

  function handleBarLeave() {
    tooltip = null;
  }

  function handleBarKeydown(
    event: KeyboardEvent,
    bar: (typeof chart.bars)[number],
  ) {
    if (event.key === "Enter" || event.key === " ") {
      event.preventDefault();
      handleBarClick(bar);
    }
  }
</script>

<div class="timeline-container">
  <div class="timeline-header">
    <div class="controls">
      <div class="metric-toggle">
        <button
          class="toggle-btn"
          class:active={metric === "messages"}
          onclick={() => (metric = "messages")}
        >
          {m.analytics_metric_messages()}
        </button>
        <button
          class="toggle-btn"
          class:active={metric === "sessions"}
          onclick={() => (metric = "sessions")}
        >
          {m.analytics_metric_sessions()}
        </button>
      </div>
      <div class="granularity-toggle">
        <button
          class="toggle-btn"
          class:active={analytics.granularity === "day"}
          onclick={() => analytics.setGranularity("day")}
        >
          {m.analytics_granularity_day()}
        </button>
        <button
          class="toggle-btn"
          class:active={analytics.granularity === "week"}
          onclick={() => analytics.setGranularity("week")}
        >
          {m.analytics_granularity_week()}
        </button>
        <button
          class="toggle-btn"
          class:active={analytics.granularity === "month"}
          onclick={() => analytics.setGranularity("month")}
        >
          {m.analytics_granularity_month()}
        </button>
      </div>
    </div>
  </div>

  {#if analytics.errors.activity}
    <div class="error">
      {analytics.errors.activity}
      <button
        class="retry-btn"
        onclick={() => analytics.fetchActivity()}
      >
        {m.shared_retry()}
      </button>
    </div>
  {:else if chart.bars.length > 0}
    <div class="chart-area">
      <Chart
        data={chart.bars}
        x="date"
        y="value"
        xScale={scaleBand().paddingInner(0.12).paddingOuter(0.02)}
        yDomain={[0, null]}
        yNice
        padding={{ top: 20, right: 24, bottom: 20, left: 24 }}
        height={164}
        class="timeline-chart"
      >
        <Layer>
          <Grid x={false} y class="grid-line" />
          <Axis
            placement="bottom"
            ticks={chart.labels}
            format={(date) => formatDateLabel(String(date))}
            tickMarks={false}
            rule={false}
            classes={{ tickLabel: "x-label" }}
          />
          {#each chart.bars as bar (bar.date)}
            <Bar
              data={bar}
              radius={1}
              class={`bar${bar.value === 0 ? " empty" : ""}${analytics.selectedDate === bar.date ? " selected" : ""}${analytics.selectedDate !== null && analytics.selectedDate !== bar.date ? " dimmed" : ""}`}
              role="button"
              tabindex={0}
              onclick={() => handleBarClick(bar)}
              onkeydown={(event) => handleBarKeydown(event, bar)}
              onpointerenter={(event) => handleBarHover(event, bar)}
              onpointerleave={handleBarLeave}
            />
          {/each}
        </Layer>
      </Chart>
    </div>

    {#if tooltip}
      <div
        class="tooltip"
        style="left: {tooltip.x}px; top: {tooltip.y}px;"
      >
        {tooltip.text}
      </div>
    {/if}
  {:else}
    <div class="empty">{m.analytics_activity_empty()}</div>
  {/if}
</div>

<style>
  .timeline-container {
    position: relative;
    flex: 1;
  }

  .timeline-header {
    display: flex;
    align-items: center;
    justify-content: space-between;
    margin-bottom: 8px;
  }

  .controls {
    display: flex;
    gap: 8px;
  }

  .metric-toggle,
  .granularity-toggle {
    display: flex;
    gap: 2px;
  }

  .toggle-btn {
    height: 22px;
    padding: 0 8px;
    border-radius: var(--radius-sm);
    font-size: 10px;
    font-weight: 500;
    color: var(--text-muted);
    cursor: pointer;
    transition: background 0.1s, color 0.1s;
  }

  .toggle-btn:hover {
    background: var(--bg-surface-hover);
    color: var(--text-secondary);
  }

  .toggle-btn.active {
    background: var(--bg-inset);
    color: var(--text-primary);
  }

  .chart-area {
    overflow-x: auto;
    padding-bottom: 4px;
  }

  :global(.timeline-chart) {
    display: block;
  }

  :global(.grid-line) {
    stroke: var(--border-muted);
    stroke-width: 0.5;
    stroke-dasharray: 2 2;
  }

  :global(.bar) {
    fill: var(--accent-blue);
    opacity: 0.8;
    cursor: pointer;
    transition: opacity 0.15s;
  }

  :global(.bar:hover) {
    opacity: 1;
  }

  :global(.bar.selected) {
    opacity: 1;
  }

  :global(.bar.dimmed) {
    opacity: 0.2;
  }

  :global(.bar.dimmed:hover) {
    opacity: 0.5;
  }

  :global(.bar.empty) {
    opacity: 0.2;
    cursor: default;
  }

  :global(.x-label) {
    font-size: 9px;
    fill: var(--text-muted);
    font-family: var(--font-sans);
  }

  .tooltip {
    position: fixed;
    transform: translateX(-50%) translateY(-100%);
    padding: 4px 8px;
    background: var(--text-primary);
    color: var(--bg-primary);
    font-size: 10px;
    border-radius: var(--radius-sm);
    white-space: nowrap;
    pointer-events: none;
    z-index: var(--z-tooltip);
  }

  .empty {
    color: var(--text-muted);
    font-size: 12px;
    padding: 24px;
    text-align: center;
  }

  .error {
    color: var(--accent-red);
    font-size: 12px;
    padding: 12px;
    display: flex;
    align-items: center;
    gap: 8px;
  }

  .retry-btn {
    padding: 2px 8px;
    border: 1px solid currentColor;
    border-radius: var(--radius-sm);
    font-size: 11px;
    color: inherit;
    cursor: pointer;
  }
</style>
