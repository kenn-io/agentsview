<script lang="ts">
  import { seriesPattern, type SeriesStyle } from "@kenn-io/kit-ui/utils/palette";
  import { seriesPatternId } from "../../utils/seriesPaint.js";

  interface Props {
    /** Prefix that keeps pattern ids unique on the page; pass the same
     * value to seriesPaint. */
    scope: string;
    styles: Iterable<Pick<SeriesStyle, "color" | "fill">>;
  }

  let { scope, styles }: Props = $props();

  // One pattern per color and fill, built from the same tile kit-ui's
  // seriesSwatchBackground draws, so legends match the chart.
  const patterns = $derived.by(() => {
    const byId = new Map<
      string,
      { id: string; color: string; tile: NonNullable<ReturnType<typeof seriesPattern>> }
    >();
    for (const style of styles) {
      const tile = seriesPattern(style.fill);
      if (tile === null) continue;
      const id = seriesPatternId(scope, style);
      if (!byId.has(id)) byId.set(id, { id, color: style.color, tile });
    }
    return [...byId.values()];
  });
</script>

{#if patterns.length > 0}
  <defs>
    {#each patterns as pattern (pattern.id)}
      {@const size = pattern.tile.size}
      {@const base = pattern.tile.lines[0]?.rotate ?? 0}
      <!-- SVG rotates clockwise and CSS gradient angles turn the other way,
           so the angle is negated to match the legend swatches. -->
      <pattern
        id={pattern.id}
        patternUnits="userSpaceOnUse"
        width={size}
        height={size}
        patternTransform={`rotate(${-base})`}
      >
        <rect width={size} height={size} fill={pattern.color} />
        {#each pattern.tile.lines as line, index (index)}
          {@const across = Math.abs(line.rotate - base) % 180 === 90}
          <line
            class="series-pattern-line"
            x1={across ? 0 : size / 2}
            y1={across ? size / 2 : 0}
            x2={across ? size : size / 2}
            y2={across ? size / 2 : size}
            stroke-width={line.width}
          />
        {/each}
        {#if pattern.tile.dotRadius > 0}
          <circle
            class="series-pattern-dot"
            cx={size / 2}
            cy={size / 2}
            r={pattern.tile.dotRadius}
          />
        {/if}
      </pattern>
    {/each}
  </defs>
{/if}

<style>
  .series-pattern-line {
    stroke: var(--bg-surface);
  }

  .series-pattern-dot {
    fill: var(--bg-surface);
  }
</style>
