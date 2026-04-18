<script lang="ts">
  import { analytics } from "../../stores/analytics.svelte.js";

  const BAR_HEIGHT = 120;
  const LABEL_HEIGHT = 20;
  const TOP_PAD = 20;
  const SVG_HEIGHT = BAR_HEIGHT + LABEL_HEIGHT + TOP_PAD + 4;
  const MIN_BAR_WIDTH = 6;
  const BAR_GAP = 2;

  type Metric = "messages" | "sessions";
  let metric = $state<Metric>("messages");

  let containerEl = $state<HTMLDivElement | null>(null);
  let containerWidth = $state(600);

  $effect(() => {
    if (!containerEl) return;
    const obs = new ResizeObserver((entries) => {
      for (const entry of entries) {
        containerWidth = entry.contentRect.width;
      }
    });
    obs.observe(containerEl);
    return () => obs.disconnect();
  });

  const chart = $derived.by(() => {
    const series = analytics.activity?.series;
    if (!series || series.length === 0) {
      return { bars: [], maxVal: 0, labels: [] };
    }

    const values = series.map((e) =>
      metric === "messages" ? e.messages : e.sessions,
    );
    const maxVal = Math.max(...values, 1);

    const barWidth = Math.max(
      MIN_BAR_WIDTH,
      Math.floor(
        (containerWidth - series.length * BAR_GAP) /
          series.length,
      ),
    );

    const bars = series.map((entry, i) => {
      const val = values[i]!;
      const height = (val / maxVal) * BAR_HEIGHT;
      return {
        x: i * (barWidth + BAR_GAP),
        y: TOP_PAD + BAR_HEIGHT - height,
        width: barWidth,
        height,
        value: val,
        date: entry.date,
        userMessages: entry.user_messages,
        assistantMessages: entry.assistant_messages,
      };
    });

    // Generate sparse x-axis labels
    const labelStep = Math.max(
      1,
      Math.floor(series.length / 8),
    );
    const labels = series
      .filter((_, i) => i % labelStep === 0)
      .map((entry, _, arr) => {
        const idx = series.indexOf(entry);
        return {
          x: idx * (barWidth + BAR_GAP) + barWidth / 2,
          text: formatDateLabel(entry.date),
        };
      });

    return { bars, maxVal, labels };
  });

  const svgWidth = $derived(
    chart.bars.length > 0
      ? chart.bars[chart.bars.length - 1]!.x +
          chart.bars[0]!.width +
          4
      : containerWidth,
  );

  function formatDateLabel(date: string): string {
    const d = new Date(date + "T00:00:00");
    return d.toLocaleDateString("en", {
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
    const d = new Date(bar.date + "T00:00:00");
    const label = d.toLocaleDateString("en", {
      month: "short",
      day: "numeric",
      year: "numeric",
    });
    const lines = [`${label}: ${bar.value.toLocaleString()} ${metric}`];
    if (metric === "messages") {
      lines.push(
        `user: ${bar.userMessages} / assistant: ${bar.assistantMessages}`,
      );
    }
    tooltip = {
      x: rect.left + rect.width / 2,
      y: rect.top - 4,
      text: lines.join(" | "),
    };
  }

  function addDays(dateStr: string, n: number): string {
    const d = new Date(dateStr + "T00:00:00");
    d.setDate(d.getDate() + n);
    const y = d.getFullYear();
    const m = String(d.getMonth() + 1).padStart(2, "0");
    const day = String(d.getDate()).padStart(2, "0");
    return `${y}-${m}-${day}`;
  }

  function endOfMonth(dateStr: string): string {
    const d = new Date(dateStr + "T00:00:00");
    d.setMonth(d.getMonth() + 1, 0);
    const y = d.getFullYear();
    const m = String(d.getMonth() + 1).padStart(2, "0");
    const day = String(d.getDate()).padStart(2, "0");
    return `${y}-${m}-${day}`;
  }

  function handleBarClick(
    bar: (typeof chart.bars)[number],
  ) {
    if (bar.value === 0) return;
    const g = analytics.granularity;
    if (g === "day") {
      analytics.selectDate(bar.date);
    } else if (g === "week") {
      analytics.setDateRange(bar.date, addDays(bar.date, 6));
    } else if (g === "month") {
      analytics.setDateRange(bar.date, endOfMonth(bar.date));
    }
  }

  function handleBarLeave() {
    tooltip = null;
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
          Messages
        </button>
        <button
          class="toggle-btn"
          class:active={metric === "sessions"}
          onclick={() => (metric = "sessions")}
        >
          Sessions
        </button>
      </div>
      <div class="granularity-toggle">
        <button
          class="toggle-btn"
          class:active={analytics.granularity === "day"}
          onclick={() => analytics.setGranularity("day")}
        >
          Day
        </button>
        <button
          class="toggle-btn"
          class:active={analytics.granularity === "week"}
          onclick={() => analytics.setGranularity("week")}
        >
          Week
        </button>
        <button
          class="toggle-btn"
          class:active={analytics.granularity === "month"}
          onclick={() => analytics.setGranularity("month")}
        >
          Month
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
        Retry
      </button>
    </div>
  {:else if chart.bars.length > 0}
    <div class="chart-area" bind:this={containerEl}>
      <svg
        width={svgWidth}
        height={SVG_HEIGHT}
        class="timeline-svg"
      >
        <!-- Y-axis guide lines -->
        {#each [0.25, 0.5, 0.75, 1] as frac}
          <line
            x1="0"
            y1={TOP_PAD + BAR_HEIGHT * (1 - frac)}
            x2={svgWidth}
            y2={TOP_PAD + BAR_HEIGHT * (1 - frac)}
            class="grid-line"
          />
        {/each}

        <!-- Bars -->
        {#each chart.bars as bar}
          <rect
            x={bar.x}
            y={bar.y}
            width={bar.width}
            height={Math.max(bar.height, 1)}
            rx="1"
            class="bar"
            class:empty={bar.value === 0}
            class:selected={analytics.selectedDate === bar.date}
            class:dimmed={analytics.selectedDate !== null && analytics.selectedDate !== bar.date}
            role="button"
            tabindex="0"
            onclick={() => handleBarClick(bar)}
            onkeydown={(e) => {
              if (e.key === "Enter" || e.key === " ") {
                e.preventDefault();
                handleBarClick(bar);
              }
            }}
            onmouseenter={(e) => handleBarHover(e, bar)}
            onmouseleave={handleBarLeave}
          />
        {/each}

        <!-- X-axis labels -->
        {#each chart.labels as label}
          <text
            x={label.x}
            y={TOP_PAD + BAR_HEIGHT + LABEL_HEIGHT - 4}
            class="x-label"
            text-anchor="middle"
          >
            {label.text}
          </text>
        {/each}
      </svg>
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
    <div class="empty">No activity data</div>
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

  .timeline-svg {
    display: block;
  }

  .grid-line {
    stroke: var(--border-muted);
    stroke-width: 0.5;
    stroke-dasharray: 2 2;
  }

  .bar {
    fill: var(--accent-blue);
    opacity: 0.8;
    cursor: pointer;
    transition: opacity 0.15s;
  }

  .bar:hover {
    opacity: 1;
  }

  .bar.selected {
    opacity: 1;
  }

  .bar.dimmed {
    opacity: 0.2;
  }

  .bar.dimmed:hover {
    opacity: 0.5;
  }

  .bar.empty {
    opacity: 0.2;
    cursor: default;
  }

  .x-label {
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
    z-index: 100;
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
