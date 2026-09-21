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
  <div class="session-find" bind:this={root} use:ownAnnouncements use:protectFindInput={inSessionSearch}>
    <div class="find-controls">
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
  .session-find { flex: 0 0 auto; min-width: 0; }
  .find-controls { display: flex; align-items: center; gap: 4px; padding-inline-end: 8px; }
  .find-input { flex: 1; min-width: 0; }
  .history-status { display: flex; align-items: center; gap: 8px; padding: 2px 12px 6px; font-size: 11px; color: var(--text-muted); }
</style>
