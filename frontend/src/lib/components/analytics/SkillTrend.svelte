<script lang="ts">
  import { analytics } from "../../stores/analytics.svelte.js";
  import GranularityPicker from "../shared/GranularityPicker.svelte";
  import { formatDateTime, m } from "../../i18n/index.js";
  import { parseLocalDate } from "../../utils/dates.js";

  // Soft cap from the series-count ladder: past six skills the tail folds
  // into "Other" instead of generating more hues.
  const MAX_SERIES = 6;
  const OTHER_KEY = "__other__";
  const PLOT_HEIGHT = 120;
  const PLOT_TOP = 8;
  const LABEL_HEIGHT = 18;
  const SVG_HEIGHT = PLOT_TOP + PLOT_HEIGHT + LABEL_HEIGHT;
  const PAD_X = 10;
  const MAX_X_LABELS = 14;

  const trendEntries = $derived(analytics.skills?.trend ?? []);

  const skillTotals = $derived.by(() => {
    const totals = new Map<string, number>();
    for (const entry of trendEntries) {
      for (const [skill, count] of Object.entries(entry.by_skill)) {
        totals.set(skill, (totals.get(skill) ?? 0) + count);
      }
    }
    return totals;
  });

  // Fixed series order by overall volume; color follows the skill for the
  // whole render, so legend toggles never repaint the survivors.
  const topSkills = $derived.by(() => {
    return [...skillTotals.entries()]
      .sort((a, b) => b[1] - a[1] || a[0].localeCompare(b[0]))
      .slice(0, MAX_SERIES)
      .map(([skill]) => skill);
  });

  const otherTotal = $derived.by(() => {
    let total = 0;
    const top = new Set(topSkills);
    for (const [skill, count] of skillTotals) {
      if (!top.has(skill)) total += count;
    }
    return total;
  });

  interface Series {
    key: string;
    label: string;
    total: number;
    colorIndex: number | null;
    values: number[];
  }

  // All series in fixed order (top skills then the "Other" fold), with a
  // value for every bucket so each series draws as a continuous line.
  const allSeries = $derived.by(() => {
    const series: Series[] = topSkills.map((skill, i) => ({
      key: skill,
      label: skill,
      total: skillTotals.get(skill) ?? 0,
      colorIndex: i,
      values: trendEntries.map(
        (entry) => entry.by_skill[skill] ?? 0,
      ),
    }));
    if (otherTotal > 0) {
      const top = new Set(topSkills);
      series.push({
        key: OTHER_KEY,
        label: m.analytics_skill_trend_other(),
        total: otherTotal,
        colorIndex: null,
        values: trendEntries.map((entry) => {
          let sum = 0;
          for (const [skill, count] of Object.entries(
            entry.by_skill,
          )) {
            if (!top.has(skill)) sum += count;
          }
          return sum;
        }),
      });
    }
    return series;
  });

  let hiddenKeys = $state<string[]>([]);

  function toggleSeries(key: string) {
    hiddenKeys = hiddenKeys.includes(key)
      ? hiddenKeys.filter((k) => k !== key)
      : [...hiddenKeys, key];
  }

  const visibleSeries = $derived(
    allSeries.filter((s) => !hiddenKeys.includes(s.key)),
  );

  const maxValue = $derived.by(() => {
    let max = 1;
    for (const series of visibleSeries) {
      for (const v of series.values) {
        if (v > max) max = v;
      }
    }
    return max;
  });

  // Fallback width until the first measurement lands (and in test DOMs
  // that never report layout sizes).
  const FALLBACK_WIDTH = 600;
  let measuredWidth = $state(0);
  const chartWidth = $derived(
    measuredWidth > 0 ? measuredWidth : FALLBACK_WIDTH,
  );

  function xAt(index: number): number {
    const n = trendEntries.length;
    const span = Math.max(chartWidth - 2 * PAD_X, 0);
    if (n <= 1) return PAD_X + span / 2;
    return PAD_X + (index * span) / (n - 1);
  }

  function yAt(value: number): number {
    return PLOT_TOP + PLOT_HEIGHT - (value / maxValue) * PLOT_HEIGHT;
  }

  function linePath(values: number[]): string {
    return values
      .map((v, i) => {
        const cmd = i === 0 ? "M" : "L";
        return `${cmd}${xAt(i).toFixed(1)},${yAt(v).toFixed(1)}`;
      })
      .join(" ");
  }

  function seriesColor(colorIndex: number | null): string {
    if (colorIndex === null) return "var(--chart-series-other)";
    return `var(--chart-series-${colorIndex + 1})`;
  }

  const labelStep = $derived(
    Math.max(Math.ceil(trendEntries.length / MAX_X_LABELS), 1),
  );

  function bucketLabel(date: string, index: number): string {
    if (index % labelStep !== 0) return "";
    const parsed = parseLocalDate(date);
    if (!parsed) return date;
    if (analytics.skillsGranularity === "month") {
      return formatDateTime(parsed, {
        year: "numeric",
        month: "short",
      });
    }
    return formatDateTime(parsed, {
      month: "short",
      day: "numeric",
    });
  }

  function bucketDateLabel(date: string): string {
    const parsed = parseLocalDate(date);
    if (!parsed) return date;
    return formatDateTime(parsed, {
      year: "numeric",
      month: "short",
      day: "numeric",
    });
  }

  // Edge labels anchor inward so they never clip at the chart bounds.
  function labelAnchor(index: number): string {
    const x = xAt(index);
    if (x < 30) return "start";
    if (x > chartWidth - 30) return "end";
    return "middle";
  }

  // Crosshair: snap the pointer to the nearest bucket and read out every
  // visible series at that X in one tooltip.
  let hoverIndex = $state<number | null>(null);
  let tooltipPos = $state<{ x: number; y: number } | null>(null);

  function handleMove(e: MouseEvent) {
    const n = trendEntries.length;
    if (n === 0 || chartWidth <= 0) return;
    const rect = (
      e.currentTarget as SVGElement
    ).getBoundingClientRect();
    const x = e.clientX - rect.left;
    const span = Math.max(chartWidth - 2 * PAD_X, 1);
    const index = Math.min(
      Math.max(Math.round(((x - PAD_X) / span) * (n - 1)), 0),
      n - 1,
    );
    hoverIndex = index;
    tooltipPos = {
      x: rect.left + xAt(index),
      y: rect.top + PLOT_TOP - 6,
    };
  }

  function handleLeave() {
    hoverIndex = null;
    tooltipPos = null;
  }

  function setKeyboardHover(element: HTMLElement, index: number) {
    const rect = element.getBoundingClientRect();
    hoverIndex = index;
    tooltipPos = {
      x: rect.left + xAt(index),
      y: rect.top + PLOT_TOP - 6,
    };
  }

  function handleFocus(e: FocusEvent) {
    if (trendEntries.length === 0) return;
    setKeyboardHover(e.currentTarget as HTMLElement, 0);
  }

  function handleKeydown(e: KeyboardEvent) {
    if (trendEntries.length === 0) return;
    if (e.key !== "ArrowLeft" && e.key !== "ArrowRight") return;
    e.preventDefault();
    const delta = e.key === "ArrowRight" ? 1 : -1;
    const index = Math.min(
      Math.max((hoverIndex ?? 0) + delta, 0),
      trendEntries.length - 1,
    );
    setKeyboardHover(e.currentTarget as HTMLElement, index);
  }

  const hoverReadout = $derived.by(() => {
    if (hoverIndex === null) return [];
    const index = hoverIndex;
    return visibleSeries
      .map((series) => ({
        key: series.key,
        label: series.label,
        colorIndex: series.colorIndex,
        value: series.values[index] ?? 0,
      }))
      .sort((a, b) => b.value - a.value);
  });
