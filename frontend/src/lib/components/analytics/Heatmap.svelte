<script lang="ts">
  import { analytics } from "../../stores/analytics.svelte.js";
  import type { HeatmapMetric } from "../../stores/analytics.svelte.js";

  const CELL_SIZE = 16;
  const CELL_GAP = 2;
  const CELL_STEP = CELL_SIZE + CELL_GAP;
  const LABEL_WIDTH = 36;
  const HEADER_HEIGHT = 16;
  const DAY_LABELS = ["", "Mon", "", "Wed", "", "Fri", ""];

  const LEVEL_COLORS_LIGHT = [
    "var(--bg-inset)",
    "#9be9a8",
    "#40c463",
    "#30a14e",
    "#216e39",
  ];

  const LEVEL_COLORS_DARK = [
    "var(--bg-inset)",
    "#0e4429",
    "#006d32",
    "#26a641",
    "#39d353",
  ];

  interface DayCell {
    date: string;
    value: number;
    level: number;
    dayOfWeek: number;
  }

  function levelColor(level: number): string {
    const isDark = document.documentElement.classList.contains(
      "dark",
    );
    const colors = isDark ? LEVEL_COLORS_DARK : LEVEL_COLORS_LIGHT;
    return colors[level] ?? colors[0]!;
  }

  function metricLabel(metric: HeatmapMetric): string {
    switch (metric) {
      case "sessions":
        return "Sessions";
      case "output_tokens":
        return "Output Tokens";
      default:
        return "Messages";
    }
  }

  let tooltip = $state<{
    x: number;
    y: number;
    text: string;
  } | null>(null);

  function handleCellHover(
    e: MouseEvent,
    cell: DayCell,
  ) {
    const rect = (
      e.currentTarget as SVGElement
    ).getBoundingClientRect();
    const d = new Date(cell.date + "T00:00:00");
    const label = d.toLocaleDateString("en", {
      month: "short",
      day: "numeric",
      year: "numeric",
    });
    tooltip = {
      x: rect.left + rect.width / 2,
      y: rect.top - 4,
      text: `${label}: ${cell.value.toLocaleString()} ${metricLabel(analytics.metric)}`,
    };
  }

  function handleCellLeave() {
    tooltip = null;
  }

  function handleCellClick(cell: DayCell) {
    analytics.selectDate(cell.date);
  }

  // Use a simple pre-computed grid
  const grid = $derived.by(() => {
    const entries = analytics.heatmap?.entries;
    if (!entries || entries.length === 0) {
      return { cols: [] as DayCell[][], months: [] as { col: number; label: string }[] };
    }

    const cols: DayCell[][] = [];
    let currentCol: DayCell[] = [];
    let lastMonth = "";
    const monthLabels: { col: number; label: string }[] = [];

    for (let i = 0; i < entries.length; i++) {
      const entry = entries[i]!;
      const d = new Date(entry.date + "T00:00:00");
      const dow = d.getDay();
      const cell: DayCell = {
        date: entry.date,
        value: entry.value,
        level: entry.level,
        dayOfWeek: dow,
      };

      // New column on Sunday (except first entry)
      if (i > 0 && dow === 0) {
        cols.push(currentCol);
        currentCol = [];
      }

      const month = d.toLocaleString("en", { month: "short" });
      if (month !== lastMonth && dow <= 3) {
        monthLabels.push({ col: cols.length, label: month });
        lastMonth = month;
      }

      currentCol.push(cell);
    }
    if (currentCol.length > 0) {
      cols.push(currentCol);
    }

    return { cols, months: monthLabels };
  });

  const svgWidth = $derived(
    grid.cols.length * CELL_STEP + LABEL_WIDTH + 4,
  );
  const svgHeight = 7 * CELL_STEP + HEADER_HEIGHT + 4;
  const supportsOutputTokens = $derived(
    analytics.summary?.total_output_tokens !== undefined &&
      analytics.summary?.token_reporting_sessions !== undefined,
  );
