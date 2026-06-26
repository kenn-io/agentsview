<script lang="ts">
  import {
    ChevronDownIcon,
    ChevronUpIcon,
    SearchIcon,
    XIcon,
  } from "../../icons.js";
  import { inSessionSearch } from "../../stores/inSessionSearch.svelte.js";
  import { m } from "../../i18n/index.js";
  import { tick } from "svelte";

  let inputRef: HTMLInputElement | undefined = $state(undefined);

  const isMac =
    typeof navigator !== "undefined" &&
    navigator.platform.toUpperCase().includes("MAC");

  $effect(() => {
    if (inSessionSearch.isOpen) {
      tick().then(() => inputRef?.focus());
    }
  });

  function handleKeydown(e: KeyboardEvent) {
    if (e.key === "Escape") {
      e.stopPropagation();
      inSessionSearch.close();
    } else if (e.key === "Enter") {
      e.preventDefault();
      if (e.shiftKey) {
        inSessionSearch.prev();
      } else {
        inSessionSearch.next();
      }
    }
  }

  let hasQuery = $derived(inSessionSearch.query.trim().length > 0);
  let hasMatches = $derived(inSessionSearch.matches.length > 0);
  let noResults = $derived(hasQuery && !hasMatches && !inSessionSearch.loading);

  let counterText = $derived.by(() => {
    if (!hasQuery) return "";
    if (inSessionSearch.loading) return "…";
    if (inSessionSearch.matches.length === 0) return m.session_find_no_results();
    return m.session_find_match_count({
      current: String(inSessionSearch.currentMatchIndex + 1),
      total: String(inSessionSearch.matches.length),
    });
  });
</script>

{#if inSessionSearch.isOpen}
  <div class="find-bar" role="search" aria-label={m.session_find_find_in_session()}>
    <SearchIcon class="find-icon" size="13" strokeWidth="2" aria-hidden="true" />

    <input
      bind:this={inputRef}
      class="find-input"
      class:no-results={noResults}
      type="text"
      placeholder={m.session_find_placeholder()}
      spellcheck="false"
      autocomplete="off"
      value={inSessionSearch.query}
      oninput={(e) =>
        (inSessionSearch.query = (e.currentTarget as HTMLInputElement).value)}
      onkeydown={handleKeydown}
      aria-label={m.session_find_search_query()}
    />

    {#if hasQuery}
      <span class="counter" class:no-results={noResults} aria-live="polite">
        {counterText}
      </span>
    {/if}

    <div class="nav-buttons">
      <button
        class="nav-btn"
        title={m.session_find_previous_match_title()}
        disabled={!hasMatches}
        onclick={() => inSessionSearch.prev()}
        tabindex="0"
        aria-label={m.session_find_previous_match()}
      >
        <ChevronUpIcon size="11" strokeWidth="2.4" aria-hidden="true" />
      </button>
      <button
        class="nav-btn"
        title={m.session_find_next_match_title()}
        disabled={!hasMatches}
        onclick={() => inSessionSearch.next()}
        tabindex="0"
        aria-label={m.session_find_next_match()}
      >
        <ChevronDownIcon size="11" strokeWidth="2.4" aria-hidden="true" />
      </button>
    </div>

    <div class="divider"></div>

    <button
      class="close-btn"
      title={m.session_find_close_title()}
      onclick={() => inSessionSearch.close()}
      tabindex="0"
      aria-label={m.session_find_close()}
    >
      <XIcon size="12" strokeWidth="2.4" aria-hidden="true" />
    </button>
  </div>
{/if}

<style>
  .find-bar {
    display: flex;
    align-items: center;
    gap: 6px;
    padding: 6px 16px;
    border-bottom: 1px solid var(--border-muted);
    background: var(--bg-surface);
    flex-shrink: 0;
    animation: slide-down 0.12s ease-out;
  }

  @keyframes slide-down {
    from {
      opacity: 0;
      transform: translateY(-8px);
    }
    to {
      opacity: 1;
      transform: translateY(0);
    }
  }

  :global(.find-icon) {
    color: var(--text-muted);
    flex-shrink: 0;
  }

  .find-input {
    flex: 1;
    min-width: 0;
    height: 26px;
    padding: 0 8px;
    font-size: 13px;
    font-family: var(--font-sans);
    color: var(--text-primary);
    background: var(--bg-inset);
    border: 1px solid var(--border-default);
    border-radius: var(--radius-sm);
    outline: none;
    transition:
      border-color 0.15s,
      background 0.15s;
  }

  .find-input:focus {
    border-color: var(--accent-blue);
    background: var(--bg-surface);
  }

  .find-input.no-results:focus {
    border-color: var(--accent-rose);
  }

  .counter {
    font-size: 11px;
    color: var(--text-muted);
    white-space: nowrap;
    flex-shrink: 0;
    min-width: 72px;
    text-align: right;
  }

  .counter.no-results {
    color: var(--accent-rose);
  }

  .nav-buttons {
    display: flex;
    align-items: center;
    gap: 2px;
    flex-shrink: 0;
  }

  .nav-btn {
    display: flex;
    align-items: center;
    justify-content: center;
    width: 24px;
    height: 24px;
    border-radius: var(--radius-sm);
    color: var(--text-secondary);
    transition:
      background 0.12s,
      color 0.12s;
  }

  .nav-btn:not(:disabled):hover {
    background: var(--bg-surface-hover);
    color: var(--text-primary);
  }

  .nav-btn:not(:disabled):active {
    transform: scale(0.9);
  }

  .divider {
    width: 1px;
    height: 16px;
    background: var(--border-muted);
    flex-shrink: 0;
    margin: 0 2px;
  }

  .close-btn {
    display: flex;
    align-items: center;
    justify-content: center;
    width: 24px;
    height: 24px;
    border-radius: var(--radius-sm);
    color: var(--text-muted);
    flex-shrink: 0;
    transition:
      background 0.12s,
      color 0.12s;
  }

  .close-btn:hover {
    background: var(--bg-surface-hover);
    color: var(--text-primary);
  }

  .close-btn:active {
    transform: scale(0.9);
  }
</style>