</script>

<div class="trend-container">
  <div class="trend-header">
    <h3 class="chart-title">{m.analytics_skill_trend_title()}</h3>
    <GranularityPicker
      value={analytics.skillsGranularity}
      onChange={(g) => analytics.setSkillsGranularity(g)}
      disabled={analytics.querying.skills}
    />
  </div>

  {#if analytics.errors.skills}
    <div class="error">
      {analytics.errors.skills}
      <button
        class="retry-btn"
        onclick={() => analytics.fetchSkills()}
      >
        {m.shared_retry()}
      </button>
    </div>
  {:else if analytics.loading.skills && trendEntries.length === 0}
    <div class="empty">{m.analytics_skill_trend_loading()}</div>
  {:else if trendEntries.length > 0 && allSeries.length > 0}
    <div
      class="legend"
      role="group"
      aria-label={m.analytics_skill_trend_legend()}
    >
      {#each allSeries as series (series.key)}
        <button
          class="legend-chip"
          class:hidden-series={hiddenKeys.includes(series.key)}
          aria-pressed={!hiddenKeys.includes(series.key)}
          onclick={() => toggleSeries(series.key)}
        >
          <span
            class="legend-key"
            style="background: {seriesColor(series.colorIndex)}"
          ></span>
          <span class="legend-name">{series.label}</span>
          <span class="legend-count">
            {series.total.toLocaleString()}
          </span>
        </button>
      {/each}
    </div>

    <div
      class="chart"
      bind:clientWidth={measuredWidth}
      role="slider"
      tabindex="0"
      aria-label={m.analytics_skill_trend_chart_label()}
      aria-describedby="skill-trend-data"
      aria-valuemin="0"
      aria-valuemax={Math.max(trendEntries.length - 1, 0)}
      aria-valuenow={hoverIndex ?? 0}
      aria-valuetext={bucketDateLabel(
        trendEntries[hoverIndex ?? 0]?.date ?? "",
      )}
      onmousemove={handleMove}
      onmouseleave={handleLeave}
      onfocus={handleFocus}
      onblur={handleLeave}
      onkeydown={handleKeydown}
    >
      <svg
        width={chartWidth}
        height={SVG_HEIGHT}
        class="chart-svg"
        aria-hidden="true"
      >
        <line
          class="baseline"
          x1={PAD_X}
          y1={PLOT_TOP + PLOT_HEIGHT}
          x2={chartWidth - PAD_X}
          y2={PLOT_TOP + PLOT_HEIGHT}
        />

        {#each visibleSeries as series (series.key)}
          {#if trendEntries.length > 1}
            <path
              class="series-line"
              d={linePath(series.values)}
              style="stroke: {seriesColor(series.colorIndex)}"
            />
          {:else}
            <circle
              class="series-marker"
              cx={xAt(0)}
              cy={yAt(series.values[0] ?? 0)}
              r="4"
              style="fill: {seriesColor(series.colorIndex)}"
            />
          {/if}
        {/each}

        {#if hoverIndex !== null}
          <line
            class="crosshair"
            x1={xAt(hoverIndex)}
            y1={PLOT_TOP}
            x2={xAt(hoverIndex)}
            y2={PLOT_TOP + PLOT_HEIGHT}
          />
          {#each visibleSeries as series (series.key)}
            <circle
              class="series-marker"
              cx={xAt(hoverIndex)}
              cy={yAt(series.values[hoverIndex] ?? 0)}
              r="4"
              style="fill: {seriesColor(series.colorIndex)}"
            />
          {/each}
        {/if}

        {#each trendEntries as entry, index (entry.date)}
          {@const label = bucketLabel(entry.date, index)}
          {#if label}
            <text
              class="x-label"
              x={xAt(index)}
              y={SVG_HEIGHT - 4}
              text-anchor={labelAnchor(index)}
            >
              {label}
            </text>
          {/if}
        {/each}
      </svg>
      <table id="skill-trend-data" class="kit-sr-only">
        <caption>{m.analytics_skill_trend_title()}</caption>
        <thead>
          <tr>
            <th scope="col">{m.analytics_skill_trend_date()}</th>
            {#each allSeries as series (series.key)}
              <th scope="col">{series.label}</th>
            {/each}
          </tr>
        </thead>
        <tbody>
          {#each trendEntries as entry, index (entry.date)}
            <tr>
              <th scope="row">{bucketDateLabel(entry.date)}</th>
              {#each allSeries as series (series.key)}
                <td>{(series.values[index] ?? 0).toLocaleString()}</td>
              {/each}
            </tr>
          {/each}
        </tbody>
      </table>
    </div>

    {#if hoverIndex !== null && tooltipPos}
      <div
        class="tooltip"
        role="status"
        aria-live="polite"
        style="left: {tooltipPos.x}px; top: {tooltipPos.y}px;"
      >
        <div class="tooltip-date">
          {bucketDateLabel(trendEntries[hoverIndex]?.date ?? "")}
        </div>
        {#each hoverReadout as row (row.key)}
          <div class="tooltip-row">
            <span
              class="tip-key"
              style="background: {seriesColor(row.colorIndex)}"
            ></span>
            <span class="tip-value">
              {row.value.toLocaleString()}
            </span>
            <span class="tip-name">{row.label}</span>
          </div>
        {/each}
      </div>
    {/if}
  {:else}
    <div class="empty">{m.analytics_skill_trend_empty()}</div>
  {/if}
</div>

<style>
  /* Series colors come from the --chart-series-* app tokens in app.css,
     which carry their own light/dark steps. */
  .trend-container {
    position: relative;
    flex: 1;
  }

  .trend-header {
    display: flex;
    align-items: center;
    justify-content: space-between;
    margin-bottom: 8px;
    gap: 12px;
  }

  .chart-title {
    font-size: 12px;
    font-weight: 600;
    color: var(--text-primary);
  }

  .legend {
    display: flex;
    align-items: center;
    flex-wrap: wrap;
    gap: 4px;
    margin-bottom: 10px;
  }

  .legend-chip { /* kit-ui-check-ignore: the legend is a pressed-state toggle, and the current kit Chip and Button controls cannot preserve aria-pressed. */
    display: inline-flex;
    align-items: center;
    gap: var(--space-2);
    max-width: 220px;
    padding: 2px 7px;
    border: 1px solid var(--border-muted);
    border-radius: var(--radius-sm);
    background: var(--bg-inset);
    color: var(--text-secondary);
    font-size: 10px;
    cursor: pointer;
    transition: opacity 0.1s, background 0.1s;
  }

  .legend-chip:hover {
    background: var(--bg-surface-hover);
  }

  .legend-chip.hidden-series {
    opacity: 0.45;
  }

  .legend-chip.hidden-series .legend-key {
    background: var(--text-muted) !important;
  }

  /* Line key mirroring the mark: a short stroke, not a box. */
  .legend-key {
    flex-shrink: 0;
    width: 10px;
    height: 2px;
    border-radius: 1px;
  }

  .legend-name {
    min-width: 0;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  .legend-count {
    font-family: var(--font-mono);
    color: var(--text-muted);
  }

  .chart {
    width: 100%;
  }

  .chart-svg {
    display: block;
  }

  .baseline {
    stroke: var(--border-muted);
    stroke-width: 1;
  }

  .series-line {
    fill: none;
    stroke-width: 2;
    stroke-linecap: round;
    stroke-linejoin: round;
  }

  /* Surface ring keeps markers legible where they cross a line. */
  .series-marker {
    stroke: var(--bg-surface);
    stroke-width: 2;
    pointer-events: none;
  }

  .crosshair {
    stroke: var(--text-muted);
    stroke-width: 1;
    stroke-dasharray: none;
    opacity: 0.5;
    pointer-events: none;
  }

  .x-label {
    font-size: 8px;
    fill: var(--text-muted);
  }

  .tooltip {
    position: fixed;
    transform: translateX(-50%) translateY(-100%);
    padding: 5px 8px;
    background: var(--text-primary);
    color: var(--bg-primary);
    font-size: 10px;
    border-radius: var(--radius-sm);
    white-space: nowrap;
    pointer-events: none;
    z-index: var(--z-tooltip);
  }

  .tooltip-date {
    font-weight: 600;
    margin-bottom: 3px;
  }

  .tooltip-row {
    display: flex;
    align-items: center;
    gap: var(--space-2);
  }

  .tip-key {
    flex-shrink: 0;
    width: 8px;
    height: 2px;
    border-radius: 1px;
  }

  .tip-value {
    font-family: var(--font-mono);
    font-weight: 600;
    min-width: 20px;
    text-align: right;
  }

  .tip-name {
    opacity: 0.8;
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
