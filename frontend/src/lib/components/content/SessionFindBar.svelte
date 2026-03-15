<script lang="ts">
  import { inSessionSearch } from "../../stores/inSessionSearch.svelte.js";
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
    if (inSessionSearch.matches.length === 0) return "No results";
    return `${inSessionSearch.currentMatchIndex + 1} of ${inSessionSearch.matches.length}`;
  });
</script>

{#if inSessionSearch.isOpen}
  <div class="find-bar" role="search" aria-label="Find in session">
    <svg
      class="find-icon"
      width="13"
      height="13"
      viewBox="0 0 16 16"
      fill="currentColor"
      aria-hidden="true"
    >
      <path d="M11.742 10.344a6.5 6.5 0 10-1.397 1.398h-.001c.03.04.062.078.098.115l3.85 3.85a1 1 0 001.415-1.414l-3.85-3.85a1.007 1.007 0 00-.115-.099zm-5.242 1.156a5.5 5.5 0 110-11 5.5 5.5 0 010 11z"/>
    </svg>

    <input
      bind:this={inputRef}
      class="find-input"
      class:no-results={noResults}
      type="text"
      placeholder="Find in session…"
      spellcheck="false"
      autocomplete="off"
      value={inSessionSearch.query}
      oninput={(e) =>
        (inSessionSearch.query = (e.currentTarget as HTMLInputElement).value)}
      onkeydown={handleKeydown}
      aria-label="Search query"
    />

    {#if hasQuery}
      <span class="counter" class:no-results={noResults} aria-live="polite">
        {counterText}
      </span>
    {/if}

    <div class="nav-buttons">
      <button
        class="nav-btn"
        title="Previous match (Shift+Enter)"
        disabled={!hasMatches}
        onclick={() => inSessionSearch.prev()}
        tabindex="0"
        aria-label="Previous match"
      >
        <svg width="11" height="11" viewBox="0 0 16 16" fill="currentColor">
          <path d="M7.646 4.646a.5.5 0 01.708 0l6 6a.5.5 0 01-.708.708L8 5.707l-5.646 5.647a.5.5 0 01-.708-.708l6-6z"/>
        </svg>
      </button>
      <button
        class="nav-btn"
        title="Next match (Enter)"
        disabled={!hasMatches}
        onclick={() => inSessionSearch.next()}
        tabindex="0"
        aria-label="Next match"
      >
        <svg width="11" height="11" viewBox="0 0 16 16" fill="currentColor">
          <path d="M1.646 4.646a.5.5 0 01.708 0L8 10.293l5.646-5.647a.5.5 0 01.708.708l-6 6a.5.5 0 01-.708 0l-6-6a.5.5 0 010-.708z"/>
        </svg>
      </button>
    </div>

    <div class="divider"></div>

    <button
      class="close-btn"
      title="Close (Esc)"
      onclick={() => inSessionSearch.close()}
      tabindex="0"
      aria-label="Close find bar"
    >
      <svg width="12" height="12" viewBox="0 0 16 16" fill="currentColor">
        <path d="M3.72 3.72a.75.75 0 011.06 0L8 6.94l3.22-3.22a.75.75 0 111.06 1.06L9.06 8l3.22 3.22a.75.75 0 11-1.06 1.06L8 9.06l-3.22 3.22a.75.75 0 01-1.06-1.06L6.94 8 3.72 4.78a.75.75 0 010-1.06z"/>
      </svg>
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

  .find-icon {
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
