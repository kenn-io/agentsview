<script lang="ts">
  import {
    usage,
    type GroupBy,
    type AttributionView,
  } from "../../stores/usage.svelte.js";
  import { Button } from "@kenn-io/kit-ui";
  import { shortenId } from "../../utils/shortId.js";
  import Treemap from "./Treemap.svelte";
  import { m } from "../../i18n/index.js";
  import { formatMoney, moneyFromMicrodollars } from "../../money.js";
  import { formatTokenCount } from "../../utils/format.js";
  import { sumSelectedTokens } from "../../stores/usageTokenTypes.js";
  import type { DbTopSessionEntry } from "../../api/generated/index.js";

  interface Props {
    colorMap: ReadonlyMap<string, string>;
  }

  let { colorMap }: Props = $props();

  function fmtPct(v: number, total: number): string {
    if (total <= 0) return "";
    return `${((v / total) * 100).toFixed(1)}%`;
  }

  const groupBy = $derived(usage.toggles.attribution.groupBy);
  const hasSelection = $derived(usage.hasSelection(groupBy));
  const view = $derived(usage.toggles.attribution.view);
  const isTokenMode = $derived(usage.mode === "token");

  interface Row {
    id: string;
    label: string;
    value: number;
    color: string;
    pct: number;
  }

  let panel: HTMLElement;

  const zoomedProject = $derived(usage.zoomedProject);

  function zoomRowId(row: { groupKey?: string; machine?: string; sessionId: string }): string {
    return row.groupKey ? `group:${JSON.stringify([row.groupKey, row.machine ?? ""])}` : row.sessionId ? `session:${row.sessionId}` : "remainder";
  }

  function zoomRowText(row: DbTopSessionEntry, rows: DbTopSessionEntry[]): { label: string; title: string } {
    const name = (item: DbTopSessionEntry) => item.groupKey ? item.groupLabel || item.groupKey : item.sessionId ? item.displayName : m.shared_other();
    const key = (item: DbTopSessionEntry) => item.groupKey || item.sessionId;
    // Cron keys are "<job ID>:<home hash>"; job IDs never contain a colon.
    const job = (item: DbTopSessionEntry) => item.groupKey ? item.groupKey.split(":")[0]! : item.sessionId;
    const home = (item: DbTopSessionEntry) => item.groupKey ? item.groupKey.slice(job(item).length + 1) : "";
    const peers = rows.filter((other) => name(other) === name(row));
    const jobs = peers.map(job);
    const homes = peers.filter((other) => job(other) === job(row)).map(home);
    const machines = peers.filter((other) => key(other) === key(row)).map((other) => other.machine ?? "");
    const label = [name(row)];
    if (jobs.some((other) => other !== job(row))) label.push(shortenId(job(row), jobs));
    if (new Set(homes).size > 1) label.push(shortenId(home(row), homes));
    if (machines.length > 1) label.push(shortenId(row.machine ?? "", machines));
    return {
      label: label.filter(Boolean).join(" · "),
      title: [name(row), job(row) === name(row) ? "" : job(row), row.machine].filter(Boolean).join(" · "),
    };
  }

  function handleBackKey(event: KeyboardEvent) {
    const target = event.target as HTMLElement;
    if (!zoomedProject || !panel.contains(document.activeElement) || target.closest("input, textarea, select, [contenteditable]")) return;
    if (event.key === "Escape" || event.key === "Backspace") {
      event.preventDefault();
      usage.setOpenProject(null);
    }
  }

  const rowItems = $derived.by(() => {
    const s = usage.attributionSummary ?? usage.summary;
    if (!s && !zoomedProject) return [];

    let items: Array<{
      id: string;
      label: string;
      value: number;
    }> = [];

    if (zoomedProject && groupBy === "project") {
      const zoomRows = usage.zoomRows ?? [];
      items = zoomRows.map((row) => {
        const id = zoomRowId(row);
        return {
          id,
          label: zoomRowText(row, zoomRows).label,
          value: isTokenMode ? sumSelectedTokens(row, usage.selectedTokenTypes) : row.cost.microdollars,
        };
      });
    } else if (groupBy === "project") {
      items = s!.projectTotals.map((p) => ({
        id: p.project_key,
        label: p.project,
        value: isTokenMode
          ? sumSelectedTokens(p, usage.selectedTokenTypes)
          : p.cost.microdollars,
      }));
    } else if (groupBy === "model") {
      items = s!.modelTotals.map((m) => ({
        id: m.model,
        label: m.model,
        value: isTokenMode
          ? sumSelectedTokens(m, usage.selectedTokenTypes)
          : m.cost.microdollars,
      }));
    } else {
      items = s!.agentTotals.map((a) => ({
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
      color: zoomedProject
        ? colorMap.get(zoomedProject.key) ?? "var(--text-muted)"
        : colorMap.get(d.id) ?? "var(--text-muted)",
      pct: total > 0 ? d.value / total : 0,
    }));
  });

  const treemapItems = $derived(
    rows.map((r) => ({
      id: r.id,
      label: r.label,
      value: r.value,
      color: r.color,
      title: rowTitle(r.id, r.label),
      selected: usage.isSelected(groupBy, r.id),
      dimmed: !zoomedProject && hasSelection && !usage.isSelected(groupBy, r.id),
      meta: fmtPct(r.value, rows.reduce(
        (sum, item) => sum + item.value, 0,
      )),
    })),
  );

  function rowTitle(id: string, label: string): string {
    if (zoomedProject) {
      const row = usage.zoomRows?.find((row) => zoomRowId(row) === id);
      return row ? zoomRowText(row, usage.zoomRows!).title : label;
    }
    return groupBy === "project" ? m.usage_click_project({ label }) : m.usage_click_to_focus({ label });
  }

  function handleSelect(id: string) {
    usage.toggleSelection(groupBy, id);
  }

  function handleOpen(id: string) {
    if (groupBy !== "project") return;
    const project = rows.find((row) => row.id === id);
    if (!project) return;
    usage.setOpenProject(project.id);
    panel.focus();
  }

  function handleClick(event: MouseEvent, id: string) {
    // A second click opens a project instead of toggling it back.
    if (event.detail < 2 || groupBy !== "project") handleSelect(id);
  }

  function handleKey(event: KeyboardEvent, id: string) {
    if (event.key === "Enter" || event.key === " ") {
      event.preventDefault();
      handleSelect(id);
    }
  }

  function handleGroupByChange(g: GroupBy) {
    usage.setAttributionGroupBy(g);
  }

  function handleViewChange(v: AttributionView) {
    usage.setAttributionView(v);
  }
</script>

<svelte:window onkeydown={handleBackKey} />

<section class="attribution-panel" aria-label={isTokenMode ? m.usage_tokens_attribution_title() : m.usage_cost_attribution_title()} tabindex="-1" bind:this={panel}>
  <div class="panel-header">
    {#if zoomedProject}
      <Button surface="soft" label={`← ${m.data_workspace_all_projects()}`} onclick={() => usage.setOpenProject(null)} />
      <h3 class="chart-title">{zoomedProject.label}</h3>
    {:else}
      <h3 class="chart-title">
        {isTokenMode
          ? m.usage_tokens_attribution_title()
          : m.usage_cost_attribution_title()}
      </h3>
    {/if}
    <div class="toggles">
      <div class="selection-actions" class:inactive={!hasSelection}>
        <Button size="sm" surface="soft" label={m.sidebar_clear_selection()} onclick={() => usage.clearSelection(groupBy)} />
        {#if groupBy === "project"}
          <span class:inactive={!usage.selectedProjectKey || !!zoomedProject || !rows.some((row) => row.id === usage.selectedProjectKey)}>
            <Button size="sm" surface="soft" label={m.breadcrumb_open()} title={usage.focusLabel} onclick={() => handleOpen(usage.selectedProjectKey)} />
          </span>
        {/if}
      </div>
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
  {#if zoomedProject && usage.errors.zoom}
    <div class="empty">{usage.errors.zoom}</div>
  {:else if zoomedProject && usage.loading.zoom && usage.zoomRows === null}
    <div class="empty">{m.subagent_inline_loading()}</div>
  {:else if rows.length === 0}
    <div class="empty">{m.shared_no_data_for_period()}</div>
  {:else}
    {#if !zoomedProject}<div class="hint">{groupBy === "project" ? m.usage_click_project_hint() : m.usage_click_to_focus_hint()}</div>{/if}
    {#if view === "treemap"}
      <div class="treemap-layout">
        <div class="treemap-main">
          <Treemap
            items={treemapItems}
            height={260}
            onSelect={zoomedProject ? undefined : handleSelect}
            onOpen={!zoomedProject && groupBy === "project" ? handleOpen : undefined}
            formatValue={isTokenMode ? formatTokenCount : undefined}
          />
        </div>
        <div class="side-rail">
          {#each rows as row, i (row.id)}
            <!-- svelte-ignore a11y_no_noninteractive_tabindex (Only selectable rows receive a button role and tab stop.) -->
            <div
              class="rail-row"
              class:selected={usage.isSelected(groupBy, row.id)}
              class:dimmed={!zoomedProject && hasSelection && !usage.isSelected(groupBy, row.id)}
              role={zoomedProject ? undefined : "button"}
              tabindex={zoomedProject ? undefined : 0}
              aria-pressed={zoomedProject ? undefined : usage.isSelected(groupBy, row.id)}
              title={rowTitle(row.id, row.label)}
              onclick={zoomedProject ? undefined : (event) => handleClick(event, row.id)}
              ondblclick={!zoomedProject && groupBy === "project" ? () => handleOpen(row.id) : undefined}
              onkeydown={zoomedProject ? undefined : (event) => handleKey(event, row.id)}
            >
              <span class="rail-rank">{i + 1}</span>
              <span
                class="rail-dot"
                style="background: {row.color}"
              ></span>
              <span class="rail-label">{row.label}</span>
              <span class="rail-cost">
                {isTokenMode
                  ? formatTokenCount(row.value)
                  : formatMoney(moneyFromMicrodollars(row.value))}
              </span>
            </div>
          {/each}
        </div>
      </div>
    {:else}
      <div class="list-view">
        {#each rows as row, i (row.id)}
          <!-- svelte-ignore a11y_no_noninteractive_tabindex (Only selectable rows receive a button role and tab stop.) -->
          <div
            class="list-row"
            class:selected={usage.isSelected(groupBy, row.id)}
            class:dimmed={!zoomedProject && hasSelection && !usage.isSelected(groupBy, row.id)}
            role={zoomedProject ? undefined : "button"}
            tabindex={zoomedProject ? undefined : 0}
            aria-pressed={zoomedProject ? undefined : usage.isSelected(groupBy, row.id)}
            title={rowTitle(row.id, row.label)}
            onclick={zoomedProject ? undefined : (event) => handleClick(event, row.id)}
            ondblclick={!zoomedProject && groupBy === "project" ? () => handleOpen(row.id) : undefined}
            onkeydown={zoomedProject ? undefined : (event) => handleKey(event, row.id)}
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
            <span class="list-cost">
              {isTokenMode
                ? formatTokenCount(row.value)
                : formatMoney(moneyFromMicrodollars(row.value))}
            </span>
          </div>
        {/each}
      </div>
    {/if}
  {/if}
</section>

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
    flex-wrap: nowrap;
    min-height: 32px;
    gap: 8px;
  }

  .chart-title {
    min-width: 0;
    font-size: 12px;
    font-weight: 600;
    color: var(--text-primary);
  }

  .toggles {
    display: flex;
    flex-wrap: wrap;
    justify-content: flex-end;
    gap: 8px;
  }

  .selection-actions {
    display: flex;
    align-items: center;
    gap: 8px;
  }

  .inactive {
    visibility: hidden;
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
    transition: background 0.1s;
  }

  .rail-row[role="button"]:hover {
    background: var(--bg-surface-hover);
  }

  .rail-row, .list-row {
    touch-action: manipulation;
  }

  .dimmed {
    opacity: 0.35;
  }

  .rail-row.selected, .list-row.selected {
    background: var(--bg-surface-hover);
  }

  .rail-row[role="button"], .list-row[role="button"] {
    cursor: pointer;
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
    transition: background 0.1s;
  }

  .list-row[role="button"]:hover {
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
    line-height: 14px;
    height: 14px;
    font-style: italic;
  }

  @media (max-width: 640px) {
    .treemap-layout {
      grid-template-columns: 1fr;
    }
  }
</style>
