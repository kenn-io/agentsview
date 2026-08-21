<script lang="ts">
  import { Chart, Group, Layer, Rect, Text } from "layerchart";
  import { Treemap as LayerTreemap } from "layerchart/hierarchy";
  import { hierarchy } from "d3-hierarchy";
  import { m } from "../../i18n/index.js";
  import { formatMoney, moneyFromMicrodollars } from "../../money.js";

  interface TreemapItem {
    id: string;
    label: string;
    value: number;
    color: string;
    meta?: string;
  }

  interface Props {
    items: TreemapItem[];
    height?: number;
    onSelect?: (id: string) => void;
    formatValue?: (value: number) => string;
  }

  const uid = $props.id();

  function formatCost(value: number): string {
    return formatMoney(moneyFromMicrodollars(value));
  }

  const {
    items,
    height = 260,
    onSelect,
    formatValue = formatCost,
  }: Props = $props();

  const root = $derived.by(() =>
    hierarchy<{ children?: TreemapItem[]; value?: number }>({ children: items })
      .sum((item) => item.value ?? 0)
      .sort((a, b) => (b.value ?? 0) - (a.value ?? 0))
  );

  let measuredWidth = $state(0);
  const chartWidth = $derived(measuredWidth > 0 ? measuredWidth : 600);

  function handleKey(e: KeyboardEvent, id: string) {
    if (e.key === "Enter" || e.key === " ") {
      e.preventDefault();
      onSelect?.(id);
    }
  }
</script>

<div class="treemap-container" bind:clientWidth={measuredWidth}>
  <Chart width={chartWidth} {height} padding={0}>
    <Layer class="treemap" title={m.usage_treemap()}>
      <LayerTreemap hierarchy={root} padding={2}>
        {#snippet children({ nodes })}
          {#each nodes.filter((node) => node.depth === 1) as node, index ((node.data as TreemapItem).id)}
            {@const tile = node.data as TreemapItem}
            {@const tileWidth = node.x1 - node.x0}
            {@const tileHeight = node.y1 - node.y0}
            {@const large = tileWidth > 60 && tileHeight > 40}
            {@const medium = tileWidth > 40 && tileHeight > 20}
            {@const clipId = `${uid}-tile-${index}`}
            <clipPath id={clipId}>
              <rect x={node.x0} y={node.y0} width={tileWidth} height={tileHeight} />
            </clipPath>
            <g
              class="tile"
              clip-path={`url(#${clipId})`}
              tabindex={0}
              role="button"
              aria-label={m.usage_hide_from_chart({ label: tile.label })}
              onclick={() => onSelect?.(tile.id)}
              onkeydown={(event) => handleKey(event, tile.id)}
            >
              <title>{m.usage_click_to_hide({ label: tile.label })}</title>
              <Group x={node.x0} y={node.y0}>
                <Rect
                  width={tileWidth}
                  height={tileHeight}
                  rx={3}
                  fill={tile.color}
                />
                {#if large}
                  <Text value={tile.label} x={6} y={16} width={tileWidth - 12} truncate class="tile-label" />
                  <Text value={formatValue(tile.value)} x={6} y={30} width={tileWidth - 12} truncate class="tile-value" />
                  {#if tile.meta}
                    <Text value={tile.meta} x={6} y={42} width={tileWidth - 12} truncate class="tile-meta" />
                  {/if}
                {:else if medium}
                  <Text value={tile.label} x={4} y={14} width={tileWidth - 8} truncate class="tile-label-sm" />
                {/if}
              </Group>
            </g>
          {/each}
        {/snippet}
      </LayerTreemap>
    </Layer>
  </Chart>
</div>

<style>
  .treemap-container {
    width: 100%;
    min-height: 0;
  }

  .treemap-container :global(.treemap) {
    display: block;
  }

  .treemap-container :global(.tile) {
    cursor: pointer;
  }

  .treemap-container :global(.tile:hover rect) {
    opacity: 0.92;
  }

  .treemap-container :global(.tile:focus-visible) {
    outline: none;
  }

  .treemap-container :global(.tile:focus-visible rect) {
    stroke: white;
    stroke-width: 2;
  }

  .treemap-container :global(.tile-label) {
    fill: white;
    font-size: 11px;
    font-weight: 600;
    font-family: var(--font-sans);
    pointer-events: none;
  }

  .treemap-container :global(.tile-value) {
    /* White regardless of theme: drawn over saturated per-agent tile fills */
    fill: white;
    fill-opacity: 0.85;
    font-size: 11px;
    font-weight: 500;
    font-family: var(--font-mono);
    pointer-events: none;
  }

  .treemap-container :global(.tile-meta) {
    /* White regardless of theme: drawn over saturated per-agent tile fills */
    fill: white;
    fill-opacity: 0.7;
    font-size: 9px;
    font-family: var(--font-sans);
    pointer-events: none;
  }

  .treemap-container :global(.tile-label-sm) {
    fill: white;
    font-size: 9px;
    font-weight: 500;
    font-family: var(--font-sans);
    pointer-events: none;
  }
</style>
