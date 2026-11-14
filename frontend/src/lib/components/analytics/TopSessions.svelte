<script lang="ts">
  import { analytics } from "../../stores/analytics.svelte.js";
  import PanelStatus from "./PanelStatus.svelte";
  import { sessions } from "../../stores/sessions.svelte.js";
  import { getSessionStatus } from "../../stores/sessionStatus.svelte.js";
  import { router } from "../../stores/router.svelte.js";
  import { formatCount } from "../../utils/format.js";
  import { normalizeMessagePreview } from "../../utils/messages.js";
  import { StatusDot } from "@kenn-io/kit-ui";
  import { sessionStatusLabel } from "../../utils/sessionStatus.js";
  import { m } from "../../i18n/index.js";

  function truncate(text: string, max: number): string {
    if (text.length <= max) return text;
    return text.slice(0, max - 1) + "\u2026";
  }

  function sessionLabel(session: {
    id: string;
    first_message: string | null;
    display_name?: string | null;
  }): string {
    return (
      session.display_name ||
      normalizeMessagePreview(session.first_message) ||
      session.id.slice(0, 12)
    );
  }

  function formatDuration(mins: number): string {
    const total = Math.round(mins);
    if (total < 60) return `${total}m`;
    const h = Math.floor(total / 60);
    const m = total % 60;
    return m > 0 ? `${h}h ${m}m` : `${h}h`;
  }

  function formatDurationWithTotal(
    activeMin: number,
    totalMin: number,
  ): string {
    return `${formatDuration(activeMin)} (${m.analytics_top_sessions_total_duration({
      duration: formatDuration(totalMin),
    })})`;
  }

  function handleSessionClick(id: string) {
    let needInvalidate = false;
    const params: Record<string, string> = {};
    const clearParams: string[] = [];
    if (analytics.includeOneShot && !sessions.filters.includeOneShot) {
      sessions.filters.includeOneShot = true;
      clearParams.push("include_one_shot");
      needInvalidate = true;
    }
    if (
      analytics.automatedScope !== "human" &&
      !sessions.filters.includeAutomated
    ) {
      sessions.filters.includeAutomated = true;
      params.include_automated = "true";
      clearParams.push("include_automated");
      needInvalidate = true;
    }
    if (needInvalidate) {
      sessions.invalidateFilterCaches();
    }
    if (clearParams.length > 0 || Object.keys(params).length > 0) {
      router.navigateToSession(
        id,
        Object.keys(params).length > 0 ? params : undefined,
        clearParams,
      );
      return;
    }
    router.navigateToSession(id);
  }

  const uncleanCount = $derived(
    (analytics.topSessions?.sessions ?? []).filter(
      (s) => getSessionStatus(s) === "unclean",
    ).length,
  );
</script>

