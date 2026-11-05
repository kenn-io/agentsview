<script lang="ts">
  import { formatCount } from "../../utils/format.js";
  import { Bar, BarChart } from "layerchart";
  import { Button } from "@kenn-io/kit-ui";
  import { scaleUtc } from "d3-scale";
  import {
    utcDay,
    utcMonday,
    utcMonth,
    type TimeInterval,
  } from "d3-time";
  import { analytics } from "../../stores/analytics.svelte.js";
  import {
    addDays,
    endOfMonth,
    localDateStr,
    parseLocalDate,
    startOfIsoWeek,
    startOfMonth,
  } from "../../utils/dates.js";
  import {
    formatDateTime,
    getLocale,
    m,
  } from "../../i18n/index.js";

  type Metric = "messages" | "sessions";
  type ScaleMode = "absolute" | "percent";
  const MAX_DAY_RANGE = 120;
  interface Props {
    deferInitialFetch?: boolean;
    onRangeSelect?: (from: string, to: string) => void;
    onRangeClear?: () => void;
  }

  let { onRangeSelect, onRangeClear, deferInitialFetch = false }: Props = $props();

  let metric = $state<Metric>("messages");
  let scaleMode = $state<ScaleMode>("absolute");
  let chartAreaWidth = $state(0);
  let keyboardAnchorIndex = $state<number | null>(null);
  const selectedRange = $derived(analytics.selectedActivityRange);
  const percentScale = $derived(
    metric === "messages" && scaleMode === "percent",
  );

  const dayRangeCount = $derived.by(() => {
    const from = Date.parse(`${analytics.from}T00:00:00Z`);
    const to = Date.parse(`${analytics.to}T00:00:00Z`);
    if (Number.isNaN(from) || Number.isNaN(to) || to < from) return 0;
    return Math.floor((to - from) / 86_400_000) + 1;
  });
  const dayViewDisabled = $derived(dayRangeCount > MAX_DAY_RANGE);

  $effect(() => {
    if (dayViewDisabled && analytics.granularity === "day") {
      if (deferInitialFetch) {
        analytics.granularity = "week";
        // Clear day data until the parent releases the initial analytics load.
        analytics.activity = null;
        analytics.errors.activity = null;
      } else {
        analytics.setGranularity("week");
      }
    }
  });

  function bucketStart(date: string): string {
    if (analytics.granularity === "week") {
      return startOfIsoWeek(date);
    }
    if (analytics.granularity === "month") {
      return startOfMonth(date);
    }
    return date;
  }

  function nextBucket(date: string): string {
    if (analytics.granularity === "week") {
      return addDays(date, 7);
    }
    if (analytics.granularity === "month") {
      const parsed = parseLocalDate(date);
      if (!parsed) return "";
      return localDateStr(
        new Date(parsed.getFullYear(), parsed.getMonth() + 1, 1),
      );
    }
    return addDays(date, 1);
  }

  function bucketDates(): string[] {
    const start = bucketStart(analytics.from);
    const end = bucketStart(analytics.to);
    if (!start || !end || start > end) return [];
    const dates: string[] = [];
    for (let date = start; date && date <= end; date = nextBucket(date)) {
      dates.push(date);
    }
    return dates;
  }

  const xInterval = $derived.by((): TimeInterval => {
    if (analytics.granularity === "week") return utcMonday;
    if (analytics.granularity === "month") return utcMonth;
    return utcDay;
  });

  const chart = $derived.by(() => {
    const activitySeries = analytics.activity?.series;
    if (!activitySeries || activitySeries.length === 0) {
      return { bars: [] };
    }

    const byDate = new Map(
      activitySeries.map((entry) => [entry.date, entry]),
    );
    const dates = bucketDates();
    const series = dates.length > 0
      ? dates.map((date) => byDate.get(date) ?? {
          date,
          sessions: 0,
          messages: 0,
          user_messages: 0,
          assistant_messages: 0,
        })
      : activitySeries;
    const bars = series.map((entry) => {
      const userMessages = entry.user_messages;
      const assistantMessages = entry.assistant_messages;
      const splitTotal = userMessages + assistantMessages;
      const total = metric === "messages" ? entry.messages : entry.sessions;
      // The message total also counts tool-result carrier rows and
      // system-injected rows that belong to neither role segment.
      const otherMessages = metric === "messages"
        ? Math.max(0, total - splitTotal)
        : 0;
      let value: number;
      let userRange: [number, number];
      let assistantRange: [number, number];
      let otherRange: [number, number];
      if (metric === "messages") {
        if (percentScale) {
          const toPct = (part: number) =>
            total > 0 ? (part / total) * 100 : 0;
          const userPct = toPct(userMessages);
          const assistantPct = toPct(assistantMessages);
          value = 100;
          userRange = [0, userPct];
          assistantRange = [userPct, userPct + assistantPct];
          otherRange = [userPct + assistantPct, total > 0 ? 100 : 0];
        } else {
          value = total;
          userRange = [0, userMessages];
          assistantRange = [userMessages, splitTotal];
          otherRange = [splitTotal, total];
        }
      } else {
        value = total;
        userRange = [0, 0];
        assistantRange = [0, 0];
        otherRange = [0, 0];
      }
      return {
        value,
        total,
        date: entry.date,
        instant: new Date(`${entry.date}T00:00:00Z`),
        userMessages,
        assistantMessages,
        otherMessages,
        userRange,
        assistantRange,
        otherRange,
      };
    });

    // Each day's focus and hover target spans the tallest column, so days
    // with no messages or no user messages stay reachable.
    const top = Math.max(0, ...bars.map((bar) => bar.value));
    return {
      bars: bars.map((bar) => ({
        ...bar,
        columnRange: [0, top] as [number, number],
      })),
    };
  });
  const xDomain = $derived.by((): [Date, Date] | undefined => {
    const first = chart.bars[0];
    const last = chart.bars.at(-1);
    if (!first || !last) return undefined;
    return [first.instant, xInterval.offset(last.instant, 1)];
  });
  // The left padding holds the y-axis value labels; the selection
  // overlay's inset in the styles below matches it.
  const PLOT_PADDING = { top: 20, right: 24, bottom: 20, left: 40 };
  const Y_TICKS = 4;
  const percentTickFormat = $derived(
    new Intl.NumberFormat(getLocale(), {
      style: "percent",
      maximumFractionDigits: 0,
    }),
  );
  function formatYTick(value: number): string {
    return percentScale
      ? percentTickFormat.format(value / 100)
      : formatCount(value);
  }
  const barInset = $derived.by(() => {
    if (chart.bars.length === 0 || chartAreaWidth === 0) return 0;
    const plotWidth = Math.max(
      chartAreaWidth - PLOT_PADDING.left - PLOT_PADDING.right,
      0,
    );
    const bucketWidth = plotWidth / chart.bars.length;
    return Math.max(0, Math.min(1, (bucketWidth - 0.75) / 2));
  });
  const selectedBrushDomain = $derived.by(():
    | [Date, Date]
    | undefined => {
    if (!selectedRange) return undefined;
    const first = new Date(`${bucketStart(selectedRange.from)}T00:00:00Z`);
    const last = new Date(`${bucketStart(selectedRange.to)}T00:00:00Z`);
    return [first, xInterval.offset(last, 1)];
  });
  const selectionCoversChart = $derived(
    selectedRange?.from === analytics.from &&
      selectedRange?.to === analytics.to,
  );

  function formatDateLabel(date: Date): string {
    return formatDateTime(date, {
      month: "short",
      day: "numeric",
      timeZone: "UTC",
    });
  }

  function barClass(
    bar: (typeof chart.bars)[number],
    segment?: "user" | "assistant" | "other",
  ): string {
    const inSelection = isSelected(bar);
    const dimmed = selectedRange !== null && !inSelection;
    return `bar${bar.total === 0 ? " empty" : ""}${inSelection ? " selected" : ""}${dimmed ? " dimmed" : ""}${segment ? ` bar-${segment}` : ""}`;
  }

  function splitPercent(bar: (typeof chart.bars)[number]): {
    user: string;
    assistant: string;
    other: string;
  } {
    const format = new Intl.NumberFormat(getLocale(), {
      style: "percent",
      maximumFractionDigits: 1,
    });
    const toPct = (part: number) =>
      format.format(bar.total > 0 ? part / bar.total : 0);
    return {
      user: toPct(bar.userMessages),
      assistant: toPct(bar.assistantMessages),
      other: toPct(bar.otherMessages),
    };
  }

  let tooltip = $state<{
    x: number;
    y: number;
    text: string;
  } | null>(null);

  function describeBar(
    bar: (typeof chart.bars)[number],
    asPercent: boolean,
  ): string[] {
    const label = formatDateTime(`${bar.date}T00:00:00`, {
      month: "short",
      day: "numeric",
      year: "numeric",
    });
    const lines = [
      m.analytics_activity_timeline_tooltip_value({
        label,
        value: formatCount(bar.total),
        metric: metric === "messages"
          ? m.analytics_metric_messages()
          : m.analytics_metric_sessions(),
      }),
    ];
    if (metric === "messages") {
      const split = asPercent
        ? splitPercent(bar)
        : {
            user: formatCount(bar.userMessages),
            assistant: formatCount(bar.assistantMessages),
            other: formatCount(bar.otherMessages),
          };
      lines.push(
        m.analytics_activity_timeline_tooltip_messages({
          user: split.user,
          assistant: split.assistant,
          other: split.other,
        }),
      );
    }
    return lines;
  }

  function isSelected(bar: (typeof chart.bars)[number]): boolean {
    return selectedRange !== null &&
      bar.date >= bucketStart(selectedRange.from) &&
      bar.date <= bucketStart(selectedRange.to);
  }

  function handleBarHover(
    e: MouseEvent,
    bar: (typeof chart.bars)[number],
  ) {
    const rect = (
      e.currentTarget as SVGElement
    ).getBoundingClientRect();
    tooltip = {
      x: rect.left + rect.width / 2,
      y: rect.top - 4,
      text: describeBar(bar, percentScale).join(" | "),
    };
  }

  function commitDateRange(from: string, to: string) {
    if (onRangeSelect) {
      onRangeSelect(from, to);
    } else {
      analytics.setActivitySelection(from, to);
    }
  }

  function clearDateRange() {
    if (onRangeClear) {
      onRangeClear();
    } else {
      analytics.clearActivitySelection();
    }
    keyboardAnchorIndex = null;
  }

  function commitIndexRange(firstIndex: number, lastIndex: number) {
    const start = Math.min(firstIndex, lastIndex);
    const end = Math.max(firstIndex, lastIndex);
    const first = chart.bars[start];
    const last = chart.bars[end];
    if (!first || !last) return;
    const from = first.date < analytics.from ? analytics.from : first.date;
    let to = last.date;
    if (analytics.granularity === "week") {
      to = addDays(to, 6);
    } else if (analytics.granularity === "month") {
      to = endOfMonth(to);
    }
    if (to > analytics.to) to = analytics.to;
    commitDateRange(from, to);
  }

  function focusBar(index: number) {
    document.querySelector<SVGElement>(
      `[data-activity-bar-index="${index}"]`,
    )?.focus();
  }

  function handleBarKeydown(event: KeyboardEvent, index: number) {
    if (event.key === "Escape" && selectedRange) {
      event.preventDefault();
      clearDateRange();
      return;
    }
    if (event.key === "Enter" || event.key === " ") {
      event.preventDefault();
      keyboardAnchorIndex = index;
      commitIndexRange(index, index);
      return;
    }
    if (event.key !== "ArrowLeft" && event.key !== "ArrowRight") return;
    event.preventDefault();
    const next = Math.max(
      0,
      Math.min(
        chart.bars.length - 1,
        index + (event.key === "ArrowRight" ? 1 : -1),
      ),
    );
    if (event.shiftKey) {
      const anchor = keyboardAnchorIndex ?? index;
      keyboardAnchorIndex = anchor;
      commitIndexRange(anchor, next);
    } else {
      keyboardAnchorIndex = next;
    }
    queueMicrotask(() => focusBar(next));
  }

  function handleBarLeave() {
    tooltip = null;
  }

  function brushDate(value: unknown): Date | null {
    if (value instanceof Date && !Number.isNaN(value.getTime())) {
      return value;
    }
    if (typeof value === "number") {
      const date = new Date(value);
      return Number.isNaN(date.getTime()) ? null : date;
    }
    return null;
  }

  function snapBrush(selection: {
    x: Array<number | Date | string | null>;
    y: Array<number | Date | string | null>;
  }) {
    const from = brushDate(selection.x[0]);
    const to = brushDate(selection.x[1]);
    if (!from || !to) return selection;
    const start = xInterval.floor(from);
    let end = xInterval.ceil(to);
    if (end.getTime() <= start.getTime()) {
      end = xInterval.offset(start, 1);
    }
    return { ...selection, x: [start, end] };
  }

  function handleBrushEnd(event: {
    brush: {
      active: boolean | undefined;
      x: Array<number | Date | string | null>;
    };
  }) {
    if (!event.brush.active) return;
    const start = brushDate(event.brush.x[0]);
    const end = brushDate(event.brush.x[1]);
    if (!start || !end) return;

    const firstBucket = xInterval.floor(start);
    const endBoundary = xInterval.ceil(end);
    const lastBucket = xInterval.offset(endBoundary, -1);
    const firstDate = firstBucket.toISOString().slice(0, 10);
    const lastDate = lastBucket.toISOString().slice(0, 10);
    const from = firstDate < analytics.from ? analytics.from : firstDate;
    let to = lastDate;
    if (analytics.granularity === "week") {
      to = addDays(lastDate, 6);
    } else if (analytics.granularity === "month") {
      to = endOfMonth(lastDate);
    }
    if (to > analytics.to) to = analytics.to;
    if (from <= to) commitDateRange(from, to);
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
          disabled={dayViewDisabled}
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
      {#if metric === "messages"}
        <div class="scale-toggle">
          <button
            class="toggle-btn"
            class:active={scaleMode === "absolute"}
            onclick={() => (scaleMode = "absolute")}
          >
            {m.analytics_activity_timeline_mode_absolute()}
          </button>
          <button
            class="toggle-btn"
            class:active={scaleMode === "percent"}
            onclick={() => (scaleMode = "percent")}
          >
            {m.analytics_activity_timeline_mode_percent()}
          </button>
        </div>
      {/if}
      {#if selectedRange}
        <Button
          size="sm"
          surface="soft"
          label={m.sidebar_clear_selection()}
          onclick={clearDateRange}
        />
      {/if}
      {#if metric === "messages"}
        <div class="segment-legend">
          <span class="legend-item">
            <span class="legend-swatch user" aria-hidden="true"></span>
            {m.message_content_role_user()}
          </span>
          <span class="legend-item">
            <span class="legend-swatch assistant" aria-hidden="true"></span>
            {m.message_content_role_assistant()}
          </span>
          <span class="legend-item">
            <span class="legend-swatch other" aria-hidden="true"></span>
            {m.shared_other()}
          </span>
        </div>
      {/if}
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
    <div class="chart-area" bind:clientWidth={chartAreaWidth}>
      <BarChart
        data={chart.bars}
        x="instant"
        y="value"
        xScale={scaleUtc()}
        {xInterval}
        {xDomain}
        yDomain={[0, null]}
        yNice
        padding={PLOT_PADDING}
        height={164}
        class="timeline-chart"
        axis={true}
        grid={{ class: "grid-line", yTicks: Y_TICKS }}
        rule={false}
        highlight={false}
        tooltipContext={false}
        brush={{
          axis: "x",
          zoomOnBrush: false,
          x: selectedBrushDomain,
          clickToReset: false,
          constrain: snapBrush,
          onBrushEnd: handleBrushEnd,
          classes: { range: "activity-brush-range" },
        }}
        props={{
          xAxis: {
            tickSpacing: 96,
            format: (date) => formatDateLabel(date as Date),
            tickMarks: false,
            rule: false,
            classes: { tickLabel: "x-label" },
          },
          yAxis: {
            ticks: Y_TICKS,
            format: (value) => formatYTick(value as number),
            tickMarks: false,
            rule: false,
            classes: { tickLabel: "y-label" },
          },
        }}
      >
        {#snippet marks()}
          {#each chart.bars as bar, index (bar.date)}
            <g
              class="day-column"
              role="button"
              tabindex={0}
              data-activity-bar-index={index}
              aria-pressed={isSelected(bar)}
              aria-label={describeBar(bar, false).join(", ")}
              onpointerenter={(event) => handleBarHover(event, bar)}
              onpointerleave={handleBarLeave}
              onkeydown={(event) => handleBarKeydown(event, index)}
            >
              <Bar
                data={bar}
                x="instant"
                y="columnRange"
                radius={1}
                insets={{ left: barInset, right: barInset }}
                class="bar-target"
              />
              {#if metric === "messages"}
                <Bar
                  data={bar}
                  x="instant"
                  y="userRange"
                  radius={1}
                  insets={{ left: barInset, right: barInset }}
                  class={barClass(bar, "user")}
                />
                <Bar
                  data={bar}
                  x="instant"
                  y="assistantRange"
                  radius={1}
                  insets={{ left: barInset, right: barInset }}
                  class={barClass(bar, "assistant")}
                />
                <Bar
                  data={bar}
                  x="instant"
                  y="otherRange"
                  radius={1}
                  insets={{ left: barInset, right: barInset }}
                  class={barClass(bar, "other")}
                />
              {:else}
                <Bar
                  data={bar}
                  x="instant"
                  radius={1}
                  insets={{ left: barInset, right: barInset }}
                  class={barClass(bar)}
                />
              {/if}
            </g>
          {/each}
        {/snippet}
      </BarChart>
      {#if selectionCoversChart}
        <div
          class="activity-selection-overlay"
          aria-hidden="true"
        ></div>
      {/if}
    </div>

    {#if tooltip}
      <div
        class="tooltip"
        style="left: {tooltip.x}px; top: {tooltip.y}px;"
      >
        <span>{tooltip.text}</span>
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
    flex: 1;
    flex-wrap: wrap;
    gap: 8px 12px;
    min-width: 0;
  }

  /* Each choice group sits in its own inset track, like the shared
     segmented control, so the three groups read as separate choices. */
  .metric-toggle,
  .granularity-toggle,
  .scale-toggle {
    display: flex;
    gap: 2px;
    padding: 2px;
    border-radius: var(--radius-md);
    background: var(--bg-inset);
  }

  .toggle-btn {
    height: 22px;
    padding: 0 8px;
    border-radius: var(--radius-sm);
    font-size: 11px;
    font-weight: 500;
    color: var(--text-muted);
    cursor: pointer;
    transition: background 0.1s, color 0.1s;
  }

  .toggle-btn:hover {
    background: var(--bg-surface-hover);
    color: var(--text-secondary);
  }

  .toggle-btn:disabled {
    opacity: var(--opacity-disabled);
    cursor: not-allowed;
  }

  .toggle-btn.active {
    background: var(--selected-strong-bg);
    box-shadow: var(--selected-strong-edge);
    color: var(--text-primary);
    font-weight: 600;
  }

  .segment-legend {
    display: flex;
    align-items: center;
    gap: var(--space-5);
    margin-left: auto;
  }

  .legend-item {
    display: flex;
    align-items: center;
    gap: 4px;
    font-size: 11px;
    color: var(--text-muted);
  }

  .legend-swatch {
    width: 8px;
    height: 8px;
    border-radius: 2px;
  }

  .legend-swatch.user {
    background: var(--accent-green);
  }

  .legend-swatch.assistant {
    background: var(--accent-blue);
  }

  .legend-swatch.other {
    background: var(--chart-series-other);
  }

  .chart-area {
    position: relative;
    padding-bottom: 4px;
    min-width: 0;
  }

  .timeline-container :global(.timeline-chart) {
    display: block;
  }

  .timeline-container :global(.grid-line) {
    stroke: var(--border-muted);
    stroke-width: 0.5;
    stroke-dasharray: 2 2;
    stroke-opacity: 0.4;
  }

  .timeline-container :global(.bar) {
    fill: var(--accent-blue);
    opacity: 0.8;
    transition: opacity 0.15s;
  }

  .timeline-container :global(.bar-user) {
    fill: var(--accent-green);
  }

  .timeline-container :global(.bar-assistant) {
    fill: var(--accent-blue);
  }

  .timeline-container :global(.bar-other) {
    fill: var(--chart-series-other);
  }

  .timeline-container :global(.day-column) {
    outline: none;
  }

  .timeline-container :global(.bar-target) {
    fill: transparent;
  }

  .timeline-container :global(.day-column:focus-visible .bar-target) {
    stroke: var(--accent-blue);
    stroke-width: 1;
  }

  .timeline-container :global(.day-column:hover .bar),
  .timeline-container :global(.day-column:focus-visible .bar) {
    opacity: 1;
  }

  .timeline-container :global(.bar.selected) {
    opacity: 1;
  }

  .timeline-container :global(.bar.dimmed) {
    opacity: 0.2;
  }

  .timeline-container :global(.day-column:hover .bar.dimmed) {
    opacity: 0.5;
  }

  .timeline-container :global(.bar.empty) {
    opacity: 0.2;
  }

  .timeline-container :global(.activity-brush-range) {
    background: color-mix(
      in srgb,
      var(--accent-blue) 16%,
      transparent
    );
    border-left: 1px solid var(--accent-blue);
    border-right: 1px solid var(--accent-blue);
  }

  .activity-selection-overlay {
    position: absolute;
    inset: 20px 24px 24px 40px;
    pointer-events: none;
    background: color-mix(
      in srgb,
      var(--accent-blue) 16%,
      transparent
    );
    border-left: 1px solid var(--accent-blue);
    border-right: 1px solid var(--accent-blue);
  }

  /* Value labels are quieter than the date labels: they give scale,
     the dates locate the bars. */
  .timeline-container :global(.y-label) {
    font-size: 10px;
    fill: var(--text-muted);
    font-family: var(--font-sans);
    font-variant-numeric: tabular-nums;
  }

  .timeline-container :global(.x-label) {
    font-size: 11px;
    fill: var(--text-secondary);
    font-family: var(--font-sans);
  }

  .tooltip {
    position: fixed;
    transform: translateX(-50%) translateY(-100%);
    padding: 4px 8px;
    background: var(--text-primary);
    color: var(--bg-primary);
    font-size: 11px;
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
