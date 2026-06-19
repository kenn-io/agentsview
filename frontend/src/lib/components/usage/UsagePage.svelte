<script lang="ts">
  import { onDestroy, onMount, tick, untrack } from "svelte";
  import {
    usage,
    buildUsageUrlParams,
    mergeUsageAndSessionUrlParams,
    parseWindowDays,
  } from "../../stores/usage.svelte.js";
  import {
    sessions,
    filtersToParams,
    parseFiltersFromParams,
    splitExcludeProjectParam,
  } from "../../stores/sessions.svelte.js";
  import { events } from "../../stores/events.svelte.js";
  import { router } from "../../stores/router.svelte.js";
  import { sync } from "../../stores/sync.svelte.js";
  import RangePicker from "../shared/RangePicker.svelte";
  import {
    resolveRange,
    selectionFromWindow,
    type RangeSelection,
  } from "../shared/rangeSelection.js";
  import UsageSummaryCards from "./UsageSummaryCards.svelte";
  import CostTimeSeriesChart from "./CostTimeSeriesChart.svelte";
  import AttributionPanel from "./AttributionPanel.svelte";
  import TopSessionsTable from "./TopSessionsTable.svelte";
  import CacheEfficiencyPanel from "./CacheEfficiencyPanel.svelte";
  import SessionFilterControl from "../filters/SessionFilterControl.svelte";
  import SessionActiveFilters from "../filters/SessionActiveFilters.svelte";
  import FilterDropdown from "./FilterDropdown.svelte";
  import RefreshControl from "../shared/RefreshControl.svelte";
  import {
    yokedDates,
    panelDateState,
    type PanelDateState,
  } from "../../stores/yokedDates.svelte.js";

  let mounted = false;
  let unsubEvents: (() => void) | undefined;

  const projectItems = $derived(
    sessions.projects.map((p) => ({
      name: p.name,
      count: p.session_count,
    })),
  );

  const earliestSession = $derived(sync.stats?.earliest_session ?? null);

  const rangeSelection = $derived(
    selectionFromWindow({
      isPinned: usage.isPinned,
      windowDays: usage.windowDays,
      from: usage.from,
      to: usage.to,
      earliestSession,
    }),
  );

  function applyRange(sel: RangeSelection) {
    if (sel.mode === "relative" && sel.days > 0) {
      usage.setRollingWindow(sel.days);
      updateYokeFromUsage(panelDateState(usage.from, usage.to, {
        mode: "rolling",
        windowDays: sel.days,
      }));
    } else {
      const range = resolveRange(sel, earliestSession);
      usage.setDateRange(range.from, range.to);
      updateYokeFromUsage(panelDateState(range.from, range.to, {
        mode: "fixed",
      }));
    }
  }

  // Track every model we've seen in any summary response or
  // model filter — never remove one. This keeps the model
  // dropdown usable when landing on a shared filtered URL.
  let knownModels: string[] = $state([]);

  function mergeIntoKnownModels(names: string[]): void {
    if (names.length === 0) return;
    const set = new Set(knownModels);
    let changed = false;
    for (const m of names) {
      if (m && !set.has(m)) {
        set.add(m);
        changed = true;
      }
    }
    if (changed) {
      knownModels = [...set].sort();
    }
  }

  // Seed from the filtered summary response.
  $effect(() => {
    const fromSummary = (usage.summary?.modelTotals ?? [])
      .map((m) => m.model);
    untrack(() => mergeIntoKnownModels(fromSummary));
  });

  // Seed from URL/local model filters before a response arrives.
  $effect(() => {
    const filtered = [
      usage.selectedModels,
    ].filter(Boolean).join(",");
    untrack(() => {
      if (!filtered) return;
      mergeIntoKnownModels(filtered.split(","));
    });
  });

  const modelItems = $derived(
    knownModels.map((m) => ({ name: m })),
  );
  const selectedModels = $derived(
    usage.selectedModels
      ? usage.selectedModels.split(",").filter(Boolean)
      : [],
  );
  const sessionUrlParams = $derived(
    filtersToParams(sessions.filters),
  );
  const sessionFilterSignature = $derived(
    JSON.stringify(sessionUrlParams),
  );
  function applyUsagePanelDate(state: PanelDateState): boolean {
    const before = JSON.stringify({
      from: usage.from,
      to: usage.to,
      isPinned: usage.isPinned,
      windowDays: usage.windowDays,
    });
    if (state.mode === "rolling" && state.windowDays) {
      usage.applyRollingWindow(state.windowDays);
    } else {
      usage.applyDateRange(state.from, state.to);
    }
    const after = JSON.stringify({
      from: usage.from,
      to: usage.to,
      isPinned: usage.isPinned,
      windowDays: usage.windowDays,
    });
    return before !== after;
  }

  function updateYokeFromUsage(state: PanelDateState | null): void {
    if (state) yokedDates.updateFromPanel(state);
  }

  // URL-init: seed store filters from URL params when landing
  // on /usage with a deep-link. A bare /usage preserves the
  // current store state (restored from localStorage). Only
  // apply params that are actually present in the URL.
  const USAGE_FILTER_KEYS = new Set([
    "from", "to", "window_days",
    "model", "exclude_model",
  ]);
  const SESSION_FILTER_KEYS = new Set([
    "project", "machine", "agent",
    "termination",
    "active_since", "exclude_project",
    "min_messages", "max_messages", "min_user_messages",
    "include_one_shot", "include_automated",
  ]);
  function usageSupportedSessionParams(
    params: Record<string, string>,
  ): Record<string, string> {
    const supported: Record<string, string> = {};
    for (const [key, value] of Object.entries(params)) {
      if (SESSION_FILTER_KEYS.has(key)) {
        supported[key] = value;
      }
    }
    return supported;
  }

  let urlInitRan = $state(false);
  let urlWritebackReady = $state(false);
  let initialFetchDone = $state(false);
  $effect(() => {
    const route = router.route;
    const params = router.params;
    untrack(() => {
      if (route !== "usage") return;
      const hasDateParam = !!params["from"] || !!params["to"];
      const parsedWindowDays = parseWindowDays(params["window_days"]);
      const supportedSessionParams =
        usageSupportedSessionParams(params);
      const hasSessionFilterKeys =
        Object.keys(supportedSessionParams).length > 0;
      const hasUsageFilterKeys = Object.keys(params).some(
        (k) => USAGE_FILTER_KEYS.has(k),
      );
      const hasFilterKeys =
        hasUsageFilterKeys || hasSessionFilterKeys;

      let changed = false;
      let sessionChanged = false;

      // Sync pin state from URL: dated URL pins, undated URL unpins.
      // Runs before the !hasFilterKeys early return so a fully bare URL
      // (no exclude_* either) still flips the pin off.
      if (usage.isPinned !== hasDateParam) {
        usage.isPinned = hasDateParam;
        changed = true;
      }

      if (!hasDateParam && parsedWindowDays === null) {
        const seed = yokedDates.seedForPanel();
        const state = seed
          ? panelDateState(seed.from, seed.to, {
              mode: seed.mode,
              windowDays: seed.windowDays,
            })
          : null;
        if (state) {
          changed = applyUsagePanelDate(state) || changed;
        }
      }

      // Apply rolling window from URL when present and the URL is
      // not pinning a specific date range.
      if (!hasDateParam && parsedWindowDays !== null) {
        const stateBefore = JSON.stringify({
          from: usage.from,
          to: usage.to,
          isPinned: usage.isPinned,
          windowDays: usage.windowDays,
        });
        usage.applyRollingWindow(parsedWindowDays);
        const stateAfter = JSON.stringify({
          from: usage.from,
          to: usage.to,
          isPinned: usage.isPinned,
          windowDays: usage.windowDays,
        });
        changed = stateBefore !== stateAfter || changed;
        updateYokeFromUsage(panelDateState(usage.from, usage.to, {
          mode: "rolling",
          windowDays: parsedWindowDays,
        }));
      }

      if (!hasFilterKeys) {
        if (changed && urlInitRan) {
          usage.fetchAll();
        }
        urlInitRan = true;
        return;
      }
      if (hasSessionFilterKeys) {
        const nextSessionParams = filtersToParams(
          parseFiltersFromParams(supportedSessionParams),
        );
        const currentSessionParams = filtersToParams(
          sessions.filters,
        );
        if (
          JSON.stringify(nextSessionParams) !==
          JSON.stringify(currentSessionParams)
        ) {
          sessions.initFromParams(supportedSessionParams);
          sessionChanged = true;
        }
      }
      if (hasDateParam) {
        const state = panelDateState(
          params["from"] ?? usage.from,
          params["to"] ?? usage.to,
          { mode: "fixed" },
        );
        if (state) {
          changed = applyUsagePanelDate(state) || changed;
          updateYokeFromUsage(state);
        }
      }
      const newExProject = splitExcludeProjectParam(
        params["exclude_project"],
      ).usageExcludedProjects;
      if (newExProject !== usage.excludedProjects) {
        usage.excludedProjects = newExProject;
        changed = true;
      }
      if (usage.excludedModels) {
        usage.excludedModels = "";
        changed = true;
      }
      const newModel = params["model"] ?? "";
      if (newModel !== usage.selectedModels) {
        usage.selectedModels = newModel;
        if (newModel) usage.excludedModels = "";
        changed = true;
      }
      if ((changed || sessionChanged) && urlInitRan) {
        usage.fetchAll();
      }
      urlInitRan = true;
    });
  });

  // URL write-back: keep URL params in sync with filter state
  // so users can share/bookmark the view.
  $effect(() => {
    const state = {
      from: usage.from,
      to: usage.to,
      isPinned: usage.isPinned,
      windowDays: usage.windowDays,
      excludedProjects: usage.excludedProjects,
      excludedAgents: usage.excludedAgents,
      excludedModels: usage.excludedModels,
      selectedModels: usage.selectedModels,
    };
    const nextParams = mergeUsageAndSessionUrlParams(
      buildUsageUrlParams(state),
      sessionUrlParams,
    );
    const ready = urlInitRan && urlWritebackReady;
    untrack(() => {
      if (!ready || router.route !== "usage") return;
      router.replaceParams(nextParams);
    });
  });

  $effect(() => {
    const signature = sessionFilterSignature;
    const ready = urlInitRan && urlWritebackReady;
    untrack(() => {
      if (!ready || !signature || router.route !== "usage" || !mounted) {
        return;
      }
      if (!initialFetchDone) {
        initialFetchDone = true;
      }
      usage.fetchAll();
    });
  });

  onMount(() => {
    mounted = true;
    // SSE events only flag new data; RefreshControl owns the periodic refresh
    // and the manual button. The initial and filter-change fetches run from the
    // effects above once URL/filter state is hydrated.
    unsubEvents = events.subscribe(() => usage.markNewData());
    tick().then(() => {
      urlWritebackReady = true;
    });
  });

  onDestroy(() => {
    unsubEvents?.();
  });
