<script lang="ts">
  import { Chart, Layer, Rect, Text } from "layerchart";
  import { analytics } from "../../stores/analytics.svelte.js";
  import { getLocale, m } from "../../i18n/index.js";

  const CELL_GAP = 2;
  const CELL_HEIGHT = 17;
  const ROW_STEP = CELL_HEIGHT + CELL_GAP;
  const ROW_LABEL_WIDTH = 29;
  const COL_LABEL_HEIGHT = 18;
  const FALLBACK_WIDTH = 480;
  const DAYS = [
    { label: "Sun", dayIdx: 6 },
    { label: "Mon", dayIdx: 0 },
    { label: "Tue", dayIdx: 1 },
    { label: "Wed", dayIdx: 2 },
    { label: "Thu", dayIdx: 3 },
    { label: "Fri", dayIdx: 4 },
    { label: "Sat", dayIdx: 5 },
  ];

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

  function levelColor(level: number): string {
    const isDark =
      document.documentElement.classList.contains("dark");
    const colors = isDark
      ? LEVEL_COLORS_DARK
      : LEVEL_COLORS_LIGHT;
    return colors[level] ?? colors[0]!;
  }

  function assignLevel(value: number, max: number): number {
    if (value <= 0) return 0;
    if (max <= 0) return 1;
    const ratio = value / max;
    if (ratio <= 0.25) return 1;
    if (ratio <= 0.5) return 2;
    if (ratio <= 0.75) return 3;
    return 4;
  }

  let tooltip = $state<{
    x: number;
    y: number;
    text: string;
  } | null>(null);

  const grid = $derived.by(() => {
    const cells = analytics.hourOfWeek?.cells;
    if (!cells || cells.length === 0) return null;

    const lookup = new Map<string, number>();
    let max = 0;
    for (const c of cells) {
      lookup.set(`${c.day_of_week}:${c.hour}`, c.messages);
      if (c.messages > max) max = c.messages;
    }

    const rows: {
      day: string;
      dayIdx: number;
      hours: {
        hour: number;
        value: number;
        level: number;
      }[];
    }[] = [];
    for (const day of DAYS) {
      const hours: {
        hour: number;
        value: number;
        level: number;
      }[] = [];
      for (let h = 0; h < 24; h++) {
        const value = lookup.get(`${day.dayIdx}:${h}`) ?? 0;
        hours.push({
          hour: h,
          value,
          level: assignLevel(value, max),
        });
      }
      rows.push({
        day: day.label,
        dayIdx: day.dayIdx,
        hours,
      });
    }
    return rows;
  });

  let availableWidth = $state(0);
  const chartWidth = $derived(availableWidth || FALLBACK_WIDTH);
  const cellStep = $derived(
    Math.max((chartWidth - ROW_LABEL_WIDTH - 4) / 24, 1),
  );
  const cellWidth = $derived(Math.max(cellStep - CELL_GAP, 1));
  const svgHeight = COL_LABEL_HEIGHT + 7 * ROW_STEP + 4;

  function handleCellHover(
    e: MouseEvent,
    day: string,
    hour: number,
    value: number,
  ) {
    const rect = (
      e.currentTarget as SVGElement
    ).getBoundingClientRect();
    const h = hour.toString().padStart(2, "0");
    tooltip = {
      x: rect.left + rect.width / 2,
      y: rect.top - 4,
      text: m.analytics_hour_of_week_tooltip({
        day,
        hour: h,
        count: value,
        countLabel: value.toLocaleString(getLocale()),
      }),
    };
  }

  function handleCellLeave() {
    tooltip = null;
  }

  function handleCellClick(dow: number, hour: number) {
    analytics.selectHourOfWeek(dow, hour);
  }

  function handleDayClick(dow: number) {
    analytics.selectHourOfWeek(dow, null);
  }

  function handleHourClick(hour: number) {
    analytics.selectHourOfWeek(null, hour);
  }

  function isDimmed(dow: number, hour: number): boolean {
    const sd = analytics.selectedDow;
    const sh = analytics.selectedHour;
    if (sd === null && sh === null) return false;
    if (sd !== null && sh !== null) {
      return dow !== sd || hour !== sh;
    }
    if (sd !== null) return dow !== sd;
    return hour !== sh;
  }

