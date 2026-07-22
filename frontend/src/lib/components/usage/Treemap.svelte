<script lang="ts">
  import { m } from "../../i18n/index.js";
  import { squarify } from "../../utils/treemap.js";
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
  }

  const { items, height = 260, onSelect }: Props = $props();

  let containerEl: HTMLDivElement | undefined = $state();
  let width = $state(600);

  $effect(() => {
    if (!containerEl) return;
    const ro = new ResizeObserver((entries) => {
      const entry = entries[0];
      if (entry) {
        width = Math.floor(entry.contentRect.width);
      }
    });
    ro.observe(containerEl);
    return () => ro.disconnect();
  });

  interface Tile {
    id: string;
    label: string;
    value: number;
    color: string;
    meta?: string;
    x: number;
    y: number;
    width: number;
    height: number;
  }

  const tiles = $derived.by((): Tile[] => {
    if (items.length === 0 || width <= 0 || height <= 0) {
      return [];
    }
    const input = items.map((d) => ({
      id: d.id,
      value: d.value,
    }));
    const layout = squarify(input, width, height);
    const byId = new Map(items.map((d) => [d.id, d]));
    return layout.map((t) => {
      const src = byId.get(t.id)!;
      return {
        id: t.id,
        label: src.label,
        value: src.value,
        color: src.color,
        meta: src.meta,
        x: t.x,
        y: t.y,
        width: t.width,
        height: t.height,
      };
    });
  });

  function handleKey(e: KeyboardEvent, id: string) {
    if (e.key === "Enter" || e.key === " ") {
      e.preventDefault();
      onSelect?.(id);
    }
  }
</script>

<div class="treemap-container" bind:this={containerEl}>
<svg
  width="100%"
  {height}
  class="treemap"
  viewBox="0 0 {width} {height}"
  preserveAspectRatio="none"
>
  {#each tiles as tile, i (tile.id)}
    {@const large = tile.width > 60 && tile.height > 40}
    {@const medium = tile.width > 40 && tile.height > 20}
    {@const clipId = `tile-clip-${i}`}
    <clipPath id={clipId}>
      <rect
        x={tile.x}
        y={tile.y}
        width={tile.width}
        height={tile.height}
      />
    </clipPath>
    <!-- svelte-ignore a11y_no_static_element_interactions -->
    <g
      class="tile"
      tabindex="0"
      role="button"
      aria-label={m.usage_hide_from_chart({ label: tile.label })}
      onclick={() => onSelect?.(tile.id)}
      onkeydown={(e) => handleKey(e, tile.id)}
      clip-path="url(#{clipId})"
    >
      <title>{m.usage_click_to_hide({ label: tile.label })}</title>
      <rect
        x={tile.x}
        y={tile.y}
        width={tile.width}
        height={tile.height}
        rx="3"
        fill={tile.color}
      />
      {#if large}
        <text
          x={tile.x + 6}
          y={tile.y + 16}
          class="tile-label"
        >
          {tile.label}
        </text>
        <text
          x={tile.x + 6}
          y={tile.y + 30}
          class="tile-value"
        >
          {formatMoney(moneyFromMicrodollars(tile.value))}
        </text>
        {#if tile.meta}
          <text
            x={tile.x + 6}
            y={tile.y + 42}
            class="tile-meta"
          >
            {tile.meta}
          </text>
        {/if}
      {:else if medium}
        <text
          x={tile.x + 4}
          y={tile.y + 14}
          class="tile-label-sm"
        >
          {tile.label}
        </text>
      {/if}
    </g>
  {/each}
</svg>
</div>

<style>
  .treemap-container {
    width: 100%;
    min-height: 0;
  }

  .treemap {
    display: block;
  }

  .tile {
    cursor: pointer;
  }

  .tile:hover rect {
    opacity: 0.92;
  }

  .tile:focus-visible {
    outline: none;
  }

  .tile:focus-visible rect {
    stroke: white;
    stroke-width: 2;
  }

  .tile-label {
    fill: white;
    font-size: 11px;
    font-weight: 600;
    font-family: var(--font-sans);
    pointer-events: none;
  }

  .tile-value {
    /* White regardless of theme: drawn over saturated per-agent tile fills */
    fill: white;
    fill-opacity: 0.85;
    font-size: 11px;
    font-weight: 500;
    font-family: var(--font-mono);
    pointer-events: none;
  }

  .tile-meta {
    /* White regardless of theme: drawn over saturated per-agent tile fills */
    fill: white;
    fill-opacity: 0.7;
    font-size: 9px;
    font-family: var(--font-sans);
    pointer-events: none;
  }

  .tile-label-sm {
    fill: white;
    font-size: 9px;
    font-weight: 500;
    font-family: var(--font-sans);
    pointer-events: none;
  }
</style>
