<script lang="ts">
  import { tick, onDestroy } from "svelte";
  import { ui } from "../../stores/ui.svelte.js";
  import { sessions } from "../../stores/sessions.svelte.js";
  import { searchStore } from "../../stores/search.svelte.js";
  import { messages } from "../../stores/messages.svelte.js";
  import {
    formatRelativeTime,
    truncate,
    sanitizeSnippet,
  } from "../../utils/format.js";
  import { agentColor } from "../../utils/agents.js";
  import { copyToClipboard } from "../../utils/clipboard.js";
  import { stripIdPrefix } from "../../utils/resume.js";
  import { normalizeMessagePreview } from "../../utils/messages.js";
  import type { Session, SearchResult } from "../../api/types.js";

  let inputRef: HTMLInputElement | undefined = $state(undefined);
  let selectedIndex: number = $state(0);
  let inputValue: string = $state("");

  // Clear state and reset sort whenever the palette is unmounted, regardless
  // of close path (Escape key, overlay click, Cmd+K toggle, or any other
  // mechanism). This ensures stale results and in-flight requests are always
  // cancelled even when the caller bypasses close().
  onDestroy(() => {
    searchStore.clear();
    searchStore.resetSort();
  });

  // Filtered recent sessions (client-side filter)
  let recentSessions = $derived.by(() => {
    if (inputValue.length > 0 && inputValue.length < 3) {
      const q = inputValue.toLowerCase();
      return sessions.sessions
        .filter(
          (s) =>
            s.project.toLowerCase().includes(q) ||
            (s.first_message?.toLowerCase().includes(q) ?? false),
        )
        .slice(0, 10);
    }
    if (!inputValue) {
      return sessions.sessions.slice(0, 10);
    }
    return [];
  });

  // Combined results: search results when query >= 3 chars, else recent
  let showSearchResults = $derived(inputValue.length >= 3);

  let totalItems = $derived(
    showSearchResults
      ? searchStore.results.length
      : recentSessions.length,
  );

  function handleInput(e: Event) {
    const target = e.target as HTMLInputElement;
    inputValue = target.value;
    selectedIndex = 0;

    if (inputValue.length >= 3) {
      searchStore.search(inputValue, sessions.filters.project);
    } else {
      searchStore.clear();
    }
  }

  function handleKeydown(e: KeyboardEvent) {
    if (e.key === "ArrowDown") {
      e.preventDefault();
      selectedIndex = Math.min(selectedIndex + 1, totalItems - 1);
    } else if (e.key === "ArrowUp") {
      e.preventDefault();
      selectedIndex = Math.max(selectedIndex - 1, 0);
    } else if (e.key === "Enter") {
      e.preventDefault();
      selectCurrent();
    } else if (e.key === "Escape") {
      e.preventDefault();
      close();
    }
  }

  function selectCurrent() {
    if (showSearchResults) {
      const result = searchStore.results[selectedIndex];
      if (result) {
        selectSearchResult(result);
      }
    } else {
      const session = recentSessions[selectedIndex];
      if (session) {
        selectSession(session);
      }
    }
  }

  function selectSession(s: Session) {
    sessions.selectSession(s.id);
    close();
  }

  function selectSearchResult(r: SearchResult) {
    sessions.selectSession(r.session_id);
    if (r.ordinal !== -1) {
      ui.scrollToOrdinal(r.ordinal, r.session_id);
    } else {
      // Name-only match: clear any stale selection/scroll state so the
      // previously highlighted ordinal is not left active.
      ui.clearScrollState();
    }
    close();
  }

  function close() {
    inputValue = "";
    ui.activeModal = null;
  }

  function handleOverlayClick(e: MouseEvent) {
    if ((e.target as HTMLElement).classList.contains("palette-overlay")) {
      close();
    }
  }

  $effect(() => {
    if (inputRef) {
      inputRef.focus();
    }
  });

  $effect(() => {
    const _idx = selectedIndex;
    tick().then(() => {
      const el = document.querySelector(
        ".palette-results .palette-item.selected",
      );
      if (el) el.scrollIntoView({ block: "nearest" });
    });
  });
</script>

<!-- svelte-ignore a11y_no_static_element_interactions -->
<div
  class="palette-overlay"
  onclick={handleOverlayClick}
  onkeydown={handleKeydown}
