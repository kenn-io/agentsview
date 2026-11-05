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
    const scope = inSessionSearch.scope;
    const total = totalSize;
    const renderUnknownXmlBlocksAsPreformatted = ui.renderUnknownXmlBlocksAsPreformatted;
    return untrack(() => overviewLocations(ordered.map((item, index) => {
      const offset = rowOffset(index);
      const end = index + 1 < ordered.length ? rowOffset(index + 1) : total;
      const messages = item.kind === "message" ? [item.message]
        : newestFirst ? [...item.messages].reverse() : item.messages;
      return { offset, size: Math.max(1, end - offset), blocks: messages.flatMap((message) =>
        collectSearchBlocks(message, { renderUnknownXmlBlocksAsPreformatted })
          .filter((block) => scope?.allowsBlock(message, block.kind))) };
    }), matches));
  });

  const TRACK_WIDTH = 6;
  let trackX = $derived(Math.max(0, (width - TRACK_WIDTH) / 2));
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
  <!-- A faint full-height track makes the ticks read as a map of the
       transcript's scroll extent; ticks use the in-text match amber. -->
  <rect class="track" x={trackX} y="0" width={TRACK_WIDTH} height={Math.max(0, height)} rx={TRACK_WIDTH / 2} />
  {#each ticks as mark}
    <rect class="tick" x={trackX} y={mark.y} width={TRACK_WIDTH} height={mark.height} />
  {/each}
  {#if currentLocation && height > 0}
    <rect class="tick current" x={trackX - 1}
      y={Math.max(0, Math.min(height - 3, Math.round(overviewY(currentLocation.offset, totalSize, height) - 1.5)))}
      width={TRACK_WIDTH + 2} height={Math.min(3, height)} rx="1" />
  {/if}
</svg>

<style>
  .find-overview-rail {
    position: absolute;
    inset-block: 0;
    /* On wide panes the rail sits in the gutter 10px past the reading
       column's edge, next to the content it indexes, without moving the
       column; on narrow panes it falls back to the space reserved at the
       pane edge (SessionFindView .has-find). */
    inset-inline-end: max(0px, calc((100% - var(--reading-column)) / 2 - 22px));
    width: 12px;
    height: 100%;
    cursor: pointer;
  }
  .track { fill: color-mix(in srgb, var(--text-primary) 7%, transparent); }
  /* Ticks match the in-text highlight: other matches in a softer amber,
     the current match in solid amber, a pixel wider and taller, as its
     in-text frame is the stronger mark. */
  .tick { fill: color-mix(in srgb, var(--accent-amber) 72%, transparent); }
  .tick.current { fill: var(--accent-amber); }
</style>
