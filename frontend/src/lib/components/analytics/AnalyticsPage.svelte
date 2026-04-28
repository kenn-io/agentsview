<script lang="ts">
  import { onMount, onDestroy, untrack } from "svelte";
  import DateRangeSelector from "../shared/DateRangeSelector.svelte";
  import SummaryCards from "./SummaryCards.svelte";
  import Heatmap from "./Heatmap.svelte";
  import ActivityTimeline from "./ActivityTimeline.svelte";
  import ProjectBreakdown from "./ProjectBreakdown.svelte";
  import HourOfWeekHeatmap from "./HourOfWeekHeatmap.svelte";
  import SessionShape from "./SessionShape.svelte";
  import VelocityMetrics from "./VelocityMetrics.svelte";
  import ToolUsage from "./ToolUsage.svelte";
  import AgentComparison from "./AgentComparison.svelte";
  import SessionHealthSection from "./SessionHealthSection.svelte";
  import TopSessions from "./TopSessions.svelte";
  import ActiveFilters from "./ActiveFilters.svelte";
  import SessionFilterControl from "../filters/SessionFilterControl.svelte";
  import { analytics } from "../../stores/analytics.svelte.js";
  import { sessions } from "../../stores/sessions.svelte.js";
  import { events } from "../../stores/events.svelte.js";
  import { ui } from "../../stores/ui.svelte.js";
  import { exportAnalyticsCSV } from "../../utils/csv-export.js";

  function shortTz(tz: string): string {
    const slash = tz.lastIndexOf("/");
    return slash >= 0
      ? tz.slice(slash + 1).replace(/_/g, " ")
      : tz;
  }

  const REFRESH_INTERVAL_MS = 5 * 60 * 1000;

  function handleExportCSV() {
    exportAnalyticsCSV({
      from: analytics.from,
      to: analytics.to,
      summary: analytics.summary,
      activity: analytics.activity,
      projects: analytics.projects,
      tools: analytics.tools,
      velocity: analytics.velocity,
    });
  }

  let refreshTimer: ReturnType<typeof setInterval> | undefined;
  let unsubEvents: (() => void) | undefined;

  onMount(() => {
    analytics.fetchAll();
    refreshTimer = setInterval(
      () => analytics.fetchAll(),
      REFRESH_INTERVAL_MS,
    );
    unsubEvents = events.subscribeDebounced(
      () => analytics.fetchAll(),
    );
  });

  // Sync sidebar filters to analytics dashboard. Runs whenever
  // the sidebar filters change. Uses untrack on analytics state
  // so that local drill-downs don't re-trigger.
  $effect(() => {
    const headerProject = sessions.filters.project;
    const headerMachine = sessions.filters.machine;
    const headerAgent = sessions.filters.agent;
    const headerTermination = sessions.filters.termination;
    const headerRecentlyActive = sessions.filters.recentlyActive;
    const headerMinUserMessages =
      sessions.filters.minUserMessages;
    const headerIncludeOneShot =
      sessions.filters.includeOneShot;
    const headerIncludeAutomated =
      sessions.filters.includeAutomated;

    const curProject = untrack(() => analytics.project);
    const curMachine = untrack(() => analytics.machine);
    const curAgent = untrack(() => analytics.agent);
    const curTermination = untrack(() => analytics.termination);
    const curRecentlyActive = untrack(
      () => analytics.recentlyActive,
    );
    const curMinUser = untrack(
      () => analytics.minUserMessages,
    );
    const curIncludeOneShot = untrack(
      () => analytics.includeOneShot,
    );
    const curIncludeAutomated = untrack(
      () => analytics.includeAutomated,
    );

    let changed = false;
    if (curProject !== headerProject) {
      analytics.project = headerProject;
      changed = true;
    }
    if (curMachine !== headerMachine) {
      analytics.machine = headerMachine;
      changed = true;
    }
    if (curAgent !== headerAgent) {
      analytics.agent = headerAgent;
      changed = true;
    }
    if (curTermination !== headerTermination) {
      analytics.termination = headerTermination;
      changed = true;
    }

    if (curRecentlyActive !== headerRecentlyActive) {
      analytics.recentlyActive = headerRecentlyActive;
      changed = true;
    }

    const minUserVal = headerMinUserMessages > 0
      ? headerMinUserMessages
      : 0;
    if (curMinUser !== minUserVal) {
      analytics.minUserMessages = minUserVal;
      changed = true;
    }

    if (curIncludeOneShot !== headerIncludeOneShot) {
      analytics.includeOneShot = headerIncludeOneShot;
      changed = true;
    }

    if (curIncludeAutomated !== headerIncludeAutomated) {
      analytics.includeAutomated = headerIncludeAutomated;
      changed = true;
    }

    if (changed) {
      untrack(() => analytics.fetchAll());
    }
  });

  onDestroy(() => {
    if (refreshTimer !== undefined) {
      clearInterval(refreshTimer);
    }
    unsubEvents?.();
  });
