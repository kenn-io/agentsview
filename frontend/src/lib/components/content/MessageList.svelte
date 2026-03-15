<script lang="ts">
  import { onDestroy } from "svelte";
  import type { Virtualizer } from "@tanstack/virtual-core";
  import { messages } from "../../stores/messages.svelte.js";
  import { ui } from "../../stores/ui.svelte.js";
  import { sessions } from "../../stores/sessions.svelte.js";
  import { createVirtualizer } from "../../virtual/createVirtualizer.svelte.js";
  import MessageContent from "./MessageContent.svelte";
  import ToolCallGroup from "./ToolCallGroup.svelte";
  import type { Message } from "../../api/types.js";
  import {
    buildDisplayItems,
    type DisplayItem,
  } from "../../utils/display-items.js";
  import {
    hasVisibleSegments,
  } from "../../utils/content-parser.js";
  import { inSessionSearch } from "../../stores/inSessionSearch.svelte.js";
  import SessionFindBar from "./SessionFindBar.svelte";

  let containerRef: HTMLDivElement | undefined = $state(undefined);
  let scrollRaf: number | null = $state(null);
  let lastScrollRequest = 0;

  const SYSTEM_MSG_PREFIXES = [
    "This session is being continued",
    "[Request interrupted",
    "<task-notification>",
    "<command-message>",
    "<command-name>",
    "<local-command-",
    "Stop hook feedback:",
  ];

  function isSystemMessage(m: Message): boolean {
    if (m.role !== "user") return false;
    const trimmed = m.content.trim();
    return SYSTEM_MSG_PREFIXES.some(
      (p) => trimmed.startsWith(p),
    );
  }

  let filteredMessages: Message[] = $derived.by(() => {
    let msgs = messages.messages;

    // Filter system-injected user messages
    msgs = msgs.filter((m) => !isSystemMessage(m));

    // Hide messages where all segments are filtered out
    // (e.g. hiding "Assistant text" still shows code/tool/thinking
    // nested inside assistant messages, but hides pure-text ones)
    if (ui.hasBlockFilters) {
      msgs = msgs.filter((m) =>
        hasVisibleSegments(m, (type) => ui.isBlockVisible(type)),
      );
    }

    return msgs;
  });

  let displayItemsAsc = $derived(
    buildDisplayItems(filteredMessages, {
      skipToolGrouping: !ui.isBlockVisible("tool"),
    }),
  );

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

  function handleScroll() {
    if (!containerRef) return;
    if (scrollRaf !== null) return;
    scrollRaf = requestAnimationFrame(() => {
      scrollRaf = null;
      if (!containerRef) return;
      const items = virtualizer.instance?.getVirtualItems() ?? [];
      if (items.length > 0 && messages.hasOlder) {
        const firstVisible = items[0]!.index;
        const lastVisible = items[items.length - 1]!.index;
        const threshold = 30;
        if (
          (ui.sortNewestFirst &&
            lastVisible >= displayItemsAsc.length - threshold) ||
          (!ui.sortNewestFirst && firstVisible <= threshold)
        ) {
          messages.loadOlder();
        }
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
    attempt: number = 0,
  ) {
    const v = virtualizer.instance;
    if (!v) return;

    const desiredCount = displayItemsAsc.length;
    const virtualCount = v.options.count;
    if (
      attempt < 5 &&
      (virtualCount !== desiredCount || index >= virtualCount)
    ) {
      requestAnimationFrame(() => {
        scrollToDisplayIndex(index, attempt + 1);
      });
      return;
    }

    // TanStack's scrollToIndex may continuously re-seek
    // in dynamic mode. Use one offset seek to avoid
    // visible scroll "fight."
    const offsetAndAlign =
      v.getOffsetForIndex(index, "start");
    if (offsetAndAlign) {
      const [offset] = offsetAndAlign;
      v.scrollToOffset(
        Math.round(offset),
        { align: "start" },
      );
      return;
    }

    // Item not yet measured — use scrollToIndex which will
    // estimate and then correct once measured.
    v.scrollToIndex(index, { align: "start" });
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
      scrollToDisplayIndex(idx);
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
    scrollToDisplayIndex(loadedIdx);
  }

  export function scrollToOrdinal(ordinal: number) {
    void scrollToOrdinalInternal(ordinal);
  }

  export function getDisplayItems(): DisplayItem[] {
    return displayItemsAsc;
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
