<script lang="ts">
  import { Button, FindBar, IconButton } from "@kenn-io/kit-ui";
  import { inSessionSearch } from "../../stores/inSessionSearch.svelte.js";
  import { protectFindInput } from "../../search/find-input.js";
  import { WholeWordIcon } from "../../icons.js";
  import { m } from "../../i18n/index.js";

  let root: HTMLDivElement | undefined = $state(undefined);
  $effect(() => {
    const request = inSessionSearch.focusRequest;
    if (!inSessionSearch.isOpen || !root) return;
    void request;
    const input = root.querySelector<HTMLInputElement>(".kit-find-bar__input");
    input?.focus();
    input?.select();
  });
  // Keep one live announcement, including partial-history status. kit-ui
  // retains its visible counter; its own live region is silenced here.
  function ownAnnouncements(node: HTMLElement) {
    const silenceCounter = () => {
      node.querySelector(".kit-find-bar__counter")?.setAttribute("aria-live", "off");
    };
    silenceCounter();
    const observer = new MutationObserver(silenceCounter);
    observer.observe(node, { childList: true, subtree: true });
    return { destroy() { observer.disconnect(); } };
  }
  let announcement = $derived(
    inSessionSearch.loadingHistory ? m.session_find_loading_history()
      : inSessionSearch.historyError ? m.session_find_history_incomplete()
      : !inSessionSearch.query.trim() ? ""
      : inSessionSearch.total > 0
        ? m.session_find_announce_match({ current: inSessionSearch.currentIndex + 1, total: inSessionSearch.total })
        : m.session_find_no_results(),
  );
</script>

{#if inSessionSearch.isOpen}
  <div class="session-find" class:has-find={inSessionSearch.isActive} bind:this={root} use:ownAnnouncements use:protectFindInput={inSessionSearch}>
    <div class="find-controls">
      <div class="find-field">
      <div class="find-input">
        <FindBar bind:query={inSessionSearch.query} matchCount={inSessionSearch.total}
          currentIndex={inSessionSearch.currentIndex}
          onnext={() => inSessionSearch.next()} onprev={() => inSessionSearch.prev()} onclose={() => inSessionSearch.close()}
          placeholder={m.session_find_placeholder()}
          matchCountLabel={m.session_find_match_count({ current: "{current}", total: "{total}" })}
          noMatchesLabel={inSessionSearch.loadingHistory ? m.session_find_loading_history()
            : inSessionSearch.historyError ? m.session_find_partial_results() : m.session_find_no_results()}
          ariaLabel={m.session_find_find_in_session()} inputAriaLabel={m.session_find_search_query()}
          previousLabel={m.session_find_previous_match()} nextLabel={m.session_find_next_match()} closeLabel={m.session_find_close()} />
      </div>
      <span class="find-divider" aria-hidden="true"></span>
      <IconButton ariaLabel={m.session_find_whole_word()}
        ariaPressed={inSessionSearch.wholeWord} onclick={() => inSessionSearch.toggleWholeWord()}>
        <WholeWordIcon size={16} aria-hidden="true" />
      </IconButton>
      <IconButton ariaLabel={inSessionSearch.resultsOpen ? m.session_find_hide_results() : m.session_find_toggle_results()}
        ariaExpanded={inSessionSearch.resultsOpen} ariaControls="session-find-results"
        ariaPressed={inSessionSearch.resultsOpen} onclick={() => { inSessionSearch.resultsOpen = !inSessionSearch.resultsOpen; }}>
        <svg width="14" height="14" viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.5" aria-hidden="true">
          <path d="M5 3h9M5 8h9M5 13h9M1 3h1M1 8h1M1 13h1" />
        </svg>
      </IconButton>
      </div>
    </div>
    {#if inSessionSearch.historyError && !inSessionSearch.loadingHistory}
      <div class="history-status">
        <span>{m.session_find_history_incomplete()}</span>
        <Button size="sm" surface="soft" onclick={() => inSessionSearch.retryHistory()}>
          {m.session_find_retry_history()}
        </Button>
      </div>
    {:else if inSessionSearch.loadingHistory && inSessionSearch.total > 0}
      <div class="history-status" aria-hidden="true">{m.session_find_loading_history()}</div>
    {/if}
    <span class="kit-sr-only search-announcement" aria-live="polite" aria-atomic="true">
      {announcement}
    </span>
  </div>
{/if}

<style>
  /* The band spans the pane, but the field and its buttons sit on the
     transcript's shared reading column (MessageList .virtual-row). On a
     narrow pane an active find reserves up to 12px for the overview rail,
     which shifts that column; wide panes keep it centered
     (SessionFindView .has-find uses the same clamp). */
  .session-find {
    --find-rail: 0px;
    --find-side: max(12px, calc((100% - var(--reading-column) - var(--find-rail)) / 2));
    flex: 0 0 auto;
    min-width: 0;
  }
  .session-find.has-find { --find-rail: clamp(0px, calc(var(--reading-column) + 44px - 100%), 12px); }
  .find-controls {
    display: flex;
    align-items: center;
    gap: 4px;
    padding-inline-start: var(--find-side);
    padding-inline-end: calc(var(--find-side) + var(--find-rail));
  }
  .find-controls { padding-block: 6px 4px; }
  .find-input { flex: 1; min-width: 0; }
  /* One contained field on the reading column: the query, counter, close,
     and the whole-word and results toggles share a single border, so the
     bar reads as one control. kit-ui's pinned strip drops its own
     underline and surface inside it. */
  .find-field {
    display: flex;
    align-items: center;
    gap: 2px;
    flex: 1;
    min-width: 0;
    padding-inline-end: 4px;
    border: 1px solid var(--border-default);
    border-radius: var(--radius-md, 6px);
    background: var(--bg-surface);
  }
  .find-field:focus-within { border-color: var(--accent-blue); }
  .find-field:has(:global(.kit-find-bar--no-results)):focus-within { border-color: var(--accent-red); }
  .find-input :global(.kit-find-bar--pinned),
  .find-input :global(.kit-find-bar--pinned:focus-within) {
    border: none;
    border-radius: var(--radius-md, 6px);
    background: transparent;
  }
  .find-divider {
    flex: none;
    width: 1px;
    height: 16px;
    margin-inline: 2px;
    background: var(--border-muted);
  }
  .find-input :global(.kit-find-bar__counter) { color: var(--text-secondary); }
  .history-status { display: flex; align-items: center; gap: 8px; padding: 2px calc(var(--find-side) + var(--find-rail)) 6px var(--find-side); font-size: 11px; color: var(--text-muted); }
</style>
