<script lang="ts">
  import { Chart, Layer, Spline } from "layerchart";
  import { scaleUtc } from "d3-scale";
  import LargeChartFrame from "../shared/LargeChartFrame.svelte";
  import { formatDateTime, m } from "../../i18n/index.js";
  import type { RateLimitWindow } from "../../stores/ratelimits.svelte.js";

  interface Props {
    snapshots: RateLimitWindow[];
    color: string;
  }

  let { snapshots, color }: Props = $props();

  // observedAt is plotted as a real instant (milliseconds since epoch),
  // not a categorical position: a point scale would space every
  // observation evenly regardless of how much time actually separated
  // them, misrepresenting bursts and gaps in the real observation
  // cadence.
  const points = $derived(
    snapshots.map((snap) => ({
      observedAt: new Date(snap.observedAt).getTime(),
      value: snap.usedPercent,
    })),
  );

  const xTicks = $derived.by(() => {
    if (points.length <= 1) return points.map((p) => p.observedAt);
    return [points[0]!.observedAt, points[points.length - 1]!.observedAt];
  });

  function labelFor(observedAt: number): string {
    return formatDateTime(observedAt, { month: "numeric", day: "numeric" });
  }
</script>

{#if points.length > 1}
  <Chart
    data={points}
    x="observedAt"
    y="value"
    xScale={scaleUtc()}
    yDomain={[0, 100]}
    padding={{ top: 6, right: 8, bottom: 18, left: 28 }}
    height={72}
    class="rate-limit-history-chart"
    role="img"
    aria-label={m.rate_limits_history_aria()}
  >
    <Layer>
      <LargeChartFrame
        {xTicks}
        yTicks={[0, 100]}
        formatX={(value) => labelFor(Number(value))}
        formatY={(value) => `${value}%`}
      >
        <Spline
          data={points}
          x="observedAt"
          y="value"
          fill="none"
          stroke={color}
          strokeWidth={2}
          stroke-linecap="round"
          stroke-linejoin="round"
        />
      </LargeChartFrame>
    </Layer>
  </Chart>
{/if}

<style>
  :global(.rate-limit-history-chart) {
    display: block;
    width: 100%;
  }
</style>
