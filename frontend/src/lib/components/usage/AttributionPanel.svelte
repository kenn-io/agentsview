<script lang="ts">
  import {
    usage,
    type GroupBy,
    type AttributionView,
  } from "../../stores/usage.svelte.js";
  import {
    branchFilterToken,
    branchLabel,
  } from "../../branchFilters.js";
  import Treemap from "./Treemap.svelte";
  import { m } from "../../i18n/index.js";
  import { formatMoney, moneyFromMicrodollars } from "../../money.js";
  import { formatTokenCount } from "../../utils/format.js";
  import { sumSelectedTokens } from "../../stores/usageTokenTypes.js";
  import { createVirtualizer } from "../../virtual/createVirtualizer.svelte.js";

  interface Props {
    colorMap: ReadonlyMap<string, string>;
  }

  let { colorMap }: Props = $props();
  const groupBy = $derived(usage.toggles.attribution.groupBy);
  const view = $derived(usage.toggles.attribution.view);
  const isTokenMode = $derived(usage.mode === "token");
  const noBranchLabel = $derived(m.shared_no_branch());
  const UNATTRIBUTED_ID = "__unattributed__";
  const OTHER_ID = "__attribution_other__";

  interface Row {
    id: string;
    label: string;
    value: number;
    color: string;
    pct: number;
    selectable: boolean;
  }

  const rowItems = $derived.by(() => {
    const s = usage.summary;
    if (!s) return [];

    let items: Array<{
      id: string;
      label: string;
      value: number;
      selectable?: boolean;
    }> = [];

    if (groupBy === "project") {
      items = s.projectTotals.map((p) => ({
        id: p.project_key,
        label: p.project,
        value: isTokenMode
          ? sumSelectedTokens(p, usage.selectedTokenTypes)
          : p.cost.microdollars,
      }));
    } else if (groupBy === "model") {
      items = s.modelTotals.map((m) => ({
        id: m.model,
        label: m.model,
        value: isTokenMode
          ? sumSelectedTokens(m, usage.selectedTokenTypes)
          : m.cost.microdollars,
      }));
    } else if (groupBy === "branch") {
      items = s.branchTotals.map((b) => ({
        id: branchFilterToken(b.project_key, b.branch),
        label: branchLabel(b.project, b.branch, noBranchLabel),
        value: isTokenMode
          ? sumSelectedTokens(b, usage.selectedTokenTypes)
          : b.cost.microdollars,
      }));
      const attributed = items.reduce((sum, item) => sum + item.value, 0);
      const total = isTokenMode
        ? sumSelectedTokens(s.totals, usage.selectedTokenTypes)
        : s.totals.totalCost.microdollars;
      const unattributed = Math.max(0, total - attributed);
      if (unattributed > 0) {
        items.push({
          id: UNATTRIBUTED_ID,
          label: m.usage_unattributed(),
          value: unattributed,
          selectable: false,
        });
      }
    } else {
      items = s.agentTotals.map((a) => ({
        id: a.agent,
        label: a.agent,
        value: isTokenMode
          ? sumSelectedTokens(a, usage.selectedTokenTypes)
          : a.cost.microdollars,
      }));
    }

    items.sort((a, b) => b.value - a.value);
    return items;
  });

  const rows = $derived.by((): Row[] => {
    const items = rowItems;
    const total = items.reduce((sum, item) => sum + item.value, 0);

    return items.map((d) => ({
      id: d.id,
      label: d.label,
      value: d.value,
      color: d.id === UNATTRIBUTED_ID
        ? "var(--text-muted)"
        : colorMap.get(d.id) ?? "var(--text-muted)",
      pct: total > 0 ? d.value / total : 0,
      selectable: d.selectable ?? true,
    }));
  });

  const RAIL_ROW_HEIGHT = 24;
  const LIST_ROW_HEIGHT = 42;
  let railScrollElement: HTMLDivElement | undefined = $state();
  let listScrollElement: HTMLDivElement | undefined = $state();

  const railVirtualizer = createVirtualizer(() => ({
    count: rows.length,
    getScrollElement: () => railScrollElement ?? null,
    estimateSize: () => RAIL_ROW_HEIGHT,
    overscan: 6,
    getItemKey: (index) => rows[index]?.id ?? index,
  }));

  const listVirtualizer = createVirtualizer(() => ({
    count: rows.length,
    getScrollElement: () => listScrollElement ?? null,
    estimateSize: () => LIST_ROW_HEIGHT,
    overscan: 6,
    getItemKey: (index) => rows[index]?.id ?? index,
  }));

  // One SVG group is drawn per tile, and branch grouping can produce
  // thousands of (project, branch) rows whose tiles would be sub-pixel.
  // Reserve the final capped tile for their aggregate so the treemap still
  // represents the full total. The side rail and list view retain every row.
  const TREEMAP_MAX_TILES = 40;

  const treemapItems = $derived.by(() => {
    const visibleCount = rows.length > TREEMAP_MAX_TILES
      ? TREEMAP_MAX_TILES - 1
      : TREEMAP_MAX_TILES;
    const items = rows.slice(0, visibleCount).map((r) => ({
      id: r.id,
      label: r.label,
      value: r.value,
      color: r.color,
      meta: r.pct > 0 ? `${(r.pct * 100).toFixed(1)}%` : "",
      selectable: r.selectable,
    }));
    const omitted = rows.slice(visibleCount);
    if (omitted.length > 0) {
      const value = omitted.reduce((sum, row) => sum + row.value, 0);
      const pct = omitted.reduce((sum, row) => sum + row.pct, 0);
      items.push({
        id: OTHER_ID,
        label: m.shared_other(),
        value,
        color: "var(--text-muted)",
        meta: pct > 0 ? `${(pct * 100).toFixed(1)}%` : "",
        selectable: false,
      });
    }
    return items;
  });

  function handleSelect(id: string) {
    if (id === UNATTRIBUTED_ID || id === OTHER_ID) return;
    if (groupBy === "project") {
      usage.toggleProjectKey(id);
    } else if (groupBy === "agent") {
      usage.toggleAgent(id);
    } else if (groupBy === "model") {
      usage.toggleModel(id);
    } else if (groupBy === "branch") {
      usage.toggleBranch(id);
    }
  }

  // Project and agent clicks exclude the item ("hide"); model and
  // branch clicks toggle an include-based selection ("filter"), so the
  // hint, tooltip, and aria copy must describe different actions.
  const includeBased = $derived(
    groupBy === "model" || groupBy === "branch",
  );

  function isRowSelected(id: string): boolean {
    if (groupBy === "model") return usage.isModelSelected(id);
    if (groupBy === "branch") return usage.isBranchSelected(id);
    return false;
  }

  function rowTitle(id: string, label: string): string {
    if (id === UNATTRIBUTED_ID || id === OTHER_ID) return label;
    if (!includeBased) return m.usage_click_to_hide({ label });
    return isRowSelected(id)
      ? m.usage_click_to_clear_filter({ label })
      : m.usage_click_to_filter({ label });
  }

  function rowAriaLabel(id: string, label: string): string {
    if (id === UNATTRIBUTED_ID || id === OTHER_ID) return label;
    if (!includeBased) return m.usage_hide_from_chart({ label });
    return isRowSelected(id)
      ? m.usage_clear_filter_item({ label })
      : m.usage_filter_to_item({ label });
  }

  function handleGroupByChange(g: GroupBy) {
    usage.setAttributionGroupBy(g);
  }

  function handleViewChange(v: AttributionView) {
    usage.setAttributionView(v);
  }
