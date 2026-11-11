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
    /** Distance in px from the plot edge to the tick labels. Roomy charts
        raise it so the y "0" and the first x label do not meet. */
    labelGap?: number;
  }

  let {
    children,
    xTicks,
    yTicks,
    formatX,
    formatY,
    labelGap = 4,
  }: Props = $props();
</script>

<g class="large-chart-frame">
  <Grid x={false} y yTicks={yTicks} class="grid-line" />
  <Axis
    placement="left"
    ticks={yTicks}
    format={formatY}
    tickMarks={false}
    tickLength={labelGap}
    rule={false}
    classes={{ tickLabel: "y-label" }}
  />
  <Axis
    placement="bottom"
    ticks={xTicks}
    format={formatX}
    tickMarks={false}
    tickLength={labelGap}
    rule={false}
    classes={{ tickLabel: "x-label" }}
  />
  {@render children()}
</g>

<style>
  /* Faint solid gridlines at each y tick, so values can be read off the
     lines without hovering; they stay below the series in weight. The
     lines themselves take the stroke: layerchart colors each line through
     layered rules whose winner depends on style load order, and one order
     painted them in full text ink. */
  .large-chart-frame :global(.grid-line),
  .large-chart-frame :global(.grid-line line) {
    stroke: var(--border-muted);
    stroke-width: 1;
    shape-rendering: crispEdges;
  }

  /* Tick labels at the 11px floor used for chart text elsewhere. */
  .large-chart-frame :global(.y-label) {
    fill: var(--text-muted);
    font-family: var(--font-mono);
    font-size: 11px;
    font-variant-numeric: tabular-nums;
  }

  .large-chart-frame :global(.x-label) {
    fill: var(--text-muted);
    font-family: var(--font-sans);
    font-size: 11px;
    font-variant-numeric: tabular-nums;
  }
</style>
