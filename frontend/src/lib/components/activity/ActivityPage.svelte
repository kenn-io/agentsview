<script lang="ts">
  import { onMount } from "svelte";
  import { activity, localDateStr } from "../../stores/activity.svelte.js";
  import ProjectTypeahead from "../layout/ProjectTypeahead.svelte";
  import RangeControl from "./RangeControl.svelte";
  import RangeNavigator from "./RangeNavigator.svelte";
  import SummaryCards from "./SummaryCards.svelte";
  import ConcurrencyTimeline from "./ConcurrencyTimeline.svelte";
  import SessionsTable from "./SessionsTable.svelte";
  import Breakdowns from "./Breakdowns.svelte";
  import ActivityInsight from "./ActivityInsight.svelte";

  // Date-only (local) bounds for the inline insight panel, derived from the
  // loaded report's range. `range_end` is exclusive, so subtract 1ms before
  // formatting to get the inclusive last local day, matching the date-only
  // insight convention. Fall back to the anchor date before a report loads.
  const insightFrom = $derived(
    activity.report ? localDateStr(new Date(activity.report.range_start)) : activity.date,
  );
  const insightTo = $derived(
    activity.report
      ? localDateStr(new Date(new Date(activity.report.range_end).getTime() - 1))
      : activity.date,
  );

  // Page-local drill-down: clicking a Concurrency bucket filters the sessions
  // table to the sessions active in that slot. Deliberately not URL-synced — it
  // is a transient selection that resets whenever the report reloads.
  let slotFilter = $state<{
    idx: number;
    label: string;
    sessionIds: string[];
  } | null>(null);

  // A reloaded report (range/filter change) gets fresh buckets and sessions, so
  // a slot index/membership captured against the old report is stale; clear it.
  $effect(() => {
    void activity.report;
    slotFilter = null;
  });

  function onProjectSelect(value: string) {
    activity.setProject(value);
    activity.load();
  }

  function onAgentChange(e: Event) {
    activity.setAgent((e.currentTarget as HTMLSelectElement).value);
    activity.load();
  }

  function onMachineChange(e: Event) {
    activity.setMachine((e.currentTarget as HTMLSelectElement).value);
    activity.load();
  }

  onMount(() => {
    // Register as a consumer so a completed sync refreshes the filter
    // dropdowns while this page is on screen; detach on unmount.
    const detach = activity.attach();
    // Idempotent; loads the activity filter option lists with one-shot
    // and automated sessions included, matching the activity report.
    activity.loadFilterOptions();
    activity.load();
    return detach;
  });
</script>

<div class="activity-page">
  <div class="activity-toolbar">
    <RangeControl />
    <RangeNavigator />

    <ProjectTypeahead
      projects={activity.projects}
      value={activity.project}
      onselect={onProjectSelect}
    />

    <select
      class="filter-select"
      value={activity.agent}
      onchange={onAgentChange}
      aria-label="Filter by agent"
    >
      <option value="">All Agents</option>
      {#each activity.agents as a}
        <option value={a.name}>{a.name}</option>
      {/each}
    </select>

    <select
      class="filter-select"
      value={activity.machine}
      onchange={onMachineChange}
      aria-label="Filter by machine"
    >
      <option value="">All Machines</option>
      {#each activity.machines as m}
        <option value={m}>{m}</option>
      {/each}
    </select>
  </div>

  <div class="activity-content">
    {#if activity.loading}
      <div class="status">Loading activity report...</div>
    {:else if activity.error}
      <div class="status error">
        <span>{activity.error}</span>
        <button class="retry-btn" onclick={() => activity.load()}>
          Retry
        </button>
      </div>
    {:else if activity.report}
      <SummaryCards report={activity.report} />
      <div class="chart-panel">
        <ConcurrencyTimeline
          report={activity.report}
          selectedBucket={slotFilter?.idx ?? null}
          onSelectBucket={(sel) => (slotFilter = sel)}
        />
      </div>
      <div class="chart-panel">
        <SessionsTable
          report={activity.report}
          filterIds={slotFilter?.sessionIds ?? null}
          filterLabel={slotFilter?.label ?? ""}
          onClearFilter={() => (slotFilter = null)}
        />
      </div>
      <div class="chart-panel">
        <Breakdowns report={activity.report} />
      </div>
    {:else}
      <div class="status">No data for this period.</div>
    {/if}

    <!-- Range-scoped, not report-filter-scoped: kept outside the
         loading/error/report chain so it stays visible and only
         refetches when the range changes, not on filter/report reloads. -->
    <div class="chart-panel">
      <ActivityInsight dateFrom={insightFrom} dateTo={insightTo} />
    </div>
  </div>
</div>

<style>
  .activity-page {
    flex: 1;
    display: flex;
    flex-direction: column;
    min-height: 0;
  }

  .activity-toolbar {
    display: flex;
    align-items: center;
    gap: 8px;
    flex-wrap: wrap;
    padding: 8px 16px;
    background: var(--bg-surface);
    border-bottom: 1px solid var(--border-muted);
    flex-shrink: 0;
  }

  .filter-select {
    height: 26px;
    padding: 0 8px;
    background: var(--bg-inset);
    border: 1px solid var(--border-muted);
    border-radius: var(--radius-sm);
    font-size: 11px;
    color: var(--text-secondary);
    cursor: pointer;
  }

  .filter-select:focus {
    outline: none;
    border-color: var(--accent-blue);
  }

  .activity-content {
    flex: 1;
    overflow-y: auto;
    padding: 16px;
    display: flex;
    flex-direction: column;
    gap: 16px;
  }

  .chart-panel {
    background: var(--bg-surface);
    border: 1px solid var(--border-muted);
    border-radius: var(--radius-md);
    padding: 12px;
    min-width: 0;
  }

  .status {
    color: var(--text-muted);
    font-size: 12px;
    padding: 24px;
    text-align: center;
  }

  .status.error {
    display: flex;
    align-items: center;
    justify-content: center;
    gap: 8px;
    color: var(--accent-red);
  }

  .retry-btn {
    padding: 2px 8px;
    border: 1px solid var(--accent-red);
    border-radius: var(--radius-sm);
    font-size: 11px;
    color: var(--accent-red);
    cursor: pointer;
  }

  .retry-btn:hover {
    background: var(--accent-red);
    color: #fff;
  }
</style>