</script>

<div class="how-container">
  {#if analytics.errors.hourOfWeek}
    <div class="error">
      {analytics.errors.hourOfWeek}
      <button
        class="retry-btn"
        onclick={() => analytics.fetchHourOfWeek()}
      >
        {m.shared_retry()}
      </button>
    </div>
  {:else if grid}
    <div class="how-chart" bind:clientWidth={availableWidth}>
      <Chart
        width={chartWidth}
        height={svgHeight}
        padding={0}
      >
        <Layer class="how-svg">
          {#each [0, 3, 6, 9, 12, 15, 18, 21] as hour}
            <Text
              value={hour}
              x={hour * cellStep + ROW_LABEL_WIDTH + cellWidth / 2}
              y={COL_LABEL_HEIGHT - 4}
              class={`hour-label${analytics.selectedHour === hour ? " active-label" : ""}`}
              textAnchor="middle"
              role="button"
              tabindex={0}
              onclick={() => handleHourClick(hour)}
              onkeydown={(event) => {
                if (event.key === "Enter" || event.key === " ") {
                  event.preventDefault();
                  handleHourClick(hour);
                }
              }}
            />
          {/each}

          {#each grid as row, rowIdx}
            <Text
              value={row.day}
              x={ROW_LABEL_WIDTH - 4}
              y={rowIdx * ROW_STEP + COL_LABEL_HEIGHT + CELL_HEIGHT - 2}
              class={`day-label${analytics.selectedDow === row.dayIdx ? " active-label" : ""}`}
              textAnchor="end"
              role="button"
              tabindex={0}
              onclick={() => handleDayClick(row.dayIdx)}
              onkeydown={(event) => {
                if (event.key === "Enter" || event.key === " ") {
                  event.preventDefault();
                  handleDayClick(row.dayIdx);
                }
              }}
            />

            {#each row.hours as cell}
              <Rect
                x={cell.hour * cellStep + ROW_LABEL_WIDTH}
                y={rowIdx * ROW_STEP + COL_LABEL_HEIGHT}
                width={cellWidth}
                height={CELL_HEIGHT}
                rx={2}
                fill={levelColor(cell.level)}
                class={`how-cell${isDimmed(row.dayIdx, cell.hour) ? " dimmed" : ""}`}
                role="button"
                tabindex={0}
                onmouseenter={(event) =>
                  handleCellHover(event, row.day, cell.hour, cell.value)}
                onmouseleave={handleCellLeave}
                onclick={() => handleCellClick(row.dayIdx, cell.hour)}
                onkeydown={(event) => {
                  if (event.key === "Enter" || event.key === " ") {
                    event.preventDefault();
                    handleCellClick(row.dayIdx, cell.hour);
                  }
                }}
              />
            {/each}
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
    <div class="empty">{m.shared_no_data_for_period()}</div>
  {/if}
</div>

<style>
  .how-container {
    position: relative;
    flex: 1;
  }

  .how-chart {
    width: 100%;
    min-width: 0;
  }

  .how-container :global(.how-svg) {
    display: block;
  }

  .how-container :global(.hour-label),
  .how-container :global(.day-label) {
    font-size: 9px;
    fill: var(--text-muted);
    font-family: var(--font-sans);
    cursor: pointer;
  }

  .how-container :global(.hour-label:hover),
  .how-container :global(.day-label:hover) {
    fill: var(--text-primary);
  }

  .how-container :global(.active-label) {
    fill: var(--accent-blue);
    font-weight: 600;
  }

  .how-container :global(.how-cell) {
    cursor: pointer;
    transition: opacity 0.15s;
  }

  .how-container :global(.how-cell:hover) {
    opacity: 0.8;
    stroke: var(--text-muted);
    stroke-width: 1;
  }

  .how-container :global(.how-cell.dimmed) {
    opacity: 0.2;
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
