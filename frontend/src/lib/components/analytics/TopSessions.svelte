<script lang="ts">
  import { analytics } from "../../stores/analytics.svelte.js";
  import { sessions } from "../../stores/sessions.svelte.js";
  import { router } from "../../stores/router.svelte.js";
  import { formatTokenCount } from "../../utils/format.js";
  import { normalizeMessagePreview } from "../../utils/messages.js";

  function truncate(text: string, max: number): string {
    if (text.length <= max) return text;
    return text.slice(0, max - 1) + "\u2026";
  }

  function formatDuration(mins: number): string {
    const total = Math.round(mins);
    if (total < 60) return `${total}m`;
    const h = Math.floor(total / 60);
    const m = total % 60;
    return m > 0 ? `${h}h ${m}m` : `${h}h`;
  }

  function handleSessionClick(id: string) {
    let needInvalidate = false;
    if (analytics.includeOneShot && !sessions.filters.includeOneShot) {
      sessions.filters.includeOneShot = true;
      needInvalidate = true;
    }
    if (analytics.includeAutomated && !sessions.filters.includeAutomated) {
      sessions.filters.includeAutomated = true;
      needInvalidate = true;
    }
    if (needInvalidate) {
      sessions.invalidateFilterCaches();
    }
    router.navigateToSession(id);
  }

  const supportsOutputTokens = $derived(
    analytics.summary?.total_output_tokens !== undefined &&
      analytics.summary?.token_reporting_sessions !== undefined,
  );
</script>

<div class="top-sessions-container">
  <div class="top-header">
    <h3 class="chart-title">Top Sessions</h3>
    <div class="metric-toggle">
      <button
        class="toggle-btn"
        class:active={analytics.topMetric === "messages"}
        onclick={() => analytics.setTopMetric("messages")}
      >
        By Messages
      </button>
      <button
        class="toggle-btn"
        class:active={analytics.topMetric === "duration"}
        onclick={() => analytics.setTopMetric("duration")}
      >
        By Duration
      </button>
      {#if supportsOutputTokens}
        <button
          class="toggle-btn"
          class:active={analytics.topMetric === "output_tokens"}
          onclick={() => analytics.setTopMetric("output_tokens")}
        >
          By Output Tokens
        </button>
      {/if}
    </div>
  </div>

  {#if analytics.errors.topSessions}
    <div class="error">
      {analytics.errors.topSessions}
      <button
        class="retry-btn"
        onclick={() => analytics.fetchTopSessions()}
      >
        Retry
      </button>
    </div>
  {:else if analytics.topSessions && analytics.topSessions.sessions.length > 0}
    <div class="session-list">
      {#each analytics.topSessions.sessions as session, i}
        {@const preview = normalizeMessagePreview(session.first_message)}
        <!-- svelte-ignore a11y_click_events_have_key_events -->
        <!-- svelte-ignore a11y_no_static_element_interactions -->
        <div
          class="session-row"
          onclick={() => handleSessionClick(session.id)}
        >
          <span class="rank">{i + 1}</span>
          <div class="session-info">
            <span class="session-label">
              {preview
                ? truncate(preview, 50)
                : session.id.slice(0, 12)}
            </span>
            <span class="session-project">{session.project}</span>
          </div>
          <span class="session-metric">
            {#if analytics.topMetric === "duration"}
              {formatDuration(session.duration_min)}
            {:else if analytics.topMetric === "output_tokens"}
              {formatTokenCount(session.output_tokens)}
            {:else}
              {session.message_count}
            {/if}
          </span>
        </div>
      {/each}
    </div>
  {:else}
    <div class="empty">No sessions in range</div>
  {/if}
</div>

<style>
  .top-sessions-container {
    flex: 1;
    display: flex;
    flex-direction: column;
  }

  .top-header {
    display: flex;
    align-items: center;
    justify-content: space-between;
    margin-bottom: 8px;
  }

  .chart-title {
    font-size: 12px;
    font-weight: 600;
    color: var(--text-primary);
  }

  .metric-toggle {
    display: flex;
    gap: 2px;
    background: var(--bg-inset);
    border-radius: var(--radius-sm);
    padding: 1px;
  }

  .toggle-btn {
    padding: 2px 8px;
    font-size: 10px;
    border-radius: var(--radius-sm);
    color: var(--text-muted);
    cursor: pointer;
    transition: background 0.1s, color 0.1s;
  }

  .toggle-btn.active {
    background: var(--bg-surface);
    color: var(--text-primary);
    font-weight: 500;
  }

  .toggle-btn:hover:not(.active) {
    color: var(--text-secondary);
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
  }

  .session-project {
    font-size: 9px;
    color: var(--text-muted);
    white-space: nowrap;
    overflow: hidden;
    text-overflow: ellipsis;
  }

  .session-metric {
    flex-shrink: 0;
    font-size: 11px;
    font-weight: 500;
    font-family: var(--font-mono);
    color: var(--accent-blue);
    min-width: 36px;
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
