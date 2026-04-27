<script lang="ts">
  import { onDestroy } from "svelte";
  import type { Virtualizer } from "@tanstack/virtual-core";
  import { messages } from "../../stores/messages.svelte.js";
  import { ui } from "../../stores/ui.svelte.js";
  import { sessions } from "../../stores/sessions.svelte.js";
  import { createVirtualizer } from "../../virtual/createVirtualizer.svelte.js";
  import MessageContent from "./MessageContent.svelte";
  import CompactBoundaryDivider from "./CompactBoundaryDivider.svelte";
  import SystemBoundaryCard from "../system/SystemBoundaryCard.svelte";
  import ToolCallGroup from "./ToolCallGroup.svelte";
  import type { Message } from "../../api/types.js";
  import {
    buildDisplayItems,
    type DisplayItem,
  } from "../../utils/display-items.js";
  import { filterDisplayItemsByTranscriptMode } from "../../utils/transcript-mode.js";
  import {
    hasVisibleSegments,
  } from "../../utils/content-parser.js";
  import { isSystemMessage } from "../../utils/messages.js";
  import { inSessionSearch } from "../../stores/inSessionSearch.svelte.js";
  import { sessionActivity } from "../../stores/sessionActivity.svelte.js";
  import SessionFindBar from "./SessionFindBar.svelte";

  let containerRef: HTMLDivElement | undefined = $state(undefined);
  let scrollRaf: number | null = $state(null);
  let lastScrollRequest = 0;

  let baseMessages: Message[] = $derived.by(() =>
    messages.messages.filter((m) => !isSystemMessage(m)),
  );

  let baseDisplayItemsAsc = $derived(
    buildDisplayItems(baseMessages),
  );

  let filteredDisplayItemsAsc = $derived(
    buildDisplayItems(baseMessages, {
      skipToolGrouping: !ui.isBlockVisible("tool"),
    }),
  );

  function isItemVisible(item: DisplayItem): boolean {
    if (item.kind === "tool-group") {
      return true;
    }
    return hasVisibleSegments(item.message, (type) =>
      ui.isBlockVisible(type),
    );
  }

  let normalDisplayItemsAsc = $derived.by(() => {
    if (!ui.hasBlockFilters) return baseDisplayItemsAsc;
    return filteredDisplayItemsAsc.filter(isItemVisible);
  });

  let displayItemsAsc = $derived.by(() => {
    if (ui.transcriptMode === "normal") {
      return normalDisplayItemsAsc;
    }

    if (!ui.hasBlockFilters) {
      return filterDisplayItemsByTranscriptMode(
        baseDisplayItemsAsc,
        "focused",
      );
    }

    return filterDisplayItemsByTranscriptMode(
      filteredDisplayItemsAsc,
      "focused",
      {
        isMessageVisible: (message) =>
          hasVisibleSegments(message, (type) =>
            ui.isBlockVisible(type),
          ),
      },
    ).filter(isItemVisible);
  });

  function itemAt(index: number) {
    if (ui.sortNewestFirst) {
      const mapped = displayItemsAsc.length - 1 - index;
      return displayItemsAsc[mapped];
    }
    return displayItemsAsc[index];
  }

  const virtualizer = createVirtualizer(() => {
    const count = displayItemsAsc.length;
    const el = containerRef ?? null;
    const sid = sessions.activeSessionId ?? "";
    return {
      count,
      getScrollElement: () => el,
      estimateSize: () => 120,
      overscan: 5,
      useAnimationFrameWithResizeObserver: true,
      measureCacheKey: sid,
      getItemKey: (index: number) => {
        const item = itemAt(index);
        if (!item) return `${sid}-${index}`;
        if (item.kind === "tool-group") {
          return `${sid}-tg-${item.ordinals[0]}`;
        }
        return `${sid}-m-${item.message.ordinal}`;
      },
    };
  });

  /** Svelte action: measure element for variable-height virtualizer */
  function measureElement(
    node: HTMLElement,
    virt: Virtualizer<HTMLElement, HTMLElement> | undefined,
  ) {
    virt?.measureElement(node);
    return {
      update(
        nextVirt:
          | Virtualizer<HTMLElement, HTMLElement>
          | undefined,
      ) {
        nextVirt?.measureElement(node);
      },
      destroy() {
        // Cleanup handled by virtualizer
      },
    };
  }

  function publishVisibleTimestamp() {
    const v = virtualizer.instance;
    if (!v) return;
    const items = v.getVirtualItems();
    // Skip overscanned items above the viewport.
    const scrollTop = v.scrollOffset ?? 0;
    for (const vi of items) {
      if (vi.end <= scrollTop) continue;
      const item =
        displayItemsAsc[
          ui.sortNewestFirst
            ? displayItemsAsc.length - 1 - vi.index
            : vi.index
        ];
      if (!item) continue;
      const ts =
        item.kind === "message"
          ? item.message.timestamp
          : item.timestamp;
      if (ts) {
        sessionActivity.firstVisibleTimestamp = ts;
        return;
      }
    }
    sessionActivity.firstVisibleTimestamp = null;
  }

  // Recompute visible timestamp when minimap opens or
  // message content changes (e.g. SSE reload).
  $effect(() => {
    if (ui.vitalsOpen) {
      // Track message array so the effect re-runs after
      // content changes while the minimap is open.
      void messages.messages.length;
      publishVisibleTimestamp();
    }
  });

  function handleScroll() {
    if (!containerRef) return;
    if (scrollRaf !== null) return;
    scrollRaf = requestAnimationFrame(() => {
      scrollRaf = null;
      if (!containerRef) return;
      const items =
        virtualizer.instance?.getVirtualItems() ?? [];
      if (items.length > 0 && messages.hasOlder) {
        const firstVisible = items[0]!.index;
        const lastVisible =
          items[items.length - 1]!.index;
        const threshold = 30;
        if (
          (ui.sortNewestFirst &&
            lastVisible >=
              displayItemsAsc.length - threshold) ||
          (!ui.sortNewestFirst &&
            firstVisible <= threshold)
        ) {
          messages.loadOlder();
        }
      }

      if (ui.vitalsOpen) {
        publishVisibleTimestamp();
      }
    });
  }

  onDestroy(() => {
    if (scrollRaf !== null) {
      cancelAnimationFrame(scrollRaf);
      scrollRaf = null;
    }
  });

  function scrollToDisplayIndex(
    index: number,
    waitFrames: number = 0,
    scrollRetries: number = 0,
    reqId: number = lastScrollRequest,
  ) {
    if (reqId !== lastScrollRequest) return;

    const v = virtualizer.instance;
    if (!v) return;

    // Phase 1: wait up to 5 frames for virtualCount to sync.
    const desiredCount = displayItemsAsc.length;
    const virtualCount = v.options.count;
    if (
      waitFrames < 5 &&
      (virtualCount !== desiredCount || index >= virtualCount)
    ) {
      requestAnimationFrame(() => {
        scrollToDisplayIndex(
          index, waitFrames + 1, 0, reqId,
        );
      });
      return;
    }

    // Phase 2a: item already rendered — use exact measured offset.
    const virtualItems = v.getVirtualItems();
    const isRendered = virtualItems.some(
      (vi) => vi.index === index,
    );
    if (isRendered) {
      const offsetAndAlign =
        v.getOffsetForIndex(index, "start");
      if (offsetAndAlign) {
        const [offset] = offsetAndAlign;
        v.scrollToOffset(
          Math.round(offset),
          { align: "start" },
        );
      }
      return;
    }

    // Phase 2b: item not yet in render window. scrollToIndex
    // scrolls to an estimated position, but TanStack's reconcile
    // loop exits after 1 stable frame — before ResizeObserver
    // measurements (delayed by bumpVersion's setTimeout(0)) have
    // updated the offsets.
    //
    // Retry in 2 frames: by then ResizeObserver + bumpVersion have
    // fired, measurements are updated, and the next attempt either
    // finds the item rendered (for an exact offset scroll) or
    // repeats with a more accurate estimate. Limit to 15 scroll
    // retries (~480 ms) to avoid looping forever.
    v.scrollToIndex(index, { align: "start" });
    if (scrollRetries < 15) {
      requestAnimationFrame(() => {
        requestAnimationFrame(() => {
          scrollToDisplayIndex(
            index, waitFrames, scrollRetries + 1, reqId,
          );
        });
      });
    }
  }

  function raf(): Promise<void> {
    return new Promise((r) => requestAnimationFrame(() => r()));
  }

  async function scrollToOrdinalInternal(ordinal: number) {
    const reqId = ++lastScrollRequest;

    const idxAsc = displayItemsAsc.findIndex((item) =>
      item.ordinals.includes(ordinal),
    );
    if (idxAsc >= 0) {
      const idx = ui.sortNewestFirst
        ? displayItemsAsc.length - 1 - idxAsc
        : idxAsc;
      scrollToDisplayIndex(idx, 0, 0, reqId);
      return;
    }

    await messages.ensureOrdinalLoaded(ordinal);
    if (reqId !== lastScrollRequest) return;

    // Let Svelte re-derive displayItemsAsc and the
    // virtualizer update its count after loading.
    // Two frames: one for Svelte reactivity, one for
    // virtualizer resize observation.
    await raf();
    await raf();
    if (reqId !== lastScrollRequest) return;

    const loadedIdxAsc = displayItemsAsc.findIndex(
      (item) => item.ordinals.includes(ordinal),
    );
    if (loadedIdxAsc < 0) return;
    const loadedIdx = ui.sortNewestFirst
      ? displayItemsAsc.length - 1 - loadedIdxAsc
      : loadedIdxAsc;
    scrollToDisplayIndex(loadedIdx, 0, 0, reqId);
  }

  export function scrollToOrdinal(ordinal: number) {
    void scrollToOrdinalInternal(ordinal);
  }

  export function getDisplayItems(): DisplayItem[] {
    return displayItemsAsc;
  }

  export function getNormalDisplayItems(): DisplayItem[] {
    return normalDisplayItemsAsc;
  }

  let highlightQuery = $derived(
    inSessionSearch.isOpen && inSessionSearch.query.trim().length > 0
      ? inSessionSearch.query
      : "",
  );