<div class="top-sessions-container">
  <div class="top-header">
    <h3 class="chart-title">{m.analytics_top_sessions_title()}</h3>
    <div class="header-controls">
      {#if uncleanCount > 0}
        <button
          class="status-count-pill"
          onclick={() => sessions.setTerminationFilter("unclean")}
          title={m.analytics_top_sessions_filter_unclean()}
        >
          <!-- The same dot the unclean rows carry, so the pill is
               also their legend. -->
          <StatusDot
            status="unclean"
            label={sessionStatusLabel("unclean")}
            size={7}
          />
          {m.analytics_top_sessions_unclean_count({
            countLabel: formatCount(uncleanCount),
          })}
        </button>
      {/if}
      <div class="metric-toggle">
        <button
          class="toggle-btn"
          class:active={analytics.topMetric === "messages"}
          onclick={() => analytics.setTopMetric("messages")}
        >
          {m.analytics_top_sessions_by_messages()}
        </button>
        <button
          class="toggle-btn"
          class:active={analytics.topMetric === "duration"}
          onclick={() => analytics.setTopMetric("duration")}
        >
          {m.analytics_top_sessions_by_duration()}
        </button>
        <button
          class="toggle-btn"
          class:active={analytics.topMetric === "output_tokens"}
          onclick={() => analytics.setTopMetric("output_tokens")}
        >
          {m.analytics_top_sessions_by_output_tokens()}
        </button>
      </div>
    </div>
  </div>

  <PanelStatus
    hasData={analytics.topSessions !== null}
    pending={analytics.querying.topSessions}
    error={analytics.errors.topSessions}
    onRetry={() => analytics.fetchTopSessions()}
  />
  {#if analytics.topSessions && analytics.topSessions.sessions.length > 0}
    <div class="session-list">
      {#each analytics.topSessions.sessions as session, i}
        {@const label = sessionLabel(session)}
        {@const status = getSessionStatus(session)}
        <!-- svelte-ignore a11y_click_events_have_key_events -->
        <!-- svelte-ignore a11y_no_static_element_interactions -->
        <div
          class="session-row"
          onclick={() => handleSessionClick(session.id)}
        >
          <span class="rank">{i + 1}</span>
          <span class="session-status">
            <StatusDot {status} label={sessionStatusLabel(status)} size={7} />
          </span>
          <div class="session-info">
            <span class="session-label">
              {truncate(label, 50)}
            </span>
            <span class="session-project">{session.project}</span>
          </div>
          <span class="session-metric">
            {#if analytics.topMetric === "duration"}
              <span
                class="session-metric-primary"
                title={m.analytics_top_sessions_active_duration()}
              >
                {formatDurationWithTotal(
                  session.active_duration_min,
                  session.duration_min,
                )}
              </span>
            {:else if analytics.topMetric === "output_tokens"}
              {formatCount(session.output_tokens)}
            {:else}
              {formatCount(session.message_count)}
            {/if}
          </span>
        </div>
      {/each}
    </div>
  {:else if analytics.topSessions}
    <div class="empty">{m.shared_no_sessions_in_range()}</div>
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

  .header-controls {
    display: flex;
    align-items: center;
    gap: 8px;
  }

  /* Red like the unclean row dots and the sidebar's unclean pill;
     the leading dot ties the count to the rows it counts. */
  .status-count-pill {
    display: inline-flex;
    align-items: center;
    gap: var(--space-3);
    padding: 2px 8px 2px 6px;
    font-size: 11px;
    font-weight: 500;
    font-variant-numeric: tabular-nums;
    border-radius: 999px;
    /* Unclean endings are a warning, not an error: the warning tone the
       unclean dots carry, so the pill does not overstate them. */
    color: color-mix(in srgb, var(--accent-orange) 70%, var(--text-primary));
    background: color-mix(in srgb, var(--accent-orange) 9%, transparent);
    border: 1px solid color-mix(in srgb, var(--accent-orange) 38%, transparent);
    cursor: pointer;
    transition: background 0.1s;
  }

  .status-count-pill:hover {
    background: color-mix(in srgb, var(--accent-orange) 16%, transparent);
  }

  /* Light orange needs a larger primary share for the 11px count to stay
     above 4.5:1 on the tinted pill, resting and hovered. */
  :global(:root:not(.dark)) .status-count-pill {
    color: color-mix(in srgb, var(--accent-orange) 55%, var(--text-primary));
  }

  .metric-toggle {
    display: flex;
    gap: 2px;
    padding: 2px;
    border-radius: var(--radius-md);
    background: var(--bg-inset);
  }

  .toggle-btn {
    padding: 2px 8px;
    font-size: 11px;
    font-weight: 500;
    border-radius: var(--radius-sm);
    color: var(--text-muted);
    cursor: pointer;
    transition: background 0.1s, color 0.1s;
  }

  /* Shared selected-segment idiom (app.css): raised pill, medium weight. */
  .toggle-btn.active {
    background: var(--selected-strong-bg);
    box-shadow: var(--selected-strong-edge);
    color: var(--text-primary);
    font-weight: var(--selected-strong-weight);
  }

  .toggle-btn:hover:not(.active) {
    color: var(--text-secondary);
  }

  /* Rows stay compact (no list gap, 3px padding, flush title and
     project lines) so all ten fit above the status bar at 1440x900. */
  .session-list {
    display: flex;
    flex-direction: column;
    gap: 0;
    overflow-y: auto;
    flex: 1;
  }

  .session-row {
    display: flex;
    align-items: center;
    gap: 8px;
    padding: 3px 6px;
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
    font-size: 11px;
    font-weight: 600;
    color: var(--text-muted);
    font-variant-numeric: tabular-nums;
  }

  .session-status {
    flex-shrink: 0;
    width: 14px;
    display: inline-flex;
    justify-content: center;
    align-items: center;
    font-size: 11px;
    line-height: 1;
  }

  .session-info {
    flex: 1;
    min-width: 0;
    display: flex;
    flex-direction: column;
    gap: 0;
  }

  /* The title leads; the project line is supporting metadata. */
  .session-label {
    font-size: 12px;
    font-weight: 500;
    color: var(--text-primary);
    white-space: nowrap;
    overflow: hidden;
    text-overflow: ellipsis;
  }

  .session-project {
    font-size: 11px;
    color: var(--text-muted);
    white-space: nowrap;
    overflow: hidden;
    text-overflow: ellipsis;
  }

  .session-metric {
    flex-shrink: 0;
    /* Values use the UI sans with tabular figures, like the other
       analytics values. */
    font-size: 12px;
    font-weight: 500;
    font-variant-numeric: tabular-nums;
    color: var(--text-primary);
    min-width: 86px;
    text-align: right;
  }

  .session-metric-primary {
    display: inline-block;
  }

  .empty {
    color: var(--text-muted);
    font-size: 12px;
    padding: 24px;
    text-align: center;
  }
</style>