</script>

<div class="heatmap-container">
  <div class="heatmap-header">
    <h3 class="chart-title">Activity</h3>
    <div class="metric-toggle">
      <button
        class="toggle-btn"
        class:active={analytics.metric === "messages"}
        onclick={() => analytics.setMetric("messages")}
      >
        Messages
      </button>
      <button
        class="toggle-btn"
        class:active={analytics.metric === "sessions"}
        onclick={() => analytics.setMetric("sessions")}
      >
        Sessions
      </button>
      {#if supportsOutputTokens}
        <button
          class="toggle-btn"
          class:active={analytics.metric === "output_tokens"}
          onclick={() => analytics.setMetric("output_tokens")}
        >
          Output Tokens
        </button>
      {/if}
    </div>
  </div>

  {#if analytics.errors.heatmap}
    <div class="error">
      {analytics.errors.heatmap}
      <button
        class="retry-btn"
        onclick={() => analytics.fetchHeatmap()}
      >
        Retry
      </button>
    </div>
  {:else if grid.cols.length > 0}
    {#if analytics.heatmap?.entries_from && analytics.heatmap.entries_from > analytics.from}
      <div class="clamp-note">Showing most recent year</div>
    {/if}
    <div class="heatmap-scroll">
      <svg
        width={svgWidth}
        height={svgHeight}
        class="heatmap-svg"
      >
        {#each DAY_LABELS as label, i}
          {#if label}
            <text
              x={LABEL_WIDTH - 4}
              y={i * CELL_STEP + HEADER_HEIGHT + CELL_SIZE - 1}
              class="day-label"
              text-anchor="end"
            >
              {label}
            </text>
          {/if}
        {/each}

        {#each grid.months as m}
          <text
            x={m.col * CELL_STEP + LABEL_WIDTH}
            y={HEADER_HEIGHT - 4}
            class="month-label"
          >
            {m.label}
          </text>
        {/each}

        {#each grid.cols as col, colIdx}
          {#each col as cell}
            <rect
              x={colIdx * CELL_STEP + LABEL_WIDTH}
              y={cell.dayOfWeek * CELL_STEP + HEADER_HEIGHT}
              width={CELL_SIZE}
              height={CELL_SIZE}
              rx="2"
              fill={levelColor(cell.level)}
              class="heatmap-cell"
              class:clickable={cell.value > 0 || analytics.selectedDate === cell.date}
              class:selected={analytics.selectedDate === cell.date}
              role="button"
              tabindex="-1"
              onmouseenter={(e) => handleCellHover(e, cell)}
              onmouseleave={handleCellLeave}
              onclick={() => {
                if (cell.value > 0 || analytics.selectedDate === cell.date)
                  handleCellClick(cell);
              }}
              onkeydown={(e) => {
                if (e.key === "Enter" && (cell.value > 0 || analytics.selectedDate === cell.date))
                  handleCellClick(cell);
              }}
            />
          {/each}
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
    <div class="empty">No data for this period</div>
  {/if}
</div>

<style>
  .heatmap-container {
    position: relative;
    flex: 1;
  }

  .heatmap-header {
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

  .metric-toggle {
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

  .heatmap-scroll {
    overflow-x: auto;
    padding-bottom: 4px;
  }

  .heatmap-svg {
    display: block;
    margin: 0 auto;
  }

  .day-label, .month-label {
    font-size: 9px;
    fill: var(--text-muted);
    font-family: var(--font-sans);
  }

  .heatmap-cell {
    cursor: default;
  }

  .heatmap-cell.clickable {
    cursor: pointer;
  }

  .heatmap-cell.clickable:hover {
    opacity: 0.8;
    stroke: var(--text-muted);
    stroke-width: 1;
  }

  .heatmap-cell.selected {
    stroke: var(--text-primary);
    stroke-width: 2;
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

  .clamp-note {
    color: var(--text-muted);
    font-size: 10px;
    margin-bottom: 4px;
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
