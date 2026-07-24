<script lang="ts">
  import { m } from "../../i18n/index.js";
  import { onMount, untrack } from "svelte";
  import {
    activity,
    localDateStr,
    type Automation,
  } from "../../stores/activity.svelte.js";
  import { events } from "../../stores/events.svelte.js";
  import { sync } from "../../stores/sync.svelte.js";
  import { router } from "../../stores/router.svelte.js";
  import {
    yokedDates,
    panelDateState,
    rangeToActivityParams,
    type PanelDateState,
  } from "../../stores/yokedDates.svelte.js";
  import ProjectTypeahead from "../layout/ProjectTypeahead.svelte";
  import { Card, Typeahead, type TypeaheadOption } from "@kenn-io/kit-ui";
  import RefreshControl from "../shared/RefreshControl.svelte";
  import BranchPicker from "../shared/BranchPicker.svelte";
  import { searchBranches } from "../../api/client.js";
  import {
    branchPickerValues,
    reconcileBranchFilterValues,
  } from "../../branchFilters.js";
  import {
    addDays,
    endOfMonth,
    startOfIsoWeek,
    startOfMonth,
  } from "../../utils/dates.js";
  import RangePicker from "../shared/RangePicker.svelte";
  import {
    resolveRange,
    type RangeSelection,
  } from "../shared/rangeSelection.js";
  import SummaryCards from "./SummaryCards.svelte";
  import ConcurrencyTimeline from "./ConcurrencyTimeline.svelte";
  import SessionsTable from "./SessionsTable.svelte";
  import Breakdowns from "./Breakdowns.svelte";
  import ActivityInsight from "./ActivityInsight.svelte";

  // Date-only (local) bounds for the inline insight panel, derived from the
  // loaded report's resolved range, the authoritative source for the current
  // selection (day/week/month/custom are all resolved server-side). `range_end`
  // is exclusive, so subtract 1ms before formatting to get the inclusive last
  // local day. The panel is gated on a loaded report (below), so these are read
  // only when a report exists; the empty-string fallback never reaches the API.
  const insightFrom = $derived(
    activity.report ? localDateStr(new Date(activity.report.range_start)) : "",
  );
  const insightTo = $derived(
    activity.report
      ? localDateStr(new Date(new Date(activity.report.range_end).getTime() - 1))
      : "",
  );
  const activityPanelDate = $derived(currentActivityPanelDate());
  const activityDateSignature = $derived(dateSignature(activityPanelDate));
  let activityYokeReady = $state(false);
  let lastActivityDateSignature = "";

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

  const earliestSession = $derived(sync.stats?.earliest_session ?? null);
  let today = $state(localDateStr(new Date()));
  let todayRolloverTimer: ReturnType<typeof setTimeout> | undefined;

  function scheduleTodayRollover(): void {
    const now = new Date();
    today = localDateStr(now);
    const nextMidnight = new Date(
      now.getFullYear(),
      now.getMonth(),
      now.getDate() + 1,
    );
    todayRolloverTimer = setTimeout(() => {
      today = localDateStr(new Date());
      scheduleTodayRollover();
    }, nextMidnight.getTime() - now.getTime());
  }

  const agentOptions = $derived.by((): TypeaheadOption[] => [
    {
      name: "",
      label: m.activity_all_agents(),
      displayLabel: m.activity_all_agents(),
    },
    ...activity.agents.map((agent) => ({
      name: agent.name,
      label: `${agent.name} (${agent.session_count})`,
      displayLabel: agent.name,
      count: agent.session_count,
    })),
  ]);
  const machineOptions = $derived.by((): TypeaheadOption[] => [
    {
      name: "",
      label: m.activity_all_machines(),
      displayLabel: m.activity_all_machines(),
    },
    ...activity.machines.map((machine) => ({
      name: machine,
      label: machine,
      displayLabel: machine,
    })),
  ]);
  const branchProjects = $derived(activity.project ? [activity.project] : []);
  const selectedBranchNames = $derived(
    branchPickerValues(activity.branch ? [activity.branch] : []),
  );

  function searchActivityBranches(params: {
    projects?: string[];
    search: string;
    limit: number;
  }) {
    return searchBranches({
      ...params,
      includeOneShot: true,
      includeAutomated: true,
      scope: "all",
    });
  }
  const automationOptions: TypeaheadOption[] = $derived([
    {
      name: "all",
      label: m.activity_all_sessions(),
      displayLabel: m.activity_all_sessions(),
    },
    {
      name: "interactive",
      label: m.activity_interactive(),
      displayLabel: m.activity_interactive(),
    },
    {
      name: "automated",
      label: m.activity_automated(),
      displayLabel: m.activity_automated(),
    },
  ]);

  // The activity store is the source of truth: day/week/month map to a calendar
  // period anchored on `date`; custom maps to from/to. Relative windows have no
  // native API equivalent here, so applyRange sends concrete dates while the
  // URL keeps `window_days` to preserve rolling intent across reloads.
  const rangeSelection = $derived.by((): RangeSelection => {
    if (activity.preset === "custom") {
      if (activity.rollingWindowDays !== null) {
        return { mode: "relative", days: activity.rollingWindowDays };
      }
      return { mode: "custom", from: activity.from, to: activity.to };
    }
    return { mode: "calendar", unit: activity.preset, anchor: activity.date };
  });

  function applyRange(sel: RangeSelection) {
    let yokeState: PanelDateState | null = null;
    if (sel.mode === "calendar") {
      activity.setPreset(sel.unit);
      activity.setDate(sel.anchor);
    } else {
      const range = resolveRange(sel, earliestSession);
      yokeState = yokeStateForSelection(sel, range);
      activity.setCustomRange(
        range.from,
        range.to,
        yokeState?.mode === "rolling"
          ? yokeState.windowDays ?? null
          : null,
      );
    }
    if (yokeState) {
      yokedDates.updateFromPanel(yokeState);
      lastActivityDateSignature = dateSignature(
        currentActivityPanelDate(),
      );
    }
    activity.load();
  }

  function yokeStateForSelection(
    sel: RangeSelection,
    range: { from: string; to: string },
  ): PanelDateState | null {
    if (sel.mode === "relative" && sel.days > 0) {
      return panelDateState(range.from, range.to, {
        mode: "rolling",
        windowDays: sel.days,
      });
    }
    return panelDateState(range.from, range.to, { mode: "fixed" });
  }

  function currentActivityPanelDate(): PanelDateState | null {
    if (activity.preset === "custom") {
      if (activity.rollingWindowDays !== null) {
        return panelDateState(activity.from, activity.to, {
          mode: "rolling",
          windowDays: activity.rollingWindowDays,
        });
      }
      return panelDateState(activity.from, activity.to, { mode: "fixed" });
    }
    if (activity.preset === "week") {
      const from = startOfIsoWeek(activity.date);
      return panelDateState(from, addDays(from, 6), {
        mode: "fixed",
      });
    }
    if (activity.preset === "month") {
      const from = startOfMonth(activity.date);
      return panelDateState(from, endOfMonth(from), {
        mode: "fixed",
      });
    }
    return panelDateState(activity.date, activity.date, { mode: "fixed" });
  }

  function dateSignature(state: PanelDateState | null): string {
    if (!state) return "";
    return [
      state.mode ?? "fixed",
      state.windowDays ?? "",
      state.from,
      state.to,
    ].join(":");
  }

  function hasActivityDateParams(params: Record<string, string>): boolean {
    return (
      !!params.preset ||
      !!params.date ||
      !!params.from ||
      !!params.to ||
      !!params.window_days
    );
  }

  function applyActivityPanelDate(state: PanelDateState): void {
    activity.setCustomRange(
      state.from,
      state.to,
      state.mode === "rolling" ? state.windowDays ?? null : null,
    );
  }

  function seedActivityYoke(): void {
    if (hasActivityDateParams(router.params)) {
      const state = currentActivityPanelDate();
      if (state) yokedDates.updateFromPanel(state);
      return;
    }

    const seed = yokedDates.seedForPanel();
    const params = seed ? rangeToActivityParams(seed) : {};
    const state = panelDateState(params.from ?? "", params.to ?? "", {
      mode: params.window_days ? "rolling" : "fixed",
      windowDays: params.window_days
        ? Number.parseInt(params.window_days, 10)
        : undefined,
    });
    if (state) applyActivityPanelDate(state);
  }

  function onProjectSelect(value: string) {
    activity.setProject(value);
    activity.load();
  }

  function onAgentChange(value: string) {
    activity.setAgent(value);
    activity.load();
  }

  function onMachineChange(value: string) {
    activity.setMachine(value);
    activity.load();
  }

  function onBranchChange(values: string[]) {
    activity.setBranch(reconcileBranchFilterValues(
      activity.branch ? [activity.branch] : [],
      values,
    )[0] ?? "");
    activity.load();
  }

  function onAutomationChange(value: string) {
    activity.setAutomation(value as Automation);
    activity.load();
  }

  onMount(() => {
    scheduleTodayRollover();
    // Register as a consumer so a completed sync refreshes the filter
    // dropdowns while this page is on screen; detach on unmount.
    const detach = activity.attach();
    // Idempotent; loads the activity filter option lists with one-shot
    // and automated sessions included, matching the activity report.
    activity.loadFilterOptions();
    seedActivityYoke();
    lastActivityDateSignature = dateSignature(currentActivityPanelDate());
    activityYokeReady = true;
    // The page owns the initial load. attach() above ran hydrateFromUrl, so the
    // range/filters are set before this first load. RefreshControl handles the
    // periodic refresh after that.
    activity.load();
    // SSE events only flag that newer data exists; they never refetch the
    // report directly. Refetching on every event flips `loading` and blanks the
    // dashboard, so it is bounded to the RefreshControl scheduler and the
    // manual button.
    const unsubEvents = events.subscribe(() => activity.markNewData());
    return () => {
      activity.cancelInFlightReads();
      if (todayRolloverTimer !== undefined) {
        clearTimeout(todayRolloverTimer);
      }
      detach();
      unsubEvents();
    };
  });

  $effect(() => {
    const state = activityPanelDate;
    const signature = activityDateSignature;
    untrack(() => {
      if (!activityYokeReady || !state) return;
      if (signature === lastActivityDateSignature) return;
      lastActivityDateSignature = signature;
      yokedDates.updateFromPanel(state);
    });
  });
