<script lang="ts">
  import { Bar, Chart, Layer } from "layerchart";
  import { scaleBand } from "d3-scale";
  import type { SignalsTrendBucket } from "../../api/types/analytics.js";
  import { getGradeStyle, scoreToGrade } from "../../utils/grade.js";
  import { m } from "../../i18n/index.js";
  import LargeChartFrame from "../shared/LargeChartFrame.svelte";

  interface Props {
    trend: SignalsTrendBucket[];
  }

  let { trend }: Props = $props();

  const chartData = $derived(
    trend.map((bucket) => ({
      ...bucket,
      value: bucket.avg_health_score ?? 50,
      color: bucket.avg_health_score != null
        ? getGradeStyle(scoreToGrade(bucket.avg_health_score)).bg
        : getGradeStyle(null).bg,
    })),
  );

  const xTicks = $derived(
    trend.length > 1
      ? [trend[0]!.date, trend[trend.length - 1]!.date]
      : trend.map((bucket) => bucket.date),
  );

  let tooltip = $state<{
    x: number;
    y: number;
    text: string;
  } | null>(null);

  function barTitle(bucket: SignalsTrendBucket): string {
    const score = bucket.avg_health_score;
    return `${bucket.date}: ${score != null
      ? Math.round(score)
      : m.analytics_health_trend_no_scored_sessions()} (${m.analytics_session_shape_session_count({
        count: bucket.session_count,
        countLabel: bucket.session_count.toLocaleString(),
      })})`;
  }

  function showTooltip(event: PointerEvent, bucket: SignalsTrendBucket) {
    const rect = (event.currentTarget as SVGElement).getBoundingClientRect();
    tooltip = {
      x: rect.left + rect.width / 2,
      y: rect.top - 4,
      text: barTitle(bucket),
    };
  }
</script>

<div class="health-trend">
  <div class="chart-title">{m.analytics_health_trend_title()}</div>
  {#if trend.length > 0}
    <Chart
      data={chartData}
      x="date"
      y="value"
      xScale={scaleBand().paddingInner(0.16).paddingOuter(0.04)}
      yDomain={[0, 100]}
      padding={{ top: 8, right: 12, bottom: 22, left: 32 }}
      height={126}
      class="health-chart"
      role="img"
      aria-label={m.analytics_health_trend_title()}
      aria-describedby="health-trend-data"
    >
      <Layer>
        <LargeChartFrame
          {xTicks}
          yTicks={[0, 50, 100]}
          formatX={(value) => String(value)}
          formatY={(value) => String(value)}
        >
          {#each chartData as bucket (bucket.date)}
            <Bar
              data={bucket}
              class="health-bar"
              fill={bucket.color}
              radius={2}
              onpointerenter={(event) => showTooltip(event, bucket)}
              onpointerleave={() => (tooltip = null)}
            />
          {/each}
        </LargeChartFrame>
      </Layer>
    </Chart>
    <table id="health-trend-data" class="kit-sr-only">
      <caption>{m.analytics_health_trend_title()}</caption>
      <thead>
        <tr>
          <th scope="col">{m.analytics_skill_trend_date()}</th>
          <th scope="col">{m.analytics_session_health_avg_score()}</th>
          <th scope="col">{m.analytics_col_sessions()}</th>
        </tr>
      </thead>
      <tbody>
        {#each trend as bucket (bucket.date)}
          <tr>
            <th scope="row">{bucket.date}</th>
            <td>
              {bucket.avg_health_score != null
                ? Math.round(bucket.avg_health_score)
                : m.analytics_health_trend_no_scored_sessions()}
            </td>
            <td>{bucket.session_count.toLocaleString()}</td>
          </tr>
        {/each}
      </tbody>
    </table>
    {#if tooltip}
      <div
        class="tooltip"
        style="left: {tooltip.x}px; top: {tooltip.y}px;"
      >
        {tooltip.text}
      </div>
    {/if}
    <div class="chart-caption">
      {m.analytics_health_trend_caption()}
    </div>
  {:else}
    <div class="empty">{m.analytics_no_trend_data()}</div>
  {/if}
</div>

<style>
  .chart-title {
    font-size: 12px;
    font-weight: 600;
    color: var(--text-primary);
    margin-bottom: 10px;
  }
  .health-trend :global(.health-chart) {
    display: block;
    width: 100%;
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
  .chart-caption {
    font-size: 11px;
    color: var(--text-muted);
    margin-top: 6px;
  }
  .empty {
    color: var(--text-muted);
    font-size: 12px;
  }
</style>
