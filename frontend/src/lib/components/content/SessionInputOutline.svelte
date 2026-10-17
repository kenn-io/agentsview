<script lang="ts">
  import {
    SplitResizeHandle,
    type SplitResizeEvent,
  } from "@kenn-io/kit-ui";
  import { tick } from "svelte";
  import { fetchSessionInputOutline } from "../../api/inputOutline.js";
  import type { ServiceInputOutlineItem as InputOutlineItem } from "../../api/generated/index.js";
  import { m } from "../../i18n/index.js";
  import { MessageSquareTextIcon, SquareTerminalIcon } from "../../icons.js";
  import { ui } from "../../stores/ui.svelte.js";
  import {
    SESSION_INPUT_OUTLINE_HEIGHT_MIN,
    SESSION_INPUT_OUTLINE_HEIGHT_STORAGE_MAX,
    clampSessionInputOutlineHeightForLayout,
  } from "./session-input-outline-height.js";

  interface Props {
    sessionId: string;
  }

  let { sessionId }: Props = $props();

  let items = $state<InputOutlineItem[]>([]);
  let loading = $state(false);
  let error = $state<string | null>(null);

  $effect(() => {
    const controller = new AbortController();
    loading = true;
    error = null;
    items = [];

    fetchSessionInputOutline(sessionId, {
      includeForkContext: true,
      signal: controller.signal,
    })
      .then((outline) => {
        items = outline.items ?? [];
      })
      .catch((err: unknown) => {
        if (controller.signal.aborted) return;
        error = err instanceof Error ? err.message : String(err);
      })
      .finally(() => {
        if (!controller.signal.aborted) loading = false;
      });

    return () => controller.abort();
  });

  // Sizes below are CSS pixels (clientHeight/offsetHeight), not
  // getBoundingClientRect, which includes interface zoom.
  let sectionElement = $state<HTMLElement | null>(null);
  let listElement = $state<HTMLElement | null>(null);
  // Height of the vitals panel the outline scrolls in, or null until it has
  // been laid out.
  let panelHeight = $state<number | null>(null);
  let resizeStartHeight = 0;

  const panelLimit = $derived(panelHeight ?? Number.POSITIVE_INFINITY);
  // The stored height is a limit: shorter outlines keep their natural height
  // and longer ones scroll inside it.
  const maxHeight = $derived(
    clampSessionInputOutlineHeightForLayout(
      ui.sessionInputOutlineHeight,
      panelLimit,
    ),
  );
  const layoutMaxHeight = $derived(
    clampSessionInputOutlineHeightForLayout(
      SESSION_INPUT_OUTLINE_HEIGHT_STORAGE_MAX,
      panelLimit,
    ),
  );

  function measurePanelHeight(): number | null {
    const height = sectionElement?.parentElement?.clientHeight ?? 0;
    panelHeight = height > 0 ? height : null;
    return panelHeight;
  }

  $effect(() => {
    const panel = sectionElement?.parentElement;
    if (!panel) return;
    measurePanelHeight();
    const observer = new ResizeObserver(() => {
      measurePanelHeight();
    });
    observer.observe(panel);
    return () => observer.disconnect();
  });

  // Keep the selected input visible when it changes or the outline loads,
  // since the list may be scrolled away from it.
  $effect(() => {
    const list = listElement;
    const selected = ui.selectedOrdinal;
    const loaded = items;
    if (!list || selected === null || loaded.length === 0) return;
    // Let the height limit settle against the measured panel first.
    void tick().then(() => {
      if (
        listElement === list &&
        ui.selectedOrdinal === selected &&
        items === loaded
      ) {
        scrollActiveItemIntoView(list);
      }
    });
  });

  function scrollActiveItemIntoView(list: HTMLElement) {
    const item = list.querySelector<HTMLElement>(".outline-item.active");
    if (!item) return;
    // Scroll the list directly: scrollIntoView would also scroll the vitals
    // panel and the page layout around it.
    const top = item.offsetTop;
    const bottom = top + item.offsetHeight;
    const viewTop = list.scrollTop;
    const viewBottom = viewTop + list.clientHeight;
    if (top >= viewTop && bottom <= viewBottom) return;
    // Center the item so the inputs around it stay visible.
    list.scrollTop = top - (list.clientHeight - item.offsetHeight) / 2;
  }

  function handleResizeStart() {
    // Start from the rendered height so an outline shorter than its limit
    // follows the handle right away.
    const renderedHeight = sectionElement?.offsetHeight ?? 0;
    resizeStartHeight =
      renderedHeight > 0 ? Math.min(renderedHeight, maxHeight) : maxHeight;
  }

  function handleResize(event: SplitResizeEvent) {
    // The handle sits below the outline, so moving it down (positive delta)
    // makes the outline taller.
    const nextHeight = clampSessionInputOutlineHeightForLayout(
      resizeStartHeight + event.delta,
      measurePanelHeight() ?? Number.POSITIVE_INFINITY,
    );

    // Skip persisting when the clamp lands on the rendered limit so a taller
    // stored preference survives drags in a short window.
    if (nextHeight === maxHeight) return;
    ui.setSessionInputOutlineHeight(nextHeight);
  }

  function jumpTo(item: InputOutlineItem) {
    ui.scrollToOrdinal(item.ordinal);
  }
