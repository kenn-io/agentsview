<script lang="ts">
  import { Chart, Circle, Layer, Line, Spline } from "layerchart";
  import { scalePoint } from "d3-scale";
  import { analytics } from "../../stores/analytics.svelte.js";
  import { settings } from "../../stores/settings.svelte.js";
  import GranularityPicker from "../shared/GranularityPicker.svelte";
  import { formatDateTime, getLocale, m } from "../../i18n/index.js";
  import { parseLocalDate } from "../../utils/dates.js";
  import { chartSeriesColorMap } from "../../utils/chartPalette.js";
  import LargeChartFrame from "../shared/LargeChartFrame.svelte";

  // Soft cap from the series-count ladder: past six skills the tail folds
  // into "Other" instead of generating more hues.
  const MAX_SERIES = 6;
  const OTHER_KEY = "__other__";
  const PLOT_HEIGHT = 120;
  const PLOT_TOP = 8;
  const LABEL_HEIGHT = 18;
  const SVG_HEIGHT = PLOT_TOP + PLOT_HEIGHT + LABEL_HEIGHT;
  const PLOT_LEFT = 40;
  const PLOT_RIGHT = 10;
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
    values: number[];
  }

  // All series in fixed order (top skills then the "Other" fold), with a
  // value for every bucket so each series draws as a continuous line.
  const allSeries = $derived.by(() => {
    const series: Series[] = topSkills.map((skill) => ({
      key: skill,
      label: skill,
      total: skillTotals.get(skill) ?? 0,
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

  const colorMap = $derived(chartSeriesColorMap(
    allSeries.map((series) => series.key),
    settings.chartPalette,
    (_key, index) => `var(--chart-series-${index + 1})`,
    "var(--chart-series-other)",
  ));

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
    const span = Math.max(chartWidth - PLOT_LEFT - PLOT_RIGHT, 0);
    if (n <= 1) return PLOT_LEFT + span / 2;
    return PLOT_LEFT + (index * span) / (n - 1);
  }

  function seriesColor(key: string): string {
    return colorMap.get(key) ?? "var(--text-muted)";
  }

  const labelStep = $derived(
    Math.max(Math.ceil(trendEntries.length / MAX_X_LABELS), 1),
  );

  function bucketLabel(date: string): string {
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

  const xTicks = $derived(
    trendEntries
      .filter((_, index) =>
        index === 0 ||
        index === trendEntries.length - 1 ||
        index % labelStep === 0
      )
      .map((entry) => entry.date),
  );

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
    const span = Math.max(chartWidth - PLOT_LEFT - PLOT_RIGHT, 1);
    const index = Math.min(
      Math.max(Math.round(((x - PLOT_LEFT) / span) * (n - 1)), 0),
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
        value: series.values[index] ?? 0,
      }))
      .sort((a, b) => b.value - a.value);
  });

  const chartSeries = $derived(
    visibleSeries.map((series) => ({
      ...series,
      points: trendEntries.map((entry, index) => ({
        date: entry.date,
        value: series.values[index] ?? 0,
      })),
    })),
  );

  const chartData = $derived(chartSeries.flatMap((series) => series.points));
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
            style="background: {seriesColor(series.key)}"
          ></span>
          <span class="legend-name">{series.label}</span>
          <span class="legend-count">
            {series.total.toLocaleString(getLocale())}
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
      <Chart
        data={chartData}
        x="date"
        y="value"
        xScale={scalePoint()}
        yDomain={[0, maxValue]}
        xRange={[PLOT_LEFT, chartWidth - PLOT_RIGHT]}
        yRange={[PLOT_TOP + PLOT_HEIGHT, PLOT_TOP]}
        padding={0}
        width={chartWidth}
        height={SVG_HEIGHT}
        aria-hidden="true"
      >
        <Layer class="chart-svg" aria-hidden="true">
          <LargeChartFrame
            {xTicks}
            yTicks={4}
            formatX={(value) => bucketLabel(String(value))}
            formatY={(value) => Number(value).toLocaleString(getLocale())}
          >
            {#each chartSeries as series (series.key)}
              {#if trendEntries.length > 1}
                <Spline
                  class="series-line"
                  data={series.points}
                  x="date"
                  y="value"
                  style={`stroke: ${seriesColor(series.key)}`}
                  fill="none"
                />
              {:else}
                <Circle
                  class="series-marker"
                  data={series.points}
                  x="date"
                  y="value"
                  r={4}
                  fill={seriesColor(series.key)}
                />
              {/if}
            {/each}

            {#if hoverIndex !== null}
              <Line
                class="crosshair"
                x1={xAt(hoverIndex)}
                y1={PLOT_TOP}
                x2={xAt(hoverIndex)}
                y2={PLOT_TOP + PLOT_HEIGHT}
              />
              {#each chartSeries as series (series.key)}
                <Circle
                  class="series-marker"
                  data={[series.points[hoverIndex]]}
                  x="date"
                  y="value"
                  r={4}
                  fill={seriesColor(series.key)}
                />
              {/each}
            {/if}
          </LargeChartFrame>
        </Layer>
      </Chart>
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
                <td>{(series.values[index] ?? 0).toLocaleString(getLocale())}</td>
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
              style="background: {seriesColor(row.key)}"
            ></span>
            <span class="tip-value">
              {row.value.toLocaleString(getLocale())}
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

  .trend-container :global(.chart-svg) {
    display: block;
  }

  .trend-container :global(.series-line) {
    fill: none;
    stroke-width: 2;
    stroke-linecap: round;
    stroke-linejoin: round;
  }

  /* Surface ring keeps markers legible where they cross a line. */
  .trend-container :global(.series-marker) {
    stroke: var(--bg-surface);
    stroke-width: 2;
    pointer-events: none;
  }

  .trend-container :global(.crosshair) {
    stroke: var(--text-muted);
    stroke-width: 1;
    stroke-dasharray: none;
    opacity: 0.5;
    pointer-events: none;
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
