<script lang="ts">
  import { usage } from "../../stores/usage.svelte.js";
  import { router } from "../../stores/router.svelte.js";
  import { formatTokenCount } from "../../utils/format.js";
  import { formatAgentName, truncate } from "../../utils/format.js";
  import { m } from "../../i18n/index.js";

  function fmtCost(v: number): string {
    return `$${v.toFixed(2)}`;
  }

  function handleRowClick(sessionId: string) {
    router.navigateToSession(sessionId);
  }

  const isTokenMode = $derived(usage.mode === "token");

  const sortedSessions = $derived.by(() => {
    const sessions = usage.topSessions ?? [];
    if (!isTokenMode) return sessions;
    // In token mode, re-sort by totalTokens descending
    return [...sessions].sort((a, b) => b.totalTokens - a.totalTokens);
  });
</script>

<div class="top-sessions-container">
  <h3 class="chart-title">{isTokenMode ? m.usage_top_sessions_by_tokens() : m.usage_top_sessions_by_cost()}</h3>

  {#if usage.errors.topSessions}
    <div class="error">
      {usage.errors.topSessions}
      <button
        class="retry-btn"
        onclick={() => usage.fetchTopSessions()}
      >
        {m.shared_retry()}
      </button>
    </div>
  {:else if sortedSessions && sortedSessions.length > 0}
    <div class="session-list">
      {#each sortedSessions as row, i (row.sessionId)}
        <!-- svelte-ignore a11y_click_events_have_key_events -->
        <!-- svelte-ignore a11y_no_static_element_interactions -->
        <div
          class="session-row"
          onclick={() => handleRowClick(row.sessionId)}
        >
          <span class="rank">{i + 1}</span>
          <div class="session-info">
            <span class="session-label">
              <span class="agent-pill">
                {formatAgentName(row.agent)}
              </span>
              {truncate(row.displayName || row.sessionId.slice(0, 12), 100)}
            </span>
            <span class="session-project">
              {row.project} &middot; {row.sessionId}
            </span>
          </div>
          <span class="session-tokens">
            {formatTokenCount(row.totalTokens)}
          </span>
          {#if !isTokenMode}
            <span class="session-cost">
              {fmtCost(row.cost)}
            </span>
          {/if}
        </div>
      {/each}
    </div>
  {:else}
    <div class="empty">{m.shared_no_sessions_in_range()}</div>
  {/if}
</div>

<style>
  .top-sessions-container {
    flex: 1;
    display: flex;
    flex-direction: column;
  }

  .chart-title {
    font-size: 12px;
    font-weight: 600;
    color: var(--text-primary);
    margin-bottom: 8px;
  }

  .session-list {
    display: flex;
    flex-direction: column;
    gap: 2px;
    overflow-y: auto;
    flex: 1;
  }

  .session-row {
    display: flex;
    align-items: center;
    gap: 8px;
    padding: 4px 6px;
    border-radius: var(--radius-sm);
    cursor: pointer;
    transition: background 0.1s;
  }

  .session-row:hover {
    background: var(--bg-surface-hover);
  }

  .rank {
    flex-shrink: 0;
    width: 18px;
    text-align: right;
    font-size: 10px;
    font-weight: 600;
    color: var(--text-muted);
    font-family: var(--font-mono);
  }

  .session-info {
    flex: 1;
    min-width: 0;
    display: flex;
    flex-direction: column;
    gap: 1px;
  }

  .session-label {
    font-size: 11px;
    color: var(--text-secondary);
    white-space: nowrap;
    overflow: hidden;
    text-overflow: ellipsis;
    display: flex;
    align-items: center;
    gap: 4px;
  }

  .agent-pill {
    flex-shrink: 0;
    font-size: 9px;
    font-weight: 500;
    padding: 1px 5px;
    border-radius: 8px;
    background: var(--bg-inset);
    color: var(--text-muted);
  }

  .session-project {
    font-size: 9px;
    color: var(--text-muted);
    white-space: nowrap;
    overflow: hidden;
    text-overflow: ellipsis;
  }

  .session-tokens {
    flex-shrink: 0;
    font-size: 10px;
    font-family: var(--font-mono);
    color: var(--text-muted);
    min-width: 36px;
    text-align: right;
  }

  .session-cost {
    flex-shrink: 0;
    font-size: 11px;
    font-weight: 500;
    font-family: var(--font-mono);
    color: var(--accent-blue);
    min-width: 48px;
    text-align: right;
  }

  .empty {
    color: var(--text-muted);
    font-size: 12px;
    padding: 24px;
    text-align: center;
  }

  .error {
    color: var(--accent-red);
    font-size: 12px;
    padding: 12px;
    display: flex;
    align-items: center;
    gap: 8px;
  }

  .retry-btn {
    padding: 2px 8px;
    border: 1px solid currentColor;
    border-radius: var(--radius-sm);
    font-size: 11px;
    color: inherit;
    cursor: pointer;
  }
</style>
