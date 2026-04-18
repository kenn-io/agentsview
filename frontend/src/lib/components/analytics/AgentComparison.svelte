<script lang="ts">
  import { analytics } from "../../stores/analytics.svelte.js";
  import { router } from "../../stores/router.svelte.js";

  interface AgentRow {
    name: string;
    sessions: number;
    messages: number;
    turnCycleP50: number;
    msgsPerMin: number;
    toolsPerMin: number;
    totalToolCalls: number;
    topCategories: string[];
  }

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

  const agents = $derived.by((): AgentRow[] => {
    const names = new Set<string>();

    const summaryAgents = analytics.summary?.agents;
    if (summaryAgents) {
      for (const k of Object.keys(summaryAgents)) {
        names.add(k);
      }
    }

    const velocityAgents = analytics.velocity?.by_agent;
    if (velocityAgents) {
      for (const bd of velocityAgents) {
        names.add(bd.label);
      }
    }

    const toolAgents = analytics.tools?.by_agent;
    if (toolAgents) {
      for (const ta of toolAgents) {
        names.add(ta.agent);
      }
    }

    const sorted = [...names].sort();

    return sorted.map((name): AgentRow => {
      const sa = summaryAgents?.[name];
      const vb = velocityAgents?.find(
        (b) => b.label === name,
      );
      const ta = toolAgents?.find(
        (a) => a.agent === name,
      );

      const topCats =
        ta?.categories
          .slice(0, 3)
          .map((c) => c.category) ?? [];

      return {
        name,
        sessions: sa?.sessions ?? 0,
        messages: sa?.messages ?? 0,
        turnCycleP50:
          vb?.overview.turn_cycle_sec.p50 ?? 0,
        msgsPerMin:
          vb?.overview.msgs_per_active_min ?? 0,
        toolsPerMin:
          vb?.overview.tool_calls_per_active_min ?? 0,
        totalToolCalls: ta?.total ?? 0,
        topCategories: topCats,
      };
    });
  });
</script>

<div class="agent-comparison">
  <h3 class="chart-title">Agent Comparison</h3>

  {#if analytics.errors.velocity || analytics.errors.summary || analytics.errors.tools}
    <div class="error">
      {analytics.errors.velocity ?? analytics.errors.summary ?? analytics.errors.tools}
      <button
        class="retry-btn"
        onclick={() => {
          analytics.fetchVelocity();
          analytics.fetchSummary();
          analytics.fetchTools();
        }}
      >
        Retry
      </button>
    </div>
  {:else if agents.length < 2}
    <div class="empty">
      No comparison data (need 2+ agents)
    </div>
  {:else}
    <div class="comparison-table">
      <div class="table-header">
        <span class="col-agent">Agent</span>
        <span class="col-num">Sessions</span>
        <span class="col-num">Messages</span>
        <span class="col-num">Cycle p50</span>
        <span class="col-num">Msgs/min</span>
        <span class="col-num">Tools/min</span>
        <span class="col-num">Tool Calls</span>
        <span class="col-cats">Top Categories</span>
      </div>
      {#each agents as agent}
        <!-- svelte-ignore a11y_click_events_have_key_events -->
        <!-- svelte-ignore a11y_no_static_element_interactions -->
        <div
          class="table-row"
          onclick={() => router.navigate("sessions", { agent: agent.name })}
        >
          <span class="col-agent">{agent.name}</span>
          <span class="col-num">
            {agent.sessions.toLocaleString()}
          </span>
          <span class="col-num">
            {agent.messages.toLocaleString()}
          </span>
          <span class="col-num">
            {formatDuration(agent.turnCycleP50)}
          </span>
          <span class="col-num">
            {formatRate(agent.msgsPerMin)}
          </span>
          <span class="col-num">
            {formatRate(agent.toolsPerMin)}
          </span>
          <span class="col-num">
            {agent.totalToolCalls.toLocaleString()}
          </span>
          <span class="col-cats">
            {agent.topCategories.join(", ") || "-"}
          </span>
        </div>
      {/each}
    </div>
  {/if}
</div>

<style>
  .agent-comparison {
    position: relative;
    flex: 1;
  }

  .chart-title {
    font-size: 12px;
    font-weight: 600;
    color: var(--text-primary);
    margin-bottom: 8px;
  }

  .comparison-table {
    display: flex;
    flex-direction: column;
    gap: 2px;
  }

  .table-header,
  .table-row {
    display: flex;
    align-items: center;
    gap: 4px;
    padding: 4px 0;
  }

  .table-header {
    border-bottom: 1px solid var(--border-muted);
    font-size: 9px;
    color: var(--text-muted);
    font-weight: 500;
  }

  .table-row {
    font-size: 11px;
    color: var(--text-secondary);
    cursor: pointer;
  }

  .table-row:hover {
    background: var(--bg-surface-hover);
  }

  .col-agent {
    flex: 0 0 80px;
    min-width: 60px;
    font-weight: 500;
  }

  .col-num {
    width: 72px;
    text-align: right;
    font-variant-numeric: tabular-nums;
  }

  .col-cats {
    flex: 1;
    min-width: 80px;
    text-align: left;
    color: var(--text-muted);
    font-size: 10px;
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