</script>

<div class="attribution-panel">
  <div class="panel-header">
    <h3 class="chart-title">
      {isTokenMode
        ? m.usage_tokens_attribution_title()
        : m.usage_cost_attribution_title()}
    </h3>
    <div class="toggles">
      <div class="segment-toggle">
        <button
          class="toggle-btn"
          class:active={groupBy === "project"}
          onclick={() => handleGroupByChange("project")}
        >
          {m.analytics_col_project()}
        </button>
        <button
          class="toggle-btn"
          class:active={groupBy === "model"}
          onclick={() => handleGroupByChange("model")}
        >
          {m.usage_model()}
        </button>
        <button
          class="toggle-btn"
          class:active={groupBy === "agent"}
          onclick={() => handleGroupByChange("agent")}
        >
          {m.analytics_col_agent()}
        </button>
        <button
          class="toggle-btn"
          class:active={groupBy === "branch"}
          onclick={() => handleGroupByChange("branch")}
        >
          {m.usage_branch()}
        </button>
      </div>
      <div class="segment-toggle">
        <button
          class="toggle-btn"
          class:active={view === "treemap"}
          onclick={() => handleViewChange("treemap")}
        >
          {m.usage_attribution_treemap()}
        </button>
        <button
          class="toggle-btn"
          class:active={view === "list"}
          onclick={() => handleViewChange("list")}
        >
          {m.usage_attribution_list()}
        </button>
      </div>
    </div>
  </div>

  {#if rows.length === 0}
    <div class="empty">{m.shared_no_data_for_period()}</div>
  {:else}
    <div class="hint">
      {includeBased
        ? m.usage_click_to_filter_hint()
        : m.usage_click_to_hide_hint()}
    </div>
    {#if view === "treemap"}
      <div class="treemap-layout">
        <div class="treemap-main">
          <Treemap
            items={treemapItems}
            height={260}
            onSelect={handleSelect}
            formatValue={isTokenMode ? formatTokenCount : undefined}
            titleFor={rowTitle}
            ariaLabelFor={rowAriaLabel}
          />
        </div>
        <div class="side-rail" bind:this={railScrollElement}>
          <div
            class="rail-virtual-spacer"
            style="height: {railVirtualizer.instance?.getTotalSize() ?? 0}px; position: relative;"
          >
            {#each railVirtualizer.instance?.getVirtualItems() ?? [] as virtualRow (virtualRow.key)}
              {@const row = rows[virtualRow.index]}
              {#if row}
                <!-- svelte-ignore a11y_click_events_have_key_events -->
                <!-- svelte-ignore a11y_no_static_element_interactions -->
                <div
                  class="rail-row"
                  class:interactive={row.selectable}
                  title={rowTitle(row.id, row.label)}
                  style="position: absolute; top: 0; left: 0; width: 100%; height: {virtualRow.size}px; transform: translateY({virtualRow.start}px);"
                  onclick={() => handleSelect(row.id)}
                >
                  <span class="rail-rank">{virtualRow.index + 1}</span>
                  <span class="rail-dot" style="background: {row.color}"></span>
                  <span class="rail-label">{row.label}</span>
                  <span class="rail-cost">
                    {isTokenMode
                      ? formatTokenCount(row.value)
                      : formatMoney(moneyFromMicrodollars(row.value))}
                  </span>
                </div>
              {/if}
            {/each}
          </div>
        </div>
      </div>
    {:else}
      <div class="list-view" bind:this={listScrollElement}>
        <div
          class="list-virtual-spacer"
          style="height: {listVirtualizer.instance?.getTotalSize() ?? 0}px; position: relative;"
        >
          {#each listVirtualizer.instance?.getVirtualItems() ?? [] as virtualRow (virtualRow.key)}
            {@const row = rows[virtualRow.index]}
            {#if row}
              <!-- svelte-ignore a11y_click_events_have_key_events -->
              <!-- svelte-ignore a11y_no_static_element_interactions -->
              <div
                class="list-row"
                class:interactive={row.selectable}
                title={rowTitle(row.id, row.label)}
                style="position: absolute; top: 0; left: 0; width: 100%; height: {virtualRow.size}px; transform: translateY({virtualRow.start}px);"
                onclick={() => handleSelect(row.id)}
              >
                <span class="list-rank">{virtualRow.index + 1}</span>
                <span class="list-dot" style="background: {row.color}"></span>
                <div class="list-info">
                  <span class="list-label">{row.label}</span>
                  <div class="list-bar-track">
                    <div
                      class="list-bar-fill"
                      style="width: {Math.max(row.pct * 100, 1)}%; background: {row.color};"
                    ></div>
                  </div>
                </div>
                <span class="list-pct">{(row.pct * 100).toFixed(1)}%</span>
                <span class="list-cost">
                  {isTokenMode
                    ? formatTokenCount(row.value)
                    : formatMoney(moneyFromMicrodollars(row.value))}
                </span>
              </div>
            {/if}
          {/each}
        </div>
      </div>
    {/if}
  {/if}
</div>

<style>
  .attribution-panel {
    display: flex;
    flex-direction: column;
  }

  .panel-header {
    display: flex;
    align-items: center;
    justify-content: space-between;
    margin-bottom: 12px;
    flex-wrap: wrap;
    gap: 8px;
  }

  .chart-title {
    font-size: 12px;
    font-weight: 600;
    color: var(--text-primary);
  }

  .toggles {
    display: flex;
    gap: 8px;
  }

  .segment-toggle {
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

  /* Treemap layout: main + side rail */
  .treemap-layout {
    display: grid;
    grid-template-columns: 2.4fr 1fr;
    gap: 12px;
    min-height: 260px;
  }

  .treemap-main {
    overflow: hidden;
    border-radius: var(--radius-md);
  }

  .side-rail {
    overflow-y: auto;
    max-height: 280px;
  }

  .rail-row {
    display: flex;
    align-items: center;
    gap: 6px;
    padding: 3px 4px;
    border-radius: var(--radius-sm);
    transition: background 0.1s;
    box-sizing: border-box;
  }

  .rail-row.interactive {
    cursor: pointer;
  }

  .rail-row.interactive:hover {
    background: var(--bg-surface-hover);
  }

  .rail-rank {
    width: 14px;
    text-align: right;
    font-size: 9px;
    font-weight: 600;
    color: var(--text-muted);
    font-family: var(--font-mono);
  }

  .rail-dot {
    width: 6px;
    height: 6px;
    border-radius: 50%;
    flex-shrink: 0;
  }

  .rail-label {
    flex: 1;
    font-size: 10px;
    color: var(--text-secondary);
    white-space: nowrap;
    overflow: hidden;
    text-overflow: ellipsis;
  }

  .rail-cost {
    font-size: 10px;
    font-weight: 500;
    font-family: var(--font-mono);
    color: var(--text-primary);
  }

  /* List view */
  .list-view {
    max-height: 420px;
    overflow-y: auto;
  }

  .list-row {
    display: flex;
    align-items: center;
    gap: 8px;
    padding: 4px 6px;
    border-radius: var(--radius-sm);
    transition: background 0.1s;
    box-sizing: border-box;
  }

  .list-row.interactive {
    cursor: pointer;
  }

  .list-row.interactive:hover {
    background: var(--bg-surface-hover);
  }

  .list-rank {
    width: 18px;
    text-align: right;
    font-size: 10px;
    font-weight: 600;
    color: var(--text-muted);
    font-family: var(--font-mono);
  }

  .list-dot {
    width: 8px;
    height: 8px;
    border-radius: 50%;
    flex-shrink: 0;
  }

  .list-info {
    flex: 1;
    min-width: 0;
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
  }

  .list-label {
    font-size: 11px;
    color: var(--text-secondary);
    white-space: nowrap;
    overflow: hidden;
    text-overflow: ellipsis;
  }

  .list-bar-track {
    height: 4px;
    background: var(--bg-inset);
    border-radius: 2px;
    overflow: hidden;
  }

  .list-bar-fill {
    height: 100%;
    border-radius: 2px;
    transition: width 0.3s ease;
  }

  .list-pct {
    flex-shrink: 0;
    min-width: 36px;
    text-align: right;
    font-size: 10px;
    font-family: var(--font-mono);
    color: var(--text-muted);
  }

  .list-cost {
    flex-shrink: 0;
    min-width: 48px;
    text-align: right;
    font-size: 11px;
    font-weight: 500;
    font-family: var(--font-mono);
    color: var(--accent-blue);
  }

  .empty {
    color: var(--text-muted);
    font-size: 12px;
    padding: 24px;
    text-align: center;
  }

  .hint {
    font-size: 10px;
    color: var(--text-muted);
    margin-bottom: 6px;
    font-style: italic;
  }

  @media (max-width: 640px) {
    .treemap-layout {
      grid-template-columns: 1fr;
    }
  }
</style>
