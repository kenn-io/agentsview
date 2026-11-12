<!-- Floating outline of a session's prompts. Each entry jumps to its
     prompt, including prompts older than the loaded part of a large
     session; the message list loads them on the way. -->
<script lang="ts">
  import { tick, untrack } from "svelte";
  import { Button, IconButton } from "@kenn-io/kit-ui";
  import { m } from "../../i18n/index.js";
  import { XIcon } from "../../icons.js";
  import { messages } from "../../stores/messages.svelte.js";
  import { sessionToc } from "../../stores/sessionToc.svelte.js";
  import { ui } from "../../stores/ui.svelte.js";
  import { formatNumber } from "../../utils/format.js";
  import {
    currentEntryIndex,
    entryVisible,
    mergeTocEntries,
    SESSION_TOC_ID as TOC_ID,
    type TocEntry,
  } from "./session-toc.js";

  let listEl: HTMLOListElement | undefined = $state();
  let focusedOrdinal: number | null = $state(null);

  let oldestLoaded = $derived(messages.messages[0]?.ordinal ?? 0);
  let needsOlder = $derived(messages.hasOlder && oldestLoaded > 0);
  let complete = $derived(
    !needsOlder || (messages.sessionId !== null && sessionToc.covers(messages.sessionId, oldestLoaded)),
  );

  // Every prompt, oldest first. Numbers come from this list, so a prompt
  // keeps its number when the block filter hides others.
  let allEntries = $derived(
    sessionToc.open ? mergeTocEntries(sessionToc.olderFor(messages.sessionId), messages.messages) : [],
  );
  // The prompts the transcript shows under the block filter.
  let entries = $derived(allEntries.filter((entry) => entryVisible(entry, (type) => ui.isBlockVisible(type))));
  let ordered: TocEntry[] = $derived(ui.sortNewestFirst ? [...entries].reverse() : entries);
  let currentOrdinal = $derived.by(() => {
    const index = currentEntryIndex(entries, sessionToc.view, sessionToc.picked);
    return index >= 0 ? entries[index]!.ordinal : null;
  });
  let currentIndex = $derived(
    currentOrdinal === null ? -1 : ordered.findIndex((entry) => entry.ordinal === currentOrdinal),
  );
  let promptNumbers = $derived(new Map(allEntries.map((entry, index) => [entry.ordinal, index + 1])));
  let tabOrdinal = $derived.by(() => {
    if (focusedOrdinal !== null && entries.some((entry) => entry.ordinal === focusedOrdinal)) {
      return focusedOrdinal;
    }
    return currentOrdinal ?? ordered[0]?.ordinal ?? null;
  });
  let olderLoading = $derived(needsOlder && sessionToc.olderLoading);
  let olderFailed = $derived(needsOlder && sessionToc.olderFailed);

  $effect(() => {
    const sessionId = messages.sessionId;
    const before = oldestLoaded;
    // A retry clears the failure, and a finished read may cover less than
    // a newer window needs, so both run this again.
    void sessionToc.olderFailed;
    void sessionToc.olderLoading;
    if (!sessionToc.open || !sessionId || messages.loading || !needsOlder) return;
    untrack(() => void sessionToc.ensureOlder(sessionId, before));
  });

  $effect(() => {
    void messages.sessionId;
    focusedOrdinal = null;
    focusPending = false;
  });

  // Keep the current entry in the list's view without scrolling anything
  // else on the page, as scrollIntoView would. It runs when the current
  // entry changes or moves in the list, such as when earlier prompts
  // arrive, but not on every transcript refresh, so someone browsing the
  // list keeps their place.
  $effect(() => {
    if (!sessionToc.open) return;
    const ordinal = currentOrdinal;
    const list = listEl;
    void currentIndex;
    if (ordinal === null || !list) return;
    const button = entryButton(ordinal);
    if (!button) return;
    const top = button.offsetTop;
    const bottom = top + button.offsetHeight;
    if (top < list.scrollTop) list.scrollTop = top;
    else if (bottom > list.scrollTop + list.clientHeight) list.scrollTop = bottom - list.clientHeight;
  });

  // Opening the outline from the keyboard moves focus into it, as soon as
  // it has an entry to focus; a large session may have none until earlier
  // prompts arrive. A request made before this panel mounted, such as on
  // another page, is not one.
  let handledFocusRequest = untrack(() => sessionToc.focusRequest);
  let focusPending = $state(false);
  $effect(() => {
    const request = sessionToc.focusRequest;
    if (request === handledFocusRequest) return;
    handledFocusRequest = request;
    focusPending = untrack(() => sessionToc.open);
  });
  $effect(() => {
    if (!focusPending) return;
    if (!sessionToc.open) {
      focusPending = false;
      return;
    }
    const ordinal = tabOrdinal;
    if (ordinal === null) return;
    void tick().then(() => {
      if (!focusPending || !sessionToc.open) return;
      focusPending = false;
      entryButton(ordinal)?.focus();
    });
  });

  function entryButton(ordinal: number): HTMLButtonElement | null {
    return listEl?.querySelector<HTMLButtonElement>(`button[data-ordinal="${ordinal}"]`) ?? null;
  }

  function jump(entry: TocEntry) {
    focusedOrdinal = entry.ordinal;
    sessionToc.picked = entry.ordinal;
    ui.scrollToOrdinal(entry.ordinal);
  }

  function close() {
    sessionToc.setOpen(false);
    document.querySelector<HTMLElement>(`[aria-controls="${TOC_ID}"]`)?.focus();
  }

  function moveFocus(index: number) {
    const entry = ordered[Math.max(0, Math.min(ordered.length - 1, index))];
    if (!entry) return;
    focusedOrdinal = entry.ordinal;
    entryButton(entry.ordinal)?.focus();
  }

  function handleKeydown(event: KeyboardEvent) {
    if (event.metaKey || event.ctrlKey || event.altKey) return;
    // Escape, or the shortcut that opened the outline, closes it and hands
    // focus back to its toggle.
    if (event.key === "Escape" || event.key === "t") {
      event.preventDefault();
      event.stopPropagation();
      close();
      return;
    }
    const target = event.target;
    if (!(target instanceof HTMLButtonElement) || target.dataset.ordinal === undefined) return;
    const index = ordered.findIndex((entry) => entry.ordinal === Number(target.dataset.ordinal));
    const next: Record<string, number> = {
      ArrowDown: index + 1,
      ArrowUp: index - 1,
      Home: 0,
      End: ordered.length - 1,
    };
    if (!(event.key in next)) return;
    // The list owns these keys while it has focus; the transcript's own
    // arrow-key navigation would otherwise run as well.
    event.preventDefault();
    event.stopPropagation();
    moveFocus(next[event.key]!);
  }
