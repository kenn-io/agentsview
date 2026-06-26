<script lang="ts">
  import { m } from "../../i18n/index.js";
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

  const HEIGHT = 300;
  const LEFT = 52;
  const RIGHT = 12;
  const TOP = 28;
  const BOTTOM = 34;
  const TICK_TARGET = 5;

  let containerEl: HTMLDivElement | undefined = $state();
  let containerWidth = $state(720);

  $effect(() => {
    if (!containerEl) return;
    const ro = new ResizeObserver((entries) => {
      const entry = entries[0];
      if (entry) {
        containerWidth = Math.floor(entry.contentRect.width);
      }
    });
    ro.observe(containerEl);
    return () => ro.disconnect();
  });

  const width = $derived(Math.max(containerWidth, 320));
  const plotW = $derived(Math.max(width - LEFT - RIGHT, 1));
  const plotH = HEIGHT - TOP - BOTTOM;
  const maxCount = $derived.by(() => {
    let max = 0;
    for (const item of series) {
      for (let i = 0; i < item.points.length; i++) {
        const value = pointValue(item.points[i]!.count, i);
        if (value > max) max = value;
      }
    }
    return max;
  });
  const hasData = $derived(series.some((item) => item.total > 0));
  const metricLabel = $derived(
    normalized ? m.trends_occurrences_per1k() : m.trends_occurrences(),
  );

  function niceScale(maxY: number): { step: number; max: number } {
    if (!Number.isFinite(maxY) || maxY <= 0) {
      return { step: 1, max: 5 };
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
    const step = Math.max(1, mult * base);
    return { step, max: Math.max(step, Math.ceil(maxY / step) * step) };
  }

  const scale = $derived(niceScale(maxCount));
  const yTicks = $derived.by(() => {
    const out: number[] = [];
    for (let v = 0; v <= scale.max; v += scale.step) {
      out.push(v);
    }
    if (out[out.length - 1] !== scale.max) out.push(scale.max);
    return out;
  });

  function xFor(index: number): number {
    if (buckets.length <= 1) return LEFT + plotW / 2;
    return LEFT + (index / (buckets.length - 1)) * plotW;
  }

  function yFor(count: number): number {
    return TOP + plotH - (count / scale.max) * plotH;
  }

  function pointValue(count: number, index: number): number {
    if (!normalized) return count;
    const denom = buckets[index]?.message_count ?? 0;
    if (denom <= 0) return 0;
    return (count / denom) * 1000;
  }

  function formatMetric(value: number): string {
    if (!normalized) return Math.round(value).toLocaleString();
    return value.toLocaleString(undefined, {
      maximumFractionDigits: value < 10 ? 2 : 1,
    });
  }

  function pathFor(item: TrendsSeries): string {
    return item.points
      .map((point, index) => {
        const cmd = index === 0 ? "M" : "L";
        return `${cmd}${xFor(index)},${yFor(pointValue(point.count, index))}`;
      })
      .join("");
  }

  function labelFor(date: string): string {
    const [, month, day] = date.split("-");
    return `${month}/${day}`;
  }

  function showXLabel(index: number): boolean {
    const maxLabels = width < 520 ? 4 : 7;
    const step = Math.max(1, Math.ceil(buckets.length / maxLabels));
    return index === 0 || index === buckets.length - 1 || index % step === 0;
  }
</script>

<div class="chart-wrap" bind:this={containerEl}>
  {#if buckets.length === 0 || series.length === 0}
    <div class="empty">{m.trends_no_trend_data()}</div>
  {:else}
    <svg
      class="chart"
      viewBox={`0 0 ${width} ${HEIGHT}`}
      role="img"
      aria-label={normalized
        ? m.trends_chart_aria_normalized()
        : m.trends_chart_aria()}
    >
      <text class="y-title" x={LEFT} y="12">
        {metricLabel}
      </text>

      {#each yTicks as tick}
        {@const y = yFor(tick)}
        <line
          class="grid"
          x1={LEFT}
          x2={width - RIGHT}
          y1={y}
          y2={y}
        />
        <text class="y-label" x={LEFT - 8} y={y + 4}>
          {formatMetric(tick)}
        </text>
      {/each}

      {#each buckets as bucket, index}
        {#if showXLabel(index)}
          <text class="x-label" x={xFor(index)} y={HEIGHT - 8}>
            {labelFor(bucket.date)}
          </text>
        {/if}
      {/each}

      {#each series as item, index}
        {@const d = pathFor(item)}
        {@const muted = activeTerm !== null && activeTerm !== item.term}
        <path
          d={d}
          role="presentation"
          fill="none"
          stroke="transparent"
          stroke-width="16"
          stroke-linecap="round"
          stroke-linejoin="round"
          onmouseenter={() => onHover(item.term)}
          onmouseleave={() => onHover(null)}
        />
        <path
          d={d}
          fill="none"
          stroke={colorFor(item.term, index)}
          stroke-width={activeTerm === item.term ? 3 : 2}
          stroke-opacity={muted ? 0.24 : 1}
          stroke-linecap="round"
          stroke-linejoin="round"
        />
        {#if activeTerm === item.term}
          {#each item.points as point, pointIndex}
            <circle
              cx={xFor(pointIndex)}
              cy={yFor(pointValue(point.count, pointIndex))}
              r="3"
              fill={colorFor(item.term, index)}
            />
          {/each}
        {/if}
      {/each}

      {#if !hasData}
        <text class="empty-svg" x={width / 2} y={HEIGHT / 2}>
          No occurrences in this range
        </text>
      {/if}
    </svg>
  {/if}
</div>

<style>
  .chart-wrap {
    width: 100%;
    min-height: 300px;
    border: 1px solid var(--border-default);
    border-radius: 8px;
    background: var(--bg-surface);
    overflow: hidden;
  }

  .chart {
    display: block;
    width: 100%;
    height: 300px;
  }

  .grid {
    stroke: var(--border-muted);
    stroke-width: 1;
  }

  .y-label {
    fill: var(--text-muted);
    font-size: 10px;
    text-anchor: end;
    font-variant-numeric: tabular-nums;
  }

  .y-title {
    fill: var(--text-muted);
    font-size: 10px;
    font-weight: 600;
    text-anchor: start;
  }

  .x-label {
    fill: var(--text-muted);
    font-size: 10px;
    text-anchor: middle;
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
</style>
