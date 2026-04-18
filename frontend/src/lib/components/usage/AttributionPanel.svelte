<script lang="ts">
  import {
    usage,
    type GroupBy,
    type AttributionView,
  } from "../../stores/usage.svelte.js";
  import { projectColor } from "../../utils/projectColor.js";
  import Treemap from "./Treemap.svelte";

  function fmtCost(v: number): string {
    if (v >= 100) return `$${v.toFixed(0)}`;
    return `$${v.toFixed(2)}`;
  }

  function fmtPct(v: number, total: number): string {
    if (total <= 0) return "";
    return `${((v / total) * 100).toFixed(1)}%`;
  }

  const groupBy = $derived(usage.toggles.attribution.groupBy);
  const view = $derived(usage.toggles.attribution.view);

  interface Row {
    id: string;
    label: string;
    cost: number;
    color: string;
    pct: number;
  }

  const rows = $derived.by((): Row[] => {
    const s = usage.summary;
    if (!s) return [];

    let items: Array<{
      id: string;
      label: string;
      cost: number;
    }> = [];

    if (groupBy === "project") {
      items = s.projectTotals.map((p) => ({
        id: p.project,
        label: p.project,
        cost: p.cost,
      }));
    } else if (groupBy === "model") {
      items = s.modelTotals.map((m) => ({
        id: m.model,
        label: m.model,
        cost: m.cost,
      }));
    } else {
      items = s.agentTotals.map((a) => ({
        id: a.agent,
        label: a.agent,
        cost: a.cost,
      }));
    }

    items.sort((a, b) => b.cost - a.cost);
    const total = items.reduce((s, d) => s + d.cost, 0);

    return items.map((d) => ({
      id: d.id,
      label: d.label,
      cost: d.cost,
      color: projectColor(d.id),
      pct: total > 0 ? d.cost / total : 0,
    }));
  });

  const treemapItems = $derived(
    rows.map((r) => ({
      id: r.id,
      label: r.label,
      value: r.cost,
      color: r.color,
      meta: fmtPct(r.cost, rows.reduce(
        (s, d) => s + d.cost, 0,
      )),
    })),
  );

  function handleSelect(id: string) {
    if (groupBy === "project") {
      usage.toggleProject(id);
    } else if (groupBy === "agent") {
      usage.toggleAgent(id);
    } else {
      usage.toggleModel(id);
    }
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
    <h3 class="chart-title">Cost Attribution</h3>
    <div class="toggles">
      <div class="segment-toggle">
        <button
          class="toggle-btn"
          class:active={groupBy === "project"}
          onclick={() => handleGroupByChange("project")}
        >
          Project
        </button>
        <button
          class="toggle-btn"
          class:active={groupBy === "model"}
          onclick={() => handleGroupByChange("model")}
        >
          Model
        </button>
        <button
          class="toggle-btn"
          class:active={groupBy === "agent"}
          onclick={() => handleGroupByChange("agent")}
        >
          Agent
        </button>
      </div>
      <div class="segment-toggle">
        <button
          class="toggle-btn"
          class:active={view === "treemap"}
          onclick={() => handleViewChange("treemap")}
        >
          Treemap
        </button>
        <button
          class="toggle-btn"
          class:active={view === "list"}
          onclick={() => handleViewChange("list")}
        >
          List
        </button>
      </div>
    </div>
  </div>

  {#if rows.length === 0}
    <div class="empty">No data for this period</div>
  {:else}
    <div class="hint">Click to hide from chart</div>
    {#if view === "treemap"}
      <div class="treemap-layout">
        <div class="treemap-main">
          <Treemap
            items={treemapItems}
            height={260}
            onSelect={handleSelect}
          />
        </div>
        <div class="side-rail">
          {#each rows as row, i (row.id)}
            <!-- svelte-ignore a11y_click_events_have_key_events -->
            <!-- svelte-ignore a11y_no_static_element_interactions -->
            <div
              class="rail-row"
              title="Click to hide {row.label}"
              onclick={() => handleSelect(row.id)}
            >
              <span class="rail-rank">{i + 1}</span>
              <span
                class="rail-dot"
                style="background: {row.color}"
              ></span>
              <span class="rail-label">{row.label}</span>
              <span class="rail-cost">{fmtCost(row.cost)}</span>
            </div>
          {/each}
        </div>
      </div>
    {:else}
      <div class="list-view">
        {#each rows as row, i (row.id)}
          <!-- svelte-ignore a11y_click_events_have_key_events -->
          <!-- svelte-ignore a11y_no_static_element_interactions -->
          <div
            class="list-row"
            title="Click to hide {row.label}"
            onclick={() => handleSelect(row.id)}
          >
            <span class="list-rank">{i + 1}</span>
            <span
              class="list-dot"
              style="background: {row.color}"
            ></span>
            <div class="list-info">
              <span class="list-label">{row.label}</span>
              <div class="list-bar-track">
                <div
                  class="list-bar-fill"
                  style="width: {Math.max(row.pct * 100, 1)}%;
                         background: {row.color};"
                ></div>
              </div>
            </div>
            <span class="list-pct">
              {(row.pct * 100).toFixed(1)}%
            </span>
            <span class="list-cost">{fmtCost(row.cost)}</span>
          </div>
        {/each}
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
    display: flex;
    flex-direction: column;
    gap: 2px;
    overflow-y: auto;
    max-height: 280px;
  }

  .rail-row {
    display: flex;
    align-items: center;
    gap: 6px;
    padding: 3px 4px;
    border-radius: var(--radius-sm);
    cursor: pointer;
    transition: background 0.1s;
  }

  .rail-row:hover {
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
    display: flex;
    flex-direction: column;
    gap: 4px;
  }

  .list-row {
    display: flex;
    align-items: center;
    gap: 8px;
    padding: 4px 6px;
    border-radius: var(--radius-sm);
    cursor: pointer;
    transition: background 0.1s;
  }

  .list-row:hover {
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
    gap: 3px;
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

  @media (max-width: 600px) {
    .treemap-layout {
      grid-template-columns: 1fr;
    }
  }
</style>