</script>

{#if sessionToc.open}
  <!-- svelte-ignore a11y_no_noninteractive_element_interactions -->
  <nav
    id={TOC_ID}
    class="session-toc"
    aria-label={m.session_toc_title()}
    data-testid="session-toc"
    onkeydown={handleKeydown}
  >
    <div class="toc-header">
      <span class="toc-title">{m.session_toc_title()}</span>
      {#if complete && entries.length > 0}
        <span class="toc-count">
          {m.session_toc_count({ count: entries.length, countLabel: formatNumber(entries.length) })}
        </span>
      {/if}
      <IconButton size="sm" ariaLabel={m.session_toc_close()} onclick={close} class="toc-close">
        <XIcon size="14" aria-hidden="true" />
      </IconButton>
    </div>

    {#if allEntries.length > 0 && entries.length === 0}
      <p class="toc-note">{m.session_toc_hidden_by_filter()}</p>
      <!-- Earlier prompts may still be ones the filter shows. -->
      {@render olderStatus()}
    {:else if entries.length === 0 && !olderLoading && !olderFailed}
      <p class="toc-note">{m.session_toc_empty()}</p>
    {:else}
      {#if ui.sortNewestFirst}
        {@render olderStatus()}
      {/if}
      <ol class="toc-list" bind:this={listEl}>
        {#each ordered as entry (entry.ordinal)}
          {@const current = entry.ordinal === currentOrdinal}
          <li>
            <button
              type="button"
              class="toc-entry"
              class:current
              data-ordinal={entry.ordinal}
              aria-current={current ? "location" : undefined}
              tabindex={entry.ordinal === tabOrdinal ? 0 : -1}
              title={entry.label || undefined}
              onclick={() => jump(entry)}
              onfocus={() => (focusedOrdinal = entry.ordinal)}
            >
              {#if complete}
                <span class="toc-number">{formatNumber(promptNumbers.get(entry.ordinal) ?? 0)}</span>
              {/if}
              {#if entry.label}
                <span class="toc-label" class:shell={entry.isShell}>{entry.label}</span>
              {:else}
                <span class="toc-label empty">{m.session_toc_empty_prompt()}</span>
              {/if}
            </button>
          </li>
        {/each}
      </ol>
      {#if !ui.sortNewestFirst}
        {@render olderStatus()}
      {/if}
    {/if}
  </nav>
{/if}

{#snippet olderStatus()}
  {#if olderLoading}
    <p class="toc-note" role="status">{m.session_toc_loading_older()}</p>
  {:else if olderFailed}
    <div class="toc-note toc-failed" role="alert">
      <span>{m.session_toc_load_failed()}</span>
      <Button size="sm" onclick={() => sessionToc.retryOlder()}>{m.shared_retry()}</Button>
    </div>
  {/if}
{/snippet}

<style>
  /* Floats over the transcript's right gutter, clear of a classic
     scrollbar and the find overview rail. */
  .session-toc {
    position: absolute;
    top: 8px;
    right: calc(var(--transcript-scrollbar, 0px) + var(--find-rail, 0px) + 12px);
    z-index: 3;
    display: flex;
    flex-direction: column;
    /* Fit the gutter beside the reading column when it has room; a
       narrower pane lets the panel cover the column's edge. */
    width: min(clamp(200px, calc((100% - var(--reading-column)) / 2 - 20px), 260px), calc(100% - 24px));
    max-height: min(440px, calc(100% - 16px));
    background: var(--bg-surface);
    border: var(--border-width, 1px) solid var(--border-popover);
    border-radius: var(--radius-md, 6px);
    box-shadow: var(--shadow-md);
    font-size: 12px;
  }

  .toc-header {
    display: flex;
    align-items: center;
    gap: 6px;
    padding: 4px 4px 4px 10px;
    border-bottom: var(--border-width, 1px) solid var(--border-muted);
  }

  .toc-title {
    font-weight: 600;
    color: var(--text-primary);
  }

  .toc-count {
    color: var(--text-muted);
    font-variant-numeric: tabular-nums;
  }

  .toc-header :global(.toc-close) {
    margin-inline-start: auto;
  }

  .toc-list {
    margin: 0;
    padding: 4px;
    list-style: none;
    overflow-y: auto;
    min-height: 0;
    position: relative;
  }

  .toc-entry {
    display: flex;
    align-items: baseline;
    gap: 8px;
    width: 100%;
    padding: 3px 6px;
    border: none;
    border-radius: var(--radius-sm, 4px);
    background: transparent;
    color: var(--text-secondary);
    font: inherit;
    text-align: start;
    cursor: pointer;
  }

  .toc-entry:hover {
    background: var(--bg-surface-hover);
    color: var(--text-primary);
  }

  .toc-entry:focus-visible {
    outline: 2px solid var(--accent-blue);
    outline-offset: -2px;
  }

  .toc-entry.current {
    background: var(--selected-bg);
    box-shadow: var(--selected-edge);
    color: var(--text-primary);
  }

  .toc-number {
    flex: none;
    min-width: 2ch;
    color: var(--text-muted);
    font-variant-numeric: tabular-nums;
    text-align: end;
  }

  .toc-label {
    flex: 1;
    min-width: 0;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  .toc-label.shell {
    font-family: var(--font-mono);
  }

  .toc-label.empty {
    color: var(--text-muted);
    font-style: italic;
  }

  .toc-note {
    margin: 0;
    padding: 8px 10px;
    color: var(--text-muted);
  }

  .toc-failed {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: 8px;
  }
</style>