</script>

<div class="usage-page">
  <div class="usage-toolbar">
    <div class="toolbar-controls">
      <div class="usage-filter-anchor">
        <SessionFilterControl
          showDisplay={false}
          showStarred={false}
          align="left"
          extraActive={usage.hasActiveFilters || !!sessions.filters.project}
          onClearExtra={() => {
            sessions.filters.project = "";
            usage.clearFilters();
          }}
        />
      </div>

      <RangePicker
        selection={rangeSelection}
        busy={usage.isQuerying}
        {earliestSession}
        onSelect={applyRange}
      />

      <FilterDropdown
        label="Project"
        items={projectItems}
        excludedCsv={usage.excludedProjects}
        onToggle={(name) => usage.toggleProject(name)}
        onSelectAll={() => usage.selectAllProjects()}
        onDeselectAll={() =>
          usage.deselectAllProjects(projectItems.map((p) => p.name))}
      />

      <FilterDropdown
        label="Model"
        items={modelItems}
        excludedCsv={usage.selectedModels}
        mode="include"
        onToggle={(name) => usage.toggleModel(name)}
        onSelectAll={() => usage.selectAllModels()}
        onDeselectAll={() =>
          usage.deselectAllModels(modelItems.map((m) => m.name))}
      />

      <RefreshControl
        lastUpdatedAt={usage.lastUpdatedAt}
        busy={usage.isQuerying}
        onRefresh={() => usage.fetchAll()}
        label="Refresh usage data"
        title="Refresh"
      />

    </div>
  </div>

  <SessionActiveFilters
    modelFilters={selectedModels}
    onClearProjects={() => usage.selectAllProjects()}
    onRemoveModel={(model) => usage.toggleModel(model)}
    onClearModels={() => usage.selectAllModels()}
  />

  <div
    class="usage-content"
    class:querying={usage.isQuerying}
    aria-busy={usage.isQuerying}
  >
    {#if usage.isQuerying}
      <div class="query-progress" aria-hidden="true"></div>
    {/if}

    <UsageSummaryCards />

    <div class="chart-panel wide">
      <CostTimeSeriesChart />
    </div>

    <div class="chart-panel wide">
      <AttributionPanel />
    </div>

    <div class="bottom-grid">
      <div class="chart-panel">
        <TopSessionsTable />
      </div>
      <div class="chart-panel">
        <CacheEfficiencyPanel />
      </div>
    </div>
  </div>
</div>

<style>
  .usage-page {
    flex: 1;
    display: flex;
    flex-direction: column;
    min-height: 0;
  }

  .usage-toolbar {
    display: flex;
    align-items: center;
    gap: 12px;
    padding: 8px 16px;
    background: var(--bg-surface);
    border-bottom: 1px solid var(--border-muted);
    flex-shrink: 0;
  }

  .toolbar-controls {
    display: flex;
    align-items: center;
    gap: 8px;
    flex-wrap: wrap;
    flex: 1;
  }

  .usage-filter-anchor {
    position: relative;
    display: flex;
    align-items: center;
  }

  .usage-content {
    flex: 1;
    overflow-y: auto;
    padding: 16px;
    display: flex;
    flex-direction: column;
    gap: 16px;
    position: relative;
    transition: opacity 0.12s;
  }

  .usage-content.querying {
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

  .chart-panel {
    background: var(--bg-surface);
    border: 1px solid var(--border-muted);
    border-radius: var(--radius-md);
    padding: 12px;
    min-width: 0;
  }

  .chart-panel.wide {
    width: 100%;
  }

  .bottom-grid {
    display: grid;
    grid-template-columns: 1fr 1fr;
    gap: 12px;
  }

  @media (max-width: 800px) {
    .bottom-grid {
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
