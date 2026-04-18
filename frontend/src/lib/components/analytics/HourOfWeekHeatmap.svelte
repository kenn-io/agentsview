<script lang="ts">
  import { analytics } from "../../stores/analytics.svelte.js";

  const CELL_SIZE = 17;
  const CELL_GAP = 2;
  const CELL_STEP = CELL_SIZE + CELL_GAP;
  const ROW_LABEL_WIDTH = 29;
  const COL_LABEL_HEIGHT = 18;
  const DAY_LABELS = [
    "Mon", "Tue", "Wed", "Thu", "Fri", "Sat", "Sun",
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
    for (let d = 0; d < 7; d++) {
      const hours: {
        hour: number;
        value: number;
        level: number;
      }[] = [];
      for (let h = 0; h < 24; h++) {
        const value = lookup.get(`${d}:${h}`) ?? 0;
        hours.push({
          hour: h,
          value,
          level: assignLevel(value, max),
        });
      }
      rows.push({ day: DAY_LABELS[d]!, dayIdx: d, hours });
    }
    return rows;
  });

  const svgWidth = ROW_LABEL_WIDTH + 24 * CELL_STEP + 4;
  const svgHeight = COL_LABEL_HEIGHT + 7 * CELL_STEP + 4;

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
      text: `${day} ${h}:00 - ${value.toLocaleString()} messages`,
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
        Retry
      </button>
    </div>
  {:else if grid}
    <div class="how-scroll">
      <svg
        width={svgWidth}
        height={svgHeight}
        class="how-svg"
      >
        {#each [0, 3, 6, 9, 12, 15, 18, 21] as h}
          <text
            x={h * CELL_STEP + ROW_LABEL_WIDTH + CELL_SIZE / 2}
            y={COL_LABEL_HEIGHT - 4}
            class="hour-label"
            class:active-label={analytics.selectedHour === h}
            text-anchor="middle"
            role="button"
            tabindex="0"
            onclick={() => handleHourClick(h)}
            onkeydown={(e) => {
              if (e.key === "Enter" || e.key === " ") {
                e.preventDefault();
                handleHourClick(h);
              }
            }}
          >
            {h}
          </text>
        {/each}

        {#each grid as row, rowIdx}
          <text
            x={ROW_LABEL_WIDTH - 4}
            y={rowIdx * CELL_STEP + COL_LABEL_HEIGHT + CELL_SIZE - 2}
            class="day-label"
            class:active-label={analytics.selectedDow === row.dayIdx}
            text-anchor="end"
            role="button"
            tabindex="0"
            onclick={() => handleDayClick(row.dayIdx)}
            onkeydown={(e) => {
              if (e.key === "Enter" || e.key === " ") {
                e.preventDefault();
                handleDayClick(row.dayIdx);
              }
            }}
          >
            {row.day}
          </text>

          {#each row.hours as cell}
            <rect
              x={cell.hour * CELL_STEP + ROW_LABEL_WIDTH}
              y={rowIdx * CELL_STEP + COL_LABEL_HEIGHT}
              width={CELL_SIZE}
              height={CELL_SIZE}
              rx="2"
              fill={levelColor(cell.level)}
              class="how-cell"
              class:dimmed={isDimmed(row.dayIdx, cell.hour)}
              role="button"
              tabindex="0"
              onmouseenter={(e) =>
                handleCellHover(e, row.day, cell.hour, cell.value)}
              onmouseleave={handleCellLeave}
              onclick={() =>
                handleCellClick(row.dayIdx, cell.hour)}
              onkeydown={(e) => {
                if (e.key === "Enter" || e.key === " ") {
                  e.preventDefault();
                  handleCellClick(row.dayIdx, cell.hour);
                }
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
  .how-container {
    position: relative;
    flex: 1;
  }

  .how-scroll {
    overflow-x: auto;
    padding-bottom: 4px;
  }

  .how-svg {
    display: block;
  }

  .hour-label,
  .day-label {
    font-size: 9px;
    fill: var(--text-muted);
    font-family: var(--font-sans);
    cursor: pointer;
  }

  .hour-label:hover,
  .day-label:hover {
    fill: var(--text-primary);
  }

  .active-label {
    fill: var(--accent-blue);
    font-weight: 600;
  }

  .how-cell {
    cursor: pointer;
    transition: opacity 0.15s;
  }

  .how-cell:hover {
    opacity: 0.8;
    stroke: var(--text-muted);
    stroke-width: 1;
  }

  .how-cell.dimmed {
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
