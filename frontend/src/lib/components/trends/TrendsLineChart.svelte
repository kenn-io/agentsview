<script lang="ts">
  import { Chart, Circle, Layer, Spline } from "layerchart";
  import { scalePoint } from "d3-scale";
  import LargeChartFrame from "../shared/LargeChartFrame.svelte";
  import { formatDateTime, getLocale, m } from "../../i18n/index.js";
  import type {
    TrendsBucket,
    TrendsSeries,
  } from "../../api/types.js";

  interface Props {
    buckets: TrendsBucket[];
    series: TrendsSeries[];
    colorFor: (term: string, index: number) => string;
    activeTerm: string | null;
    normalized: boolean;
    onHover: (term: string | null) => void;
  }

  let {
    buckets,
    series,
    colorFor,
    activeTerm,
    normalized,
    onHover,
  }: Props = $props();

  const hasData = $derived(series.some((item) => item.total > 0));
  const metricLabel = $derived(
    normalized ? m.trends_occurrences_per1k() : m.trends_occurrences(),
  );

  function pointValue(count: number, index: number): number {
    if (!normalized) return count;
    const denom = buckets[index]?.message_count ?? 0;
    if (denom <= 0) return 0;
    return (count / denom) * 1000;
  }

  function formatMetric(value: number): string {
    if (!normalized) return Math.round(value).toLocaleString(getLocale());
    return value.toLocaleString(getLocale(), {
      maximumFractionDigits: value < 10 ? 2 : 1,
    });
  }

  function labelFor(date: string): string {
    return formatDateTime(`${date}T00:00:00`, {
      month: "numeric",
      day: "numeric",
    });
  }

  const chartSeries = $derived(
    series.map((item, seriesIndex) => ({
      ...item,
      color: colorFor(item.term, seriesIndex),
      points: item.points.map((point, index) => ({
        date: buckets[index]?.date ?? String(index),
        value: pointValue(point.count, index),
      })),
    })),
  );

  const flatPoints = $derived(chartSeries.flatMap((item) => item.points));

  const xTicks = $derived.by(() => {
    const step = Math.max(1, Math.ceil(buckets.length / 7));
    return buckets
      .filter((_, index) =>
        index === 0 || index === buckets.length - 1 || index % step === 0
      )
      .map((bucket) => bucket.date);
  });
</script>

<div class="chart-wrap">
  {#if buckets.length === 0 || series.length === 0}
    <div class="empty">{m.trends_no_trend_data()}</div>
  {:else}
    <div class="y-title">{metricLabel}</div>
    <Chart
      data={flatPoints}
      x="date"
      y="value"
      xScale={scalePoint().padding(0.1)}
      yDomain={[0, null]}
      yNice
      padding={{ top: 28, right: 12, bottom: 34, left: 52 }}
      height={300}
      class="chart"
      role="img"
      aria-label={normalized
        ? m.trends_chart_aria_normalized()
        : m.trends_chart_aria()}
    >
      <Layer>
        <LargeChartFrame
          xTicks={xTicks}
          yTicks={5}
          formatX={(value) => labelFor(String(value))}
          formatY={(value) => formatMetric(Number(value))}
        >
          {#each chartSeries as item (item.term)}
            <Spline
              data={item.points}
              x="date"
              y="value"
              fill="none"
              stroke={item.color}
              strokeWidth={activeTerm === item.term ? 3 : 2}
              strokeOpacity={activeTerm !== null && activeTerm !== item.term ? 0.24 : 1}
              stroke-linecap="round"
              stroke-linejoin="round"
            />
            <Spline
              data={item.points}
              x="date"
              y="value"
              fill="none"
              stroke="transparent"
              strokeWidth={16}
              stroke-linecap="round"
              stroke-linejoin="round"
              data-trend-hit={item.term}
              onmouseenter={() => onHover(item.term)}
              onmouseleave={() => onHover(null)}
            />
            {#if activeTerm === item.term}
              <Circle
                data={item.points}
                x="date"
                y="value"
                r={3}
                fill={item.color}
                pointer-events="none"
              />
            {/if}
          {/each}
        </LargeChartFrame>
      </Layer>
    </Chart>
    {#if !hasData}
      <div class="empty-svg">No occurrences in this range</div>
    {/if}
  {/if}
</div>

<style>
  .chart-wrap {
    position: relative;
    width: 100%;
    min-height: 300px;
    border: 1px solid var(--border-default);
    border-radius: 8px;
    background: var(--bg-surface);
    overflow: hidden;
  }

  .chart-wrap :global(.chart) {
    display: block;
    width: 100%;
    height: 300px;
  }

  .y-title {
    position: absolute;
    top: 8px;
    left: 52px;
    z-index: 1;
    color: var(--text-muted);
    font-size: 10px;
    font-weight: 600;
    text-anchor: start;
  }

  .empty,
  .empty-svg {
    color: var(--text-muted);
    fill: var(--text-muted);
    font-size: 12px;
    text-anchor: middle;
  }

  .empty {
    height: 300px;
    display: grid;
    place-items: center;
  }

  .empty-svg {
    position: absolute;
    inset: 0;
    display: grid;
    place-items: center;
    pointer-events: none;
  }
</style>