>
  <div class="palette">
    <div class="palette-input-wrap">
      <svg class="search-icon" width="14" height="14" viewBox="0 0 16 16" fill="currentColor">
        <path d="M11.742 10.344a6.5 6.5 0 10-1.397 1.398h-.001l3.85 3.85a1 1 0 001.415-1.414l-3.85-3.85zm-5.44.656a5 5 0 110-10 5 5 0 010 10z"/>
      </svg>
      <input
        bind:this={inputRef}
        type="text"
        class="palette-input"
        placeholder="Search sessions and messages..."
        value={inputValue}
        oninput={handleInput}
      />
      <kbd class="esc-hint">Esc</kbd>
    </div>

    <div class="palette-results">
      {#if showSearchResults}
        <div class="palette-sort">
          <button
            class="sort-btn"
            class:active={searchStore.sort === "relevance"}
            onmousedown={(e: MouseEvent) => e.preventDefault()}
            onclick={() => { searchStore.setSort("relevance"); selectedIndex = 0; }}
          >Relevance</button>
          <button
            class="sort-btn"
            class:active={searchStore.sort === "recency"}
            onmousedown={(e: MouseEvent) => e.preventDefault()}
            onclick={() => { searchStore.setSort("recency"); selectedIndex = 0; }}
          >Recency</button>
        </div>
        {#if searchStore.isSearching}
          <div class="palette-empty">Searching...</div>
        {:else if searchStore.results.length === 0}
          <div class="palette-empty">No results</div>
        {:else}
          {#each searchStore.results as result, i}
            <button
              class="palette-item"
              class:selected={i === selectedIndex}
              onclick={() => selectSearchResult(result)}
              onmouseenter={() => (selectedIndex = i)}
            >
              <span
                class="item-dot"
                style:background={agentColor(result.agent)}
              ></span>
              <span class="item-body">
                {#if result.name}
                  <span class="item-name">{truncate(result.name, 60)}</span>
                {/if}
                {#if result.snippet && result.snippet.replace(/<\/?mark>/g, '') !== result.name}
                  <span class="item-snippet">
                    {@html sanitizeSnippet(result.snippet)}
                  </span>
                {/if}
              </span>
              <span class="item-meta">
                {truncate(result.project, 20)}{result.session_ended_at ? ' · ' + formatRelativeTime(result.session_ended_at) : ''}
              </span>
              <!-- svelte-ignore a11y_click_events_have_key_events -->
              <!-- svelte-ignore a11y_no_static_element_interactions -->
              <span
                class="item-id"
                title="Copy session ID"
                onclick={(e) => {
                  e.stopPropagation();
                  copyToClipboard(result.session_id);
                }}
              >{stripIdPrefix(result.session_id, result.agent).slice(0, 8)}</span>
            </button>
          {/each}
        {/if}
      {:else}
        <div class="palette-section-label">Recent Sessions</div>
        {#each recentSessions as session, i}
          {@const preview = normalizeMessagePreview(session.first_message)}
          <button
            class="palette-item"
            class:selected={i === selectedIndex}
            onclick={() => selectSession(session)}
            onmouseenter={() => (selectedIndex = i)}
          >
            <span class="item-dot" style:background={agentColor(session.agent)}></span>
            <span class="item-body">
              <span class="item-name">{preview
                ? truncate(preview, 60)
                : session.project}</span>
            </span>
            <span class="item-meta">
              {formatRelativeTime(session.ended_at ?? session.started_at)}
            </span>
          </button>
        {/each}
      {/if}
    </div>
  </div>
</div>

<style>
  .palette-overlay {
    position: fixed;
    inset: 0;
    background: var(--overlay-bg);
    display: flex;
    justify-content: center;
    padding-top: 20vh;
    z-index: 100;
  }

  .palette {
    width: 560px;
    max-height: 400px;
    background: var(--bg-surface);
    border: 1px solid var(--border-default);
    border-radius: var(--radius-lg);
    box-shadow: var(--shadow-md);
    display: flex;
    flex-direction: column;
    overflow: hidden;
  }

  .palette-input-wrap {
    display: flex;
    align-items: center;
    gap: 8px;
    padding: 10px 14px;
    border-bottom: 1px solid var(--border-default);
  }

  .search-icon {
    flex-shrink: 0;
    color: var(--text-muted);
  }

  .palette-input {
    flex: 1;
    background: none;
    border: none;
    font-size: 14px;
    color: var(--text-primary);
    outline: none;
  }

  .palette-input::placeholder {
    color: var(--text-muted);
  }

  .esc-hint {
    font-size: 10px;
    padding: 1px 5px;
    border: 1px solid var(--border-default);
    border-radius: var(--radius-sm);
    color: var(--text-muted);
    background: var(--bg-inset);
    font-family: var(--font-sans);
  }

  .palette-results {
    overflow-y: auto;
    flex: 1;
    padding: 4px 0;
  }

  .palette-section-label {
    padding: 6px 14px 4px;
    font-size: 10px;
    font-weight: 600;
    color: var(--text-muted);
    text-transform: uppercase;
    letter-spacing: 0.05em;
  }

  .palette-item {
    display: flex;
    align-items: center;
    gap: 8px;
    width: 100%;
    padding: 6px 14px;
    text-align: left;
    font-size: 13px;
    color: var(--text-primary);
    transition: background 0.05s;
  }

  .palette-item:hover,
  .palette-item.selected {
    background: var(--bg-surface-hover);
  }

  .item-dot {
    width: 6px;
    height: 6px;
    border-radius: 50%;
    flex-shrink: 0;
  }

  .item-body {
    flex: 1;
    min-width: 0;
    display: flex;
    flex-direction: column;
    gap: 1px;
  }

  .item-name {
    white-space: nowrap;
    overflow: hidden;
    text-overflow: ellipsis;
    font-size: 13px;
    color: var(--text-primary);
  }

  .item-snippet {
    white-space: nowrap;
    overflow: hidden;
    text-overflow: ellipsis;
    font-size: 11px;
    color: var(--text-muted);
  }

  .item-meta {
    font-size: 11px;
    color: var(--text-muted);
    white-space: nowrap;
    flex-shrink: 0;
  }

  .palette-empty {
    padding: 16px;
    text-align: center;
    color: var(--text-muted);
    font-size: 13px;
  }

  .palette-sort {
    display: flex;
    gap: 4px;
    padding: 6px 14px 2px;
  }

  .sort-btn {
    padding: 2px 8px;
    font-size: 11px;
    border: 1px solid var(--border-default);
    border-radius: var(--radius-sm);
    background: none;
    color: var(--text-muted);
    cursor: pointer;
    font-family: var(--font-sans);
  }

  .sort-btn.active {
    background: var(--bg-surface-hover);
    color: var(--text-primary);
    border-color: var(--accent-purple);
  }

  .item-id {
    font-family: var(--font-mono);
    font-size: 10px;
    color: var(--text-muted);
    white-space: nowrap;
    flex-shrink: 0;
    cursor: pointer;
    padding: 1px 3px;
    border-radius: var(--radius-sm);
  }

  .item-id:hover {
    background: var(--bg-inset);
    color: var(--text-primary);
  }
</style>
