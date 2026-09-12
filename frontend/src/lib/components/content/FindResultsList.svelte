<script lang="ts">
  import { untrack } from "svelte";
  import { Button, EmptyState, VirtualList } from "@kenn-io/kit-ui";
  import { messages } from "../../stores/messages.svelte.js";
  import { ui } from "../../stores/ui.svelte.js";
  import { inSessionSearch } from "../../stores/inSessionSearch.svelte.js";
  import { collectSearchBlocks } from "../../search/block-text.js";
  import { groupFindResults, resultRows, type FindResultRow } from "../../search/results.js";
  import { formatTimestamp } from "../../utils/format.js";
  import { m } from "../../i18n/index.js";

  let activeIndex = $state(-1);
  let groups = $derived(groupFindResults(messages.messages, inSessionSearch.matches, (message) => collectSearchBlocks(message, { renderUnknownXmlBlocksAsPreformatted: ui.renderUnknownXmlBlocksAsPreformatted }), ui.sortNewestFirst));
  let rows = $derived(resultRows(groups));
  $effect(() => {
    const current = inSessionSearch.resolvedCurrent;
    const list = rows;
    untrack(() => {
      activeIndex = current ? list.findIndex((row) => row.kind === "match" &&
        row.entry.match.blockKey === current.blockKey && row.entry.match.occurrence === current.occurrence) : -1;
    });
  });

  function activate(row: FindResultRow) {
    const match = row.kind === "match" ? row.entry.match : row.group.entries[0]?.match;
    if (match) inSessionSearch.goTo(match);
  }
</script>

<section id="session-find-results" class="find-results" aria-label={m.session_find_results_title()}>
  <h2>{m.session_find_results_title()}</h2>
  {#if rows.length}
    <VirtualList items={rows} estimateHeight={68} overscan={4} height="min(36vh, 320px)"
      bind:activeIndex ariaLabel={m.session_find_results_title()} onactivate={activate}>
      {#snippet row(result)}
        {#if result.kind === "group"}
          <div class="result-group">
            <span>{result.group.message.role === "user" ? m.message_content_role_user() : m.message_content_role_assistant()}</span>
            <time>{formatTimestamp(result.group.message.timestamp)}</time>
            <span class="result-count">{m.session_find_results_group_count({ count: result.group.count })}</span>
          </div>
        {:else}
          {@const entry = result.entry}
          <Button class="find-result-button" surface="soft" size="sm"
            tone={inSessionSearch.isCurrentBlock(entry.match.blockKey) && inSessionSearch.currentOccurrence(entry.match.blockKey) === entry.match.occurrence ? "info" : "neutral"}
            onclick={() => activate(result)}>
            <span class="result-snippet">
              {#if entry.block.label}<span class="result-tool">{entry.block.label}</span>{/if}
              <span>{entry.snippet.leading ? "…" : ""}{entry.snippet.before}<b>{entry.snippet.hit}</b>{entry.snippet.after}{entry.snippet.trailing ? "…" : ""}</span>
            </span>
          </Button>
        {/if}
      {/snippet}
    </VirtualList>
  {:else}
    <EmptyState title={inSessionSearch.loadingHistory ? m.session_find_loading_history()
      : inSessionSearch.historyError ? m.session_find_history_incomplete() : m.session_find_results_empty()} />
  {/if}
  <p class="result-scope">{m.session_find_subagent_excluded()}</p>
</section>

<style>
  .find-results { flex: 0 0 auto; min-width: 0; border-bottom: 1px solid var(--border-muted); }
  h2 { margin: 0; padding: 6px 12px; font-size: 12px; color: var(--text-secondary); }
  .result-group { display: flex; flex-wrap: wrap; gap: 8px; padding: 6px 12px; font-size: 11px; color: var(--text-muted); }
  .result-count { margin-inline-start: auto; }
  :global(.find-result-button) { width: 100%; text-align: start; justify-content: flex-start; }
  .result-snippet { display: flex; flex-direction: column; min-width: 0; white-space: pre-wrap; overflow-wrap: anywhere; font-size: 12px; }
  .result-tool { color: var(--text-muted); font-family: var(--font-mono); font-size: 10px; }
  b { color: var(--accent-blue); font-weight: 700; }
  .result-scope { margin: 0; padding: 4px 12px 8px; font-size: 11px; color: var(--text-muted); }
</style>
