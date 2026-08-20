<script lang="ts">
  import { Axis, Grid } from "layerchart";
  import type { Snippet } from "svelte";

  type Ticks = number | Array<number | string>;
  type Formatter = (value: unknown) => string;

  interface Props {
    children: Snippet;
    xTicks?: Ticks;
    yTicks?: Ticks;
    formatX?: Formatter;
    formatY?: Formatter;
  }

  let {
    children,
    xTicks,
    yTicks,
    formatX,
    formatY,
  }: Props = $props();
</script>

<g class="large-chart-frame">
  <Grid x={false} y yTicks={yTicks} class="grid-line" />
  <Axis
    placement="left"
    ticks={yTicks}
    format={formatY}
    tickMarks={false}
    rule={false}
    classes={{ tickLabel: "y-label" }}
  />
  <Axis
    placement="bottom"
    ticks={xTicks}
    format={formatX}
    tickMarks={false}
    rule={false}
    classes={{ tickLabel: "x-label" }}
  />
  {@render children()}
</g>

<style>
  .large-chart-frame :global(.grid-line) {
    stroke: var(--border-muted);
    stroke-width: 0.5;
    stroke-dasharray: 2 2;
    stroke-opacity: 0.4;
  }

  .large-chart-frame :global(.y-label) {
    fill: var(--text-muted);
    font-family: var(--font-mono);
    font-size: 9px;
    font-variant-numeric: tabular-nums;
  }

  .large-chart-frame :global(.x-label) {
    fill: var(--text-muted);
    font-family: var(--font-sans);
    font-size: 9px;
  }
</style>