</script>

{#if !sessions.activeSessionId}
  <div class="empty-state">
    <div class="empty-icon">
      <svg width="36" height="36" viewBox="0 0 16 16" fill="var(--text-muted)">
        <path d="M14 1a1 1 0 011 1v8a1 1 0 01-1 1h-2.5a2 2 0 00-1.6.8L8 14.333 6.1 11.8a2 2 0 00-1.6-.8H2a1 1 0 01-1-1V2a1 1 0 011-1h12z"/>
      </svg>
    </div>
    <p class="empty-text">Select a session to view messages</p>
  </div>
{:else if messages.loading && messages.messages.length === 0}
  <div class="empty-state">
    <p class="empty-text">Loading messages...</p>
  </div>
{:else}
  <SessionFindBar />
  <div
    class="message-list-scroll layout-{ui.messageLayout}"
    bind:this={containerRef}
    data-session-id={sessions.activeSessionId}
    data-messages-session-id={messages.sessionId}
    data-loaded={!messages.loading}
    onscroll={handleScroll}
  >
    <div
      style="height: {virtualizer.instance?.getTotalSize() ?? 0}px; width: 100%; position: relative;"
    >
      {#each virtualizer.instance?.getVirtualItems() ?? [] as row (row.key)}
        {@const item = itemAt(row.index)}
        {#if item}
          <!-- svelte-ignore a11y_click_events_have_key_events -->
          <!-- svelte-ignore a11y_no_static_element_interactions -->
          <div
            class="virtual-row"
            class:selected={ui.selectedOrdinal !== null &&
              item.ordinals.includes(ui.selectedOrdinal)}
            data-index={row.index}
            style="position: absolute; top: 0; left: 0; width: 100%; transform: translateY({row.start}px);"
            use:measureElement={virtualizer.instance}
            onclick={() => {
              const sel = window.getSelection();
              if (sel && sel.toString().length > 0) return;
              ui.selectOrdinal(item.ordinals[0]!);
            }}
          >
            {#if item.kind === "tool-group"}
              <ToolCallGroup
                messages={item.messages}
                timestamp={item.timestamp}
                highlightQuery={highlightQuery}
                isCurrentHighlight={item.ordinals.includes(inSessionSearch.currentOrdinal ?? -1)}
              />
            {:else if item.message.is_compact_boundary}
              <CompactBoundaryDivider message={item.message} />
            {:else if item.message.is_system && item.message.source_subtype && item.message.source_subtype !== 'compact_boundary'}
              <SystemBoundaryCard
                subtype={item.message.source_subtype}
                content={item.message.content}
                timestamp={item.message.timestamp}
              />
            {:else}
              <MessageContent
                message={item.message}
                highlightQuery={highlightQuery}
                isCurrentHighlight={inSessionSearch.currentOrdinal === item.message.ordinal}
              />
            {/if}
          </div>
        {/if}
      {/each}
    </div>
  </div>
{/if}

<style>
  .message-list-scroll {
    flex: 1;
    overflow-y: auto;
    overflow-x: hidden;
    padding: 8px 0;
    overflow-anchor: none;
  }

  .virtual-row {
    padding: 5px 12px;
    overflow-anchor: none;
  }

  .virtual-row.selected > :global(*) {
    outline: 2px solid var(--accent-blue);
    outline-offset: -2px;
    border-radius: var(--radius-md, 6px);
  }

  .empty-state {
    flex: 1;
    display: flex;
    flex-direction: column;
    align-items: center;
    justify-content: center;
    color: var(--text-muted);
    gap: 12px;
  }

  .empty-icon {
    opacity: 0.25;
  }

  .empty-text {
    font-size: 14px;
    font-weight: 500;
  }

  /* ── Compact layout ── */
  .layout-compact {
    padding: 4px 0;
  }

  .layout-compact .virtual-row {
    padding: 2px 12px;
  }

  .layout-compact :global(.message) {
    padding: 6px 12px;
    border-left-width: 2px;
    border-radius: 0;
  }

  .layout-compact :global(.message-header) {
    margin-bottom: 4px;
    gap: 6px;
  }

  .layout-compact :global(.role-icon) {
    width: 16px;
    height: 16px;
    font-size: 9px;
  }

  .layout-compact :global(.role-label) {
    font-size: 10px;
    text-transform: uppercase;
    letter-spacing: 0.04em;
    font-weight: 700;
  }

  .layout-compact :global(.timestamp),
  .layout-compact :global(.group-timestamp) {
    font-size: 10px;
  }

  .layout-compact :global(.text-content) {
    font-size: 13px;
    line-height: 1.55;
  }

  .layout-compact :global(.message-body) {
    gap: 4px;
  }

  /* ── Stream layout ── */
  .layout-stream {
    padding: 0;
  }

  .layout-stream .virtual-row {
    padding: 0;
  }

  .layout-stream :global(.message) {
    border-left: none;
    border-radius: 0;
    padding: 16px 24px;
  }

  .layout-stream :global(.message.is-user) {
    background: color-mix(
      in srgb,
      var(--accent-blue) 5%,
      transparent
    ) !important;
  }

  .layout-stream :global(.message:not(.is-user)) {
    background: transparent !important;
  }

  .layout-stream :global(.message-header) {
    display: none;
  }

  .layout-stream :global(.text-content) {
    font-size: 14px;
    line-height: 1.75;
  }
</style>