</script>

<div class="analytics-page">
  <div class="analytics-toolbar">
    {#if !ui.sidebarOpen}
      <div class="toolbar-filter-anchor">
        <SessionFilterControl
          showDisplay={false}
          showStarred={false}
          align="left"
        />
      </div>
    {/if}

    <DateRangeSelector
      from={analytics.from}
      to={analytics.to}
      onChange={(from, to) => analytics.setDateRange(from, to)}
      onPreset={(days) => analytics.setRollingWindow(days)}
    />
    <button
      class="refresh-btn"
      onclick={() => analytics.fetchAll()}
      title="Refresh analytics"
      aria-label="Refresh analytics"
    >
      <svg width="14" height="14" viewBox="0 0 16 16" fill="currentColor">
        <path d="M8 3a5 5 0 00-4.546 2.914.5.5 0 01-.908-.418A6 6 0 0114 8a.5.5 0 01-1 0 5 5 0 00-5-5zm4.546 7.086a.5.5 0 01.908.418A6 6 0 012 8a.5.5 0 011 0 5 5 0 005 5 5 5 0 004.546-2.914z"/>
      </svg>
    </button>
    <button class="export-btn" onclick={handleExportCSV}>
      Export CSV
    </button>
  </div>

  <ActiveFilters />

  <div class="analytics-content">
    <SummaryCards />

    <div class="chart-grid">
      <div class="chart-panel wide">
        <Heatmap />
      </div>

      <div class="chart-panel">
        <div class="chart-header">
          <h3 class="chart-title">
            Activity by Day and Hour
            <span class="tz-label">
              {shortTz(analytics.timezone)}
            </span>
          </h3>
        </div>
        <ActivityTimeline />
        <div class="chart-divider"></div>
        <HourOfWeekHeatmap />
      </div>

      <div class="chart-panel">
        <TopSessions />
      </div>

      <div class="chart-panel wide">
        <ProjectBreakdown />
      </div>

      <div class="chart-panel">
        <SessionShape />
      </div>

      <div class="chart-panel">
        <ToolUsage />
      </div>

      <div class="chart-panel wide">
        <VelocityMetrics />
      </div>

      <div class="chart-panel wide">
        <AgentComparison />
      </div>
    </div>

    <SessionHealthSection />
  </div>
</div>

<style>
  .analytics-page {
    flex: 1;
    display: flex;
    flex-direction: column;
    overflow: hidden;
  }

  .analytics-toolbar {
    display: flex;
    align-items: center;
    gap: 12px;
    padding: 8px 16px;
    background: var(--bg-surface);
    border-bottom: 1px solid var(--border-muted);
    flex-shrink: 0;
  }

  .toolbar-filter-anchor {
    position: relative;
    display: flex;
    align-items: center;
  }

  .refresh-btn {
    width: 28px;
    height: 28px;
    display: flex;
    align-items: center;
    justify-content: center;
    border-radius: var(--radius-sm);
    color: var(--text-muted);
    cursor: pointer;
  }

  .refresh-btn:hover {
    background: var(--bg-surface-hover);
    color: var(--text-primary);
  }

  .export-btn {
    height: 24px;
    padding: 0 8px;
    border-radius: var(--radius-sm);
    font-size: 11px;
    font-weight: 500;
    color: var(--text-muted);
    cursor: pointer;
    transition: background 0.1s, color 0.1s;
    margin-left: auto;
  }

  .export-btn:hover {
    background: var(--bg-surface-hover);
    color: var(--text-secondary);
  }

  .analytics-content {
    flex: 1;
    overflow-y: auto;
    padding: 16px;
    display: flex;
    flex-direction: column;
    gap: 16px;
  }

  .chart-grid {
    display: grid;
    grid-template-columns: 1fr 1fr;
    gap: 12px;
  }

  .chart-panel {
    background: var(--bg-surface);
    border: 1px solid var(--border-muted);
    border-radius: var(--radius-md);
    padding: 12px;
    min-height: 200px;
    min-width: 0;
    overflow-x: hidden;
    display: flex;
    flex-direction: column;
  }

  .chart-panel.wide {
    grid-column: 1 / -1;
  }

  .chart-header {
    display: flex;
    align-items: center;
    gap: 8px;
    margin-bottom: 8px;
  }

  .chart-title {
    font-size: 12px;
    font-weight: 600;
    color: var(--text-primary);
  }

  .tz-label {
    font-weight: 400;
    color: var(--text-muted);
    font-size: 10px;
    margin-left: 4px;
  }

  .chart-divider {
    height: 1px;
    background: var(--border-muted);
    margin: 12px 0;
  }

  @media (max-width: 800px) {
    .chart-grid {
      grid-template-columns: 1fr;
    }
  }
</style>
