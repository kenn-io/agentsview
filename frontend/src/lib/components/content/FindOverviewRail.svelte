<script lang="ts">
  import { untrack } from "svelte";
  import type { DisplayItem } from "../../utils/display-items.js";
  import { collectSearchBlocks } from "../../search/block-text.js";
  import { nearestOverviewMatch, overviewLocations, overviewTicks, overviewY } from "../../search/overview.js";
  import { ui } from "../../stores/ui.svelte.js";
  import { inSessionSearch } from "../../stores/inSessionSearch.svelte.js";
  import { m } from "../../i18n/index.js";

  interface Props {
    items: DisplayItem[];
    totalSize: number;
    newestFirst: boolean;
    rowOffset: (index: number) => number;
  }
  let { items, totalSize, newestFirst, rowOffset }: Props = $props();
  let rail: SVGSVGElement | undefined = $state(undefined);
  let width = $state(0);
  let height = $state(0);

  let locations = $derived.by(() => {
    const ordered = newestFirst ? [...items].reverse() : items;
    const matches = inSessionSearch.matches;
    const total = totalSize;
    const renderUnknownXmlBlocksAsPreformatted = ui.renderUnknownXmlBlocksAsPreformatted;
    return untrack(() => overviewLocations(ordered.map((item, index) => {
      const offset = rowOffset(index);
      const end = index + 1 < ordered.length ? rowOffset(index + 1) : total;
      const messages = item.kind === "message" ? [item.message]
        : newestFirst ? [...item.messages].reverse() : item.messages;
      return { offset, size: Math.max(1, end - offset), blocks: messages.flatMap((message) => collectSearchBlocks(message, { renderUnknownXmlBlocksAsPreformatted })) };
    }), matches));
  });

  let ticks = $derived(overviewTicks(locations, totalSize, height));
  let currentLocation = $derived(locations.find(({ match }) =>
    match.blockKey === inSessionSearch.resolvedCurrent?.blockKey &&
    match.occurrence === inSessionSearch.resolvedCurrent?.occurrence));

  $effect(() => {
    const node = rail;
    if (!node) return;
    const measure = () => {
      width = node.clientWidth;
      height = node.clientHeight;
    };
    measure();
    const resize = new ResizeObserver(measure);
    resize.observe(node);
    return () => resize.disconnect();
  });

  function selectAt(event: MouseEvent) {
    if (!rail || !totalSize) return;
    const rect = rail.getBoundingClientRect();
    if (!rect.height) return;
    const offset = Math.max(0, Math.min(1, (event.clientY - rect.top) / rect.height)) * totalSize;
    const match = nearestOverviewMatch(locations, offset);
    if (match) inSessionSearch.goTo(match);
  }

  function navigate(event: KeyboardEvent) {
    if (!["ArrowUp", "ArrowDown", "Home", "End", "Enter", " "].includes(event.key)) return;
    event.preventDefault();
    event.stopPropagation();
    if (event.key === "ArrowUp") inSessionSearch.prev();
    else if (event.key === "ArrowDown") inSessionSearch.next();
    else if (event.key === "Home" || event.key === "End") {
      const match = event.key === "Home" ? locations[0]?.match : locations.at(-1)?.match;
      if (match) inSessionSearch.goTo(match);
    } else if (inSessionSearch.resolvedCurrent) inSessionSearch.goTo(inSessionSearch.resolvedCurrent);
  }
</script>

<svg class="find-overview-rail" bind:this={rail}
  role="button" tabindex="0" aria-label={m.session_find_rail_label({ count: inSessionSearch.total })}
  onclick={selectAt} onkeydown={navigate}>
  {#each ticks as mark}
    <rect x="2" y={mark.y} width={Math.max(1, width - 4)} height={mark.height} fill="var(--accent-amber)" />
  {/each}
  {#if currentLocation && height > 0}
    <rect x="0" y={Math.max(0, Math.min(height - 4, overviewY(currentLocation.offset, totalSize, height) - 2))}
      {width} height={Math.min(4, height)} fill="currentColor" />
  {/if}
</svg>

<style>
  .find-overview-rail {
    position: absolute;
    inset-block: 0;
    inset-inline-end: 0;
    width: 12px;
    height: 100%;
    color: var(--accent-blue);
    cursor: pointer;
  }
</style>
