<script lang="ts">
  import { onMount, onDestroy, untrack } from "svelte";
  import RangePicker from "../shared/RangePicker.svelte";
  import {
    resolveRange,
    selectionFromWindow,
    type RangeSelection,
  } from "../shared/rangeSelection.js";
  import SummaryCards from "./SummaryCards.svelte";
  import Heatmap from "./Heatmap.svelte";
  import ActivityTimeline from "./ActivityTimeline.svelte";
  import ProjectBreakdown from "./ProjectBreakdown.svelte";
  import HourOfWeekHeatmap from "./HourOfWeekHeatmap.svelte";
  import SessionShape from "./SessionShape.svelte";
  import VelocityMetrics from "./VelocityMetrics.svelte";
  import ToolUsage from "./ToolUsage.svelte";
  import TopSkills from "./TopSkills.svelte";
  import AgentComparison from "./AgentComparison.svelte";
  import SessionHealthSection from "./SessionHealthSection.svelte";
  import TopSessions from "./TopSessions.svelte";
  import ActiveFilters from "./ActiveFilters.svelte";
  import SessionFilterControl from "../filters/SessionFilterControl.svelte";
  import { analytics } from "../../stores/analytics.svelte.js";
  import { sessions } from "../../stores/sessions.svelte.js";
  import { events } from "../../stores/events.svelte.js";
  import { ui } from "../../stores/ui.svelte.js";
  import { sync } from "../../stores/sync.svelte.js";
  import { exportAnalyticsCSV } from "../../utils/csv-export.js";
  import RefreshControl from "../shared/RefreshControl.svelte";

  const earliestSession = $derived(sync.stats?.earliest_session ?? null);

  const rangeSelection = $derived(
    selectionFromWindow({
      isPinned: analytics.isPinned,
      windowDays: analytics.windowDays,
      from: analytics.from,
      to: analytics.to,
      earliestSession,
    }),
  );

  function applyRange(sel: RangeSelection) {
    if (sel.mode === "relative" && sel.days > 0) {
      analytics.setRollingWindow(sel.days);
    } else {
      const range = resolveRange(sel, earliestSession);
      analytics.setDateRange(range.from, range.to);
    }
  }

  function shortTz(tz: string): string {
    const slash = tz.lastIndexOf("/");
    return slash >= 0
      ? tz.slice(slash + 1).replace(/_/g, " ")
      : tz;
  }

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

  let unsubEvents: (() => void) | undefined;

  onMount(() => {
    // The page owns the initial load; RefreshControl handles the periodic
    // refresh after that. SSE events only flag new data -- refetching on every
    // event would thrash the aggregation -- so refetching stays bounded to the
    // RefreshControl scheduler and its manual button.
    analytics.fetchAll();
    unsubEvents = events.subscribe(() => analytics.markNewData());
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
    const curAutomatedScope = untrack(
      () => analytics.automatedScope,
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
    const headerAutomatedScope = headerIncludeAutomated
      ? "all"
      : "human";
    if (curAutomatedScope !== headerAutomatedScope) {
      analytics.automatedScope = headerAutomatedScope;
      changed = true;
    }

    if (changed) {
      untrack(() => analytics.fetchAll());
    }
  });

  onDestroy(() => {
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

    <RangePicker
      selection={rangeSelection}
      busy={analytics.isQuerying}
      {earliestSession}
      onSelect={applyRange}
    />
    <RefreshControl
      lastUpdatedAt={analytics.lastUpdatedAt}
      busy={analytics.isQuerying}
      onRefresh={() => analytics.fetchAll()}
      label="Refresh analytics"
    />
    <button class="export-btn" onclick={handleExportCSV}>
      Export CSV
    </button>
  </div>

  <ActiveFilters />

  <div
    class="analytics-content"
    class:querying={analytics.isQuerying}
    aria-busy={analytics.isQuerying}
  >
    {#if analytics.isQuerying}
      <div class="query-progress" aria-hidden="true"></div>
    {/if}

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
        <TopSkills />
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
    position: relative;
    transition: opacity 0.12s;
  }

  .analytics-content.querying {
    opacity: 0.88;
  }

  .query-progress {
    position: absolute;
    top: 0;
    left: 0;
    right: 0;
    z-index: 4;
    height: 2px;
    overflow: hidden;
    background: color-mix(
      in srgb,
      var(--accent-blue) 16%,
      transparent
    );
  }

  .query-progress::before {
    content: "";
    display: block;
    width: 38%;
    height: 100%;
    background: var(--accent-blue);
    border-radius: 999px;
    animation: query-progress 1s ease-in-out infinite;
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

  @keyframes query-progress {
    0% {
      transform: translateX(-105%);
    }
    100% {
      transform: translateX(265%);
    }
  }
</style>
