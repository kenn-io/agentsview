<script lang="ts">
  import { onMount, onDestroy, untrack } from "svelte";
  import {
    usage,
    buildUsageUrlParams,
    parseWindowDays,
  } from "../../stores/usage.svelte.js";
  import { sessions } from "../../stores/sessions.svelte.js";
  import { router } from "../../stores/router.svelte.js";
  import { events } from "../../stores/events.svelte.js";
  import { agentColor } from "../../utils/agents.js";
  import UsageSummaryCards from "./UsageSummaryCards.svelte";
  import CostTimeSeriesChart from "./CostTimeSeriesChart.svelte";
  import AttributionPanel from "./AttributionPanel.svelte";
  import TopSessionsTable from "./TopSessionsTable.svelte";
  import CacheEfficiencyPanel from "./CacheEfficiencyPanel.svelte";
  import DateRangeSelector from "../shared/DateRangeSelector.svelte";
  import FilterDropdown from "./FilterDropdown.svelte";

  const REFRESH_MS = 5 * 60 * 1000;
  let refreshTimer: ReturnType<typeof setInterval> | undefined;
  let unsubEvents: (() => void) | undefined;

  const projectItems = $derived(
    sessions.projects.map((p) => ({
      name: p.name,
      count: p.session_count,
    })),
  );

  const agentItems = $derived(
    sessions.agents.map((a) => ({
      name: a.name,
      count: a.session_count,
    })),
  );

  // Track every model we've seen in any summary response or
  // excluded-model filter — never remove one. This keeps the model
  // dropdown usable even when landing on a shared URL like
  // /usage?exclude_model=claude-opus, which would otherwise never
  // show that model in the dropdown (it's filtered out of
  // modelTotals on every response) and leave the user unable to
  // re-include it.
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

  // Seed from the excluded-model filter so a shared URL like
  // /usage?exclude_model=foo shows "foo" in the dropdown on first
  // load, letting the user re-include it without clearing first.
  $effect(() => {
    const excluded = usage.excludedModels;
    untrack(() => {
      if (!excluded) return;
      mergeIntoKnownModels(excluded.split(","));
    });
  });

  const modelItems = $derived(
    knownModels.map((m) => ({ name: m })),
  );

  // URL-init: seed store filters from URL params when landing
  // on /usage with a deep-link. A bare /usage preserves the
  // current store state (restored from localStorage). Only
  // apply params that are actually present in the URL.
  const USAGE_FILTER_KEYS = new Set([
    "from", "to", "window_days",
    "exclude_project", "exclude_agent", "exclude_model",
  ]);
  let urlInitRan = false;
  $effect(() => {
    const route = router.route;
    const params = router.params;
    untrack(() => {
      if (route !== "usage") return;
      const hasDateParam = !!params["from"] || !!params["to"];
      const parsedWindowDays = parseWindowDays(params["window_days"]);
      const hasFilterKeys = Object.keys(params).some(
        (k) => USAGE_FILTER_KEYS.has(k),
      );

      let changed = false;

      // Sync pin state from URL: dated URL pins, undated URL unpins.
      // Runs before the !hasFilterKeys early return so a fully bare URL
      // (no exclude_* either) still flips the pin off.
      if (usage.isPinned !== hasDateParam) {
        usage.isPinned = hasDateParam;
        changed = true;
      }

      // Apply rolling window from URL when present and the URL is
      // not pinning a specific date range.
      if (!hasDateParam && parsedWindowDays !== null) {
        if (usage.windowDays !== parsedWindowDays) {
          usage.windowDays = parsedWindowDays;
          changed = true;
        }
      }

      if (!hasFilterKeys) {
        if (changed && urlInitRan) {
          usage.fetchAll();
        }
        urlInitRan = true;
        return;
      }
      if (params["from"] && params["from"] !== usage.from) {
        usage.from = params["from"];
        changed = true;
      }
      if (params["to"] && params["to"] !== usage.to) {
        usage.to = params["to"];
        changed = true;
      }
      const newExProj = params["exclude_project"] ?? "";
      if (newExProj !== usage.excludedProjects) {
        usage.excludedProjects = newExProj;
        changed = true;
      }
      const newExAgent = params["exclude_agent"] ?? "";
      if (newExAgent !== usage.excludedAgents) {
        usage.excludedAgents = newExAgent;
        changed = true;
      }
      const newExModel = params["exclude_model"] ?? "";
      if (newExModel !== usage.excludedModels) {
        usage.excludedModels = newExModel;
        changed = true;
      }
      if (changed && urlInitRan) {
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
    };
    untrack(() => {
      if (router.route !== "usage") return;
      router.replaceParams(buildUsageUrlParams(state));
    });
  });

  onMount(() => {
    usage.fetchAll();
    refreshTimer = setInterval(
      () => usage.fetchAll(),
      REFRESH_MS,
    );
    unsubEvents = events.subscribeDebounced(
      () => usage.fetchAll(),
    );
  });

  onDestroy(() => {
    if (refreshTimer !== undefined) {
      clearInterval(refreshTimer);
    }
    unsubEvents?.();
  });
</script>

<div class="usage-page">
  <div class="usage-toolbar">
    <h2 class="page-title">Usage</h2>

    <div class="toolbar-controls">
      <DateRangeSelector
        from={usage.from}
        to={usage.to}
        onChange={(from, to) => usage.setDateRange(from, to)}
        onPreset={(days) => usage.setRollingWindow(days)}
      />

      <FilterDropdown
        label="Project"
        items={projectItems}
        excludedCsv={usage.excludedProjects}
        onToggle={(name) => usage.toggleProject(name)}
        onSelectAll={() => usage.selectAllProjects()}
        onDeselectAll={() => usage.deselectAllProjects(projectItems.map(p => p.name))}
      />

      <FilterDropdown
        label="Agent"
        items={agentItems}
        excludedCsv={usage.excludedAgents}
        onToggle={(name) => usage.toggleAgent(name)}
        onSelectAll={() => usage.selectAllAgents()}
        onDeselectAll={() => usage.deselectAllAgents(agentItems.map(a => a.name))}
        color={agentColor}
      />

      <FilterDropdown
        label="Model"
        items={modelItems}
        excludedCsv={usage.excludedModels}
        onToggle={(name) => usage.toggleModel(name)}
        onSelectAll={() => usage.selectAllModels()}
        onDeselectAll={() => usage.deselectAllModels(modelItems.map(m => m.name))}
      />

      <button
        class="refresh-btn"
        onclick={() => usage.fetchAll()}
        title="Refresh"
        aria-label="Refresh usage data"
      >
        <svg
          width="14"
          height="14"
          viewBox="0 0 16 16"
          fill="currentColor"
        >
          <path d="M8 3a5 5 0 00-4.546 2.914.5.5 0 01-.908-.418A6 6 0 0114 8a.5.5 0 01-1 0 5 5 0 00-5-5zm4.546 7.086a.5.5 0 01.908.418A6 6 0 012 8a.5.5 0 011 0 5 5 0 005 5 5 5 0 004.546-2.914z" />
        </svg>
      </button>

      {#if usage.hasActiveFilters}
        <button
          class="clear-filters"
          onclick={() => usage.clearFilters()}
        >
          Clear filters
        </button>
      {/if}
    </div>
  </div>

  <div class="usage-content">
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

  .page-title {
    font-size: 14px;
    font-weight: 600;
    color: var(--text-primary);
    white-space: nowrap;
  }

  .toolbar-controls {
    display: flex;
    align-items: center;
    gap: 8px;
    flex-wrap: wrap;
    flex: 1;
  }

  .clear-filters {
    height: 26px;
    padding: 0 8px;
    border: none;
    background: none;
    color: var(--text-muted);
    font-size: 11px;
    cursor: pointer;
    text-decoration: underline;
    text-underline-offset: 2px;
  }

  .clear-filters:hover {
    color: var(--text-primary);
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

  .usage-content {
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
</style>