</script>

<section
  class="outline-section"
  bind:this={sectionElement}
  style:max-height={`${maxHeight}px`}
>
  <header class="outline-header">
    <span>{m.session_input_outline_title()}</span>
    {#if items.length > 0}
      <span class="outline-count">{items.length}</span>
    {/if}
  </header>

  {#if loading}
    <div class="outline-state">{m.session_input_outline_loading()}</div>
  {:else if error}
    <div class="outline-state error">{m.session_input_outline_error()}</div>
  {:else if items.length === 0}
    <div class="outline-state">{m.session_input_outline_empty()}</div>
  {:else}
    <div class="outline-list" bind:this={listElement}>
      {#each items as item (item.ordinal)}
        <button
          type="button"
          class="outline-item"
          class:active={ui.selectedOrdinal === item.ordinal}
          title={m.session_input_outline_jump_to({
            preview: item.preview,
          })}
          onclick={() => jumpTo(item)}
        >
          <span class="outline-icon" class:shell={item.is_shell}>
            {#if item.is_shell}
              <SquareTerminalIcon size="12" strokeWidth="2" aria-hidden="true" />
            {:else}
              <MessageSquareTextIcon size="12" strokeWidth="2" aria-hidden="true" />
            {/if}
          </span>
          <span class="outline-preview" class:shell={item.is_shell}>
            {item.preview}
          </span>
          <span class="outline-ordinal">#{item.ordinal}</span>
        </button>
      {/each}
    </div>
  {/if}
</section>
{#if items.length > 0}
  <div class="outline-resize">
    <SplitResizeHandle
      orientation="vertical"
      ariaLabel={m.session_input_outline_resize()}
      ariaValueMin={SESSION_INPUT_OUTLINE_HEIGHT_MIN}
      ariaValueMax={layoutMaxHeight}
      ariaValueNow={maxHeight}
      onResizeStart={handleResizeStart}
      onResize={handleResize}
      onResizeEnd={handleResize}
    />
  </div>
{/if}

<style>
  .outline-section {
    display: flex;
    flex-direction: column;
    padding: 10px 14px 12px;
    border-bottom: 1px solid var(--border-muted);
  }

  /* The vitals panel is block layout; a flex column stretches the handle
     across it. */
  .outline-resize {
    display: flex;
    flex-direction: column;
  }

  .outline-header {
    flex-shrink: 0;
    color: var(--text-muted);
    font-size: 9px;
    text-transform: uppercase;
    letter-spacing: 0.6px;
    margin-bottom: 8px;
    font-weight: 500;
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: 8px;
  }

  .outline-count {
    color: var(--text-muted);
    font-family: var(--font-mono);
    font-size: 9px;
    text-transform: none;
    letter-spacing: 0;
  }

  .outline-state {
    color: var(--text-muted);
    font-size: 11px;
    line-height: 1.35;
    padding: 2px 0;
  }

  .outline-state.error {
    color: var(--slow-fg);
  }

  .outline-list {
    /* Offset parent for the items, so their offsetTop is in scroll space. */
    position: relative;
    min-height: 0;
    overflow-y: auto;
    /* Room for the focus ring, which the scrolling list would clip. */
    margin: -4px;
    padding: 4px;
    display: grid;
    gap: 4px;
  }

  .outline-item {
    width: 100%;
    min-height: 28px;
    display: grid;
    grid-template-columns: 16px minmax(0, 1fr) auto;
    align-items: center;
    gap: 6px;
    padding: 4px 6px;
    border-radius: var(--radius-sm);
    border: 1px solid transparent;
    background: transparent;
    color: var(--text-secondary);
    text-align: left;
    cursor: pointer;
    transition: background 0.12s, border-color 0.12s, color 0.12s;
  }

  .outline-item:hover {
    background: var(--bg-surface-hover);
    color: var(--text-primary);
  }

  .outline-item.active {
    color: var(--text-primary);
    background: color-mix(in srgb, var(--accent-blue) 12%, transparent);
    border-color: color-mix(in srgb, var(--accent-blue) 32%, transparent);
  }

  .outline-item:focus-visible {
    outline: 2px solid var(--accent-blue);
    outline-offset: 2px;
  }

  .outline-icon {
    width: 16px;
    height: 16px;
    display: inline-flex;
    align-items: center;
    justify-content: center;
    color: var(--text-muted);
  }

  .outline-icon.shell {
    color: var(--accent-green);
  }

  .outline-preview {
    min-width: 0;
    overflow: hidden;
    white-space: nowrap;
    text-overflow: ellipsis;
    font-size: 11px;
    line-height: 1.35;
  }

  .outline-preview.shell {
    font-family: var(--font-mono);
    font-size: 10px;
  }

  .outline-ordinal {
    color: var(--text-muted);
    font-family: var(--font-mono);
    font-size: 9px;
  }
</style>