</script>

<div class="activity-page">
  <div class="activity-toolbar">
    <RangePicker
      selection={rangeSelection}
      busy={activity.loading}
      {earliestSession}
      maxDate={today}
      onSelect={applyRange}
    />

    <ProjectTypeahead
      projects={activity.projects}
      value={activity.project}
      onselect={onProjectSelect}
    />

    <div class="toolbar-typeahead">
      <Typeahead
        options={agentOptions}
        value={activity.agent}
        fallbackLabel={m.activity_all_agents()}
        placeholder={m.activity_filter_agents_placeholder()}
        title={m.activity_filter_by_agent()}
        emptyLabel={m.activity_no_matching_agents()}
        onselect={onAgentChange}
      />
    </div>

    <div class="toolbar-typeahead">
      <Typeahead
        options={machineOptions}
        value={activity.machine}
        fallbackLabel={m.activity_all_machines()}
        placeholder={m.activity_filter_machines_placeholder()}
        title={m.activity_filter_by_machine()}
        emptyLabel={m.activity_no_matching_machines()}
        onselect={onMachineChange}
      />
    </div>

    <div class="toolbar-typeahead">
      <BranchPicker
        mode="single"
        selected={selectedBranchNames}
        projects={branchProjects}
        search={searchActivityBranches}
        label={m.activity_filter_by_branch()}
        allLabel={m.activity_all_branches()}
        placeholder={m.activity_filter_branches_placeholder()}
        clearSearchLabel={m.shared_branch_clear_search()}
        loadingLabel={m.shared_branch_loading()}
        emptyLabel={m.shared_branch_no_match()}
        refineLabel={m.shared_branch_refine()}
        noBranchLabel={m.shared_no_branch()}
        onChange={onBranchChange}
      />
    </div>

    <div class="toolbar-typeahead compact">
      <Typeahead
        options={automationOptions}
        value={activity.automation}
        fallbackLabel={m.activity_all_sessions()}
        placeholder={m.activity_filter_automation_placeholder()}
        title={m.activity_filter_by_automation()}
        emptyLabel={m.activity_no_automation_filters()}
        onselect={onAutomationChange}
      />
    </div>

    <RefreshControl
      lastUpdatedAt={activity.lastUpdatedAt}
      busy={activity.loading}
      onRefresh={() => activity.load({ background: true })}
      label={m.activity_refresh()}
    />
  </div>

  <div class="activity-content">
    <!-- Report-first ordering: once a report is loaded it stays mounted through
         every background refresh, so a periodic/SSE-driven reload updates props
         in place instead of swapping in the full-screen "Loading" state and
         remounting the charts (which read as a blank flash). The loading and
         error states only show before the first report exists. -->
    {#if activity.report}
      <SummaryCards report={activity.report} />
      <Card level="default" padding="none" class="chart-panel">
        <ConcurrencyTimeline
          report={activity.report}
          selectedBucket={slotFilter?.idx ?? null}
          onSelectBucket={(sel) => (slotFilter = sel)}
        />
      </Card>
      <Card level="default" padding="none" class="chart-panel">
        <SessionsTable
          report={activity.report}
          filterIds={slotFilter?.sessionIds ?? null}
          filterLabel={slotFilter?.label ?? ""}
          onClearFilter={() => (slotFilter = null)}
        />
      </Card>
      <Card level="default" padding="none" class="chart-panel">
        <Breakdowns report={activity.report} />
      </Card>
    {:else if activity.loading}
      <div class="status">{m.activity_loading_report()}</div>
    {:else if activity.error}
      <div class="status error">
        <span>{activity.error}</span>
        <button class="retry-btn" onclick={() => activity.load()}>
          {m.shared_retry()}
        </button>
      </div>
    {:else}
      <div class="status">{m.shared_no_data_for_period()}</div>
    {/if}

    <!-- Range-scoped, not report-filter-scoped: kept outside the loading/error
         chain so it stays visible across filter reloads and only refetches when
         the resolved range changes. Gated on a loaded report so its bounds come
         from the authoritative resolved range, never a stale or pre-load
         single-day fallback (a deep link to a week/month/custom range would
         otherwise fetch an insight for the wrong span while the report loads). -->
    {#if activity.report}
      <Card level="default" padding="none" class="chart-panel">
        <ActivityInsight
          dateFrom={insightFrom}
          dateTo={insightTo}
          timezone={activity.timezone}
        />
      </Card>
    {/if}
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

  .toolbar-typeahead {
    --typeahead-min-width: 132px;
    --typeahead-max-width: 184px;
  }

  .toolbar-typeahead.compact {
    --typeahead-min-width: 118px;
    --typeahead-max-width: 150px;
  }

  .activity-content {
    flex: 1;
    overflow-y: auto;
    padding: 16px;
    display: flex;
    flex-direction: column;
    gap: 16px;
  }

  .activity-content :global(.chart-panel) {
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
    color: var(--accent-red-foreground);
  }
</style>
