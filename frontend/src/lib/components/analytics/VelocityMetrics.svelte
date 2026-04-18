<script lang="ts">
  import { analytics } from "../../stores/analytics.svelte.js";
  import type {
    VelocityOverview,
    VelocityBreakdown,
  } from "../../api/types.js";

  function formatDuration(sec: number): string {
    if (sec <= 0) return "-";
    if (sec < 1) return `${Math.round(sec * 1000)}ms`;
    if (sec < 60) return `${sec.toFixed(1)}s`;
    const m = Math.floor(sec / 60);
    const s = Math.round(sec % 60);
    return s > 0 ? `${m}m ${s}s` : `${m}m`;
  }

  function formatRate(val: number): string {
    if (val <= 0) return "-";
    return val.toFixed(1);
  }

  type Tab = "overall" | "agent" | "complexity";
  let activeTab: Tab = $state("overall");

  const velocity = $derived(analytics.velocity);

  const breakdowns = $derived.by((): VelocityBreakdown[] => {
    if (!velocity) return [];
    switch (activeTab) {
      case "agent":
        return velocity.by_agent;
      case "complexity":
        return velocity.by_complexity;
      default:
        return [];
    }
  });
</script>

<div class="velocity-container">
  <div class="velocity-header">
    <h3 class="chart-title">Velocity</h3>
    <div class="tab-toggle">
      {#each (["overall", "agent", "complexity"] as const) as t}
        <button
          class="toggle-btn"
          class:active={activeTab === t}
          onclick={() => (activeTab = t)}
        >
          {t === "overall"
            ? "Overview"
            : t === "agent"
              ? "By Agent"
              : "By Size"}
        </button>
      {/each}
    </div>
  </div>

  {#if analytics.errors.velocity}
    <div class="error">
      {analytics.errors.velocity}
      <button
        class="retry-btn"
        onclick={() => analytics.fetchVelocity()}
      >
        Retry
      </button>
    </div>
  {:else if velocity}
    {#if activeTab === "overall"}
      {@const o = velocity.overall}
      <div class="metrics-grid">
        <div class="metric-card">
          <div class="metric-label">Turn Cycle (p50)</div>
          <div class="metric-value">
            {formatDuration(o.turn_cycle_sec.p50)}
          </div>
        </div>
        <div class="metric-card">
          <div class="metric-label">Turn Cycle (p90)</div>
          <div class="metric-value">
            {formatDuration(o.turn_cycle_sec.p90)}
          </div>
        </div>
        <div class="metric-card">
          <div class="metric-label">First Response (p50)</div>
          <div class="metric-value">
            {formatDuration(o.first_response_sec.p50)}
          </div>
        </div>
        <div class="metric-card">
          <div class="metric-label">First Response (p90)</div>
          <div class="metric-value">
            {formatDuration(o.first_response_sec.p90)}
          </div>
        </div>
        <div class="metric-card">
          <div class="metric-label">Msgs / Active Min</div>
          <div class="metric-value">
            {formatRate(o.msgs_per_active_min)}
          </div>
        </div>
        <div class="metric-card">
          <div class="metric-label">Chars / Active Min</div>
          <div class="metric-value">
            {formatRate(o.chars_per_active_min)}
          </div>
        </div>
        <div class="metric-card">
          <div class="metric-label">Tools / Active Min</div>
          <div class="metric-value">
            {formatRate(o.tool_calls_per_active_min)}
          </div>
        </div>
      </div>
    {:else if breakdowns.length > 0}
      <div class="breakdown-table">
        <div class="breakdown-header">
          <span class="col-label">Group</span>
          <span class="col-num">Sessions</span>
          <span class="col-num">Cycle p50</span>
          <span class="col-num">Cycle p90</span>
          <span class="col-num">Msgs/min</span>
          <span class="col-num">Tools/min</span>
        </div>
        {#each breakdowns as bd}
          <div class="breakdown-row">
            <span class="col-label">{bd.label}</span>
            <span class="col-num">{bd.sessions}</span>
            <span class="col-num">
              {formatDuration(bd.overview.turn_cycle_sec.p50)}
            </span>
            <span class="col-num">
              {formatDuration(bd.overview.turn_cycle_sec.p90)}
            </span>
            <span class="col-num">
              {formatRate(bd.overview.msgs_per_active_min)}
            </span>
            <span class="col-num">
              {formatRate(bd.overview.tool_calls_per_active_min)}
            </span>
          </div>
        {/each}
      </div>
    {:else}
      <div class="empty">No breakdown data</div>
    {/if}
  {:else}
    <div class="empty">No data for this period</div>
  {/if}
</div>

<style>
  .velocity-container {
    position: relative;
    flex: 1;
  }

  .velocity-header {
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

  .tab-toggle {
    display: flex;
    gap: 2px;
  }

  .toggle-btn {
    height: 22px;
    padding: 0 8px;
    border-radius: var(--radius-sm);
    font-size: 10px;
    font-weight: 500;
    color: var(--text-muted);
    cursor: pointer;
    transition: background 0.1s, color 0.1s;
  }

  .toggle-btn:hover {
    background: var(--bg-surface-hover);
    color: var(--text-secondary);
  }

  .toggle-btn.active {
    background: var(--bg-inset);
    color: var(--text-primary);
  }

  .metrics-grid {
    display: grid;
    grid-template-columns: 1fr 1fr 1fr;
    gap: 8px;
  }

  .metric-card {
    padding: 8px;
    background: var(--bg-inset);
    border-radius: var(--radius-sm);
    text-align: center;
  }

  .metric-label {
    font-size: 9px;
    color: var(--text-muted);
    margin-bottom: 4px;
  }

  .metric-value {
    font-size: 16px;
    font-weight: 600;
    color: var(--text-primary);
    font-variant-numeric: tabular-nums;
  }

  .breakdown-table {
    display: flex;
    flex-direction: column;
    gap: 2px;
  }

  .breakdown-header,
  .breakdown-row {
    display: flex;
    align-items: center;
    gap: 4px;
    padding: 4px 0;
  }

  .breakdown-header {
    border-bottom: 1px solid var(--border-muted);
    font-size: 9px;
    color: var(--text-muted);
    font-weight: 500;
  }

  .breakdown-row {
    font-size: 11px;
    color: var(--text-secondary);
  }

  .breakdown-row:hover {
    background: var(--bg-surface-hover);
  }

  .col-label {
    flex: 1;
    min-width: 60px;
  }

  .col-num {
    width: 64px;
    text-align: right;
    font-variant-numeric: tabular-nums;
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
