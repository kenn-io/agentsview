<script lang="ts">
  import { Chart, Group, Layer, Rect, Text } from "layerchart";
  import { Treemap as LayerTreemap } from "layerchart/hierarchy";
  import { hierarchy } from "d3-hierarchy";
  import { m } from "../../i18n/index.js";
  import type { SeriesStyle } from "@kenn-io/kit-ui/utils/palette";
  import { formatMoney, moneyFromMicrodollars } from "../../money.js";
  import { seriesPaint } from "../../utils/seriesPaint.js";
  import SeriesPatternDefs from "../shared/SeriesPatternDefs.svelte";
  import { treemapLabelSize, type TreemapLabelScale } from "./treemapLabelSize.js";

  interface TreemapItem {
    id: string;
    label: string;
    value: number;
    style: Pick<SeriesStyle, "color" | "fill">;
    meta?: string;
    title?: string;
    selected?: boolean;
    dimmed?: boolean;
  }

  interface Props {
    items: TreemapItem[];
    height?: number;
    onSelect?: (id: string) => void;
    onOpen?: (id: string) => void;
    formatValue?: (value: number) => string;
    labelScale?: TreemapLabelScale;
  }

  const uid = $props.id();

  function formatCost(value: number): string {
    return formatMoney(moneyFromMicrodollars(value));
  }

  const {
    items,
    height = 260,
    onSelect,
    onOpen,
    formatValue = formatCost,
    labelScale = "sqrt",
  }: Props = $props();

  const root = $derived.by(() =>
    hierarchy<{ children?: TreemapItem[]; value?: number }>({ children: items })
      .sum((item) => item.value ?? 0)
      .sort((a, b) => (b.value ?? 0) - (a.value ?? 0))
  );

  const total = $derived(root.value ?? 0);

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
      <SeriesPatternDefs scope={uid} styles={items.map((item) => item.style)} />
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
            <!-- svelte-ignore a11y_no_noninteractive_tabindex (Only selectable tiles receive a button role and tab stop.) -->
            <g
              class="tile"
              class:dimmed={tile.dimmed}
              class:patterned={tile.style.fill !== "solid"}
              style:--tile-color={tile.style.color}
              clip-path={`url(#${clipId})`}
              tabindex={onSelect ? 0 : undefined}
              role={onSelect ? "button" : undefined}
              aria-pressed={onSelect ? tile.selected ?? false : undefined}
              aria-label={onSelect ? tile.title ?? m.usage_click_to_focus({ label: tile.label }) : undefined}
              onclick={onSelect ? (event) => { if (event.detail < 2 || !onOpen) onSelect(tile.id); } : undefined}
              ondblclick={onOpen ? () => onOpen(tile.id) : undefined}
              onkeydown={onSelect ? (event) => handleKey(event, tile.id) : undefined}
            >
              <title>{tile.title ?? m.usage_click_to_focus({ label: tile.label })}</title>
              <Group x={node.x0} y={node.y0}>
                <Rect
                  width={tileWidth}
                  height={tileHeight}
                  rx={3}
                  fill={seriesPaint(uid, tile.style)}
                />
                {#if large}
                  {@const fontSize = treemapLabelSize(tile.value, total, tileHeight, labelScale)}
                  <Text value={tile.label} x={6} y={fontSize + 5} width={tileWidth - 12} truncate class="tile-label" style={`font-size: ${fontSize}px`} />
                  <Text value={formatValue(tile.value)} x={6} y={fontSize + 19} width={tileWidth - 12} truncate class="tile-value" />
                  {#if tile.meta}
                    <Text value={tile.meta} x={6} y={fontSize + 31} width={tileWidth - 12} truncate class="tile-meta" />
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
    touch-action: manipulation;
  }

  /* On a patterned tile, a halo in the tile's color keeps labels clear of
     the pattern lines. */
  .treemap-container :global(.tile.patterned text) {
    paint-order: stroke;
    stroke: var(--tile-color);
    stroke-width: 3px;
    stroke-linejoin: round;
  }

  .treemap-container :global(.tile.dimmed) {
    opacity: 0.35;
  }

  .treemap-container :global(.tile[role="button"]) {
    cursor: pointer;
  }

  .treemap-container :global(.tile[role="button"]:hover rect) {
    opacity: 0.92;
  }

  .treemap-container :global(.tile rect) {
    stroke: transparent;
    stroke-width: 2;
  }

  .treemap-container :global(.tile:focus-visible) {
    outline: none;
  }

  .treemap-container :global(.tile:focus-visible rect) {
    stroke: white;
  }

  .treemap-container :global(.tile[aria-pressed="true"] rect) {
    stroke: white;
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
