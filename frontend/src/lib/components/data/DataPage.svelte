<script lang="ts">
  import { onMount } from "svelte";
  import { m } from "../../i18n/index.js";
  import { data } from "../../stores/data.svelte.js";
  import { sync } from "../../stores/sync.svelte.js";
  import type { DbProjectInventoryRow } from "../../api/generated/index";
  import type { ProjectInfo } from "../../api/types/core.js";
  import ProjectInventoryTable from "./ProjectInventoryTable.svelte";
  import ProjectWorkspace from "./ProjectWorkspace.svelte";
  import WorktreeMappingRules from "./WorktreeMappingRules.svelte";
  import { SegmentedControl, type SegmentedControlOption } from "@kenn-io/kit-ui";

  const viewOptions: SegmentedControlOption[] = $derived([
    { value: "inventory", label: m.data_view_inventory() },
    { value: "rules", label: m.data_view_rules() },
  ]);

  let workspaceGeneration = $state(0);

  const dataReadOnly = $derived(sync.serverVersion === null || sync.readOnly);

  const inventoryProjects = $derived.by((): ProjectInfo[] => {
    const rows = (data.inventory?.projects ?? []) as DbProjectInventoryRow[];
    return rows.map((row) => ({ name: row.label, session_count: row.sessions }));
  });

  function onViewChange(value: string) {
    if (value === "rules") data.showRules();
    else data.showInventory();
  }

  function selectProjectByLabel(label: string) {
    const rows = (data.inventory?.projects ?? []) as DbProjectInventoryRow[];
    const row = rows.find((r) => r.label === label);
    if (row) {
      data.selectProject(row.project_key);
    } else {
      // The inventory may not have loaded yet, or the rule may target a
      // project with no visible sessions; fall back to the plain inventory.
      data.showInventory();
      void data.load();
    }
  }

  function closeWorkspace() {
    const key = data.selectedProjectKey;
    data.clearSelection();
    requestAnimationFrame(() => {
      // Match on dataset instead of an attribute selector so arbitrary
      // project keys never need CSS escaping.
      for (const el of document.querySelectorAll<HTMLElement>("[data-project-key]")) {
        if (el.dataset.projectKey === key) {
          el.focus();
          return;
        }
      }
    });
  }

  onMount(() => {
    const detach = data.attach();
    void data.load();
    return () => {
      data.cancelInFlightReads();
      detach();
    };
  });
</script>

<div class="data-page">
  <div class="data-header">
    <h2>{m.data_projects_heading()}</h2>
    <SegmentedControl
      options={viewOptions}
      value={data.view}
      ariaLabel={m.data_view_toggle_label()}
      onchange={onViewChange}
    />
  </div>

  {#if data.view === "rules"}
    <!-- The rules component captures its machine prop once at mount, so
         store-driven machine changes (popstate on ?machine=, internal
         selections routed through setRulesMachine) remount it. The remount
         resets form state exactly like the component's own selectMachine,
         and the destroyed component's LatestRead cancels its in-flight
         load. -->
    {#key data.rulesMachine}
      <WorktreeMappingRules
        readOnly={dataReadOnly}
        machine={data.rulesMachine}
        onMachineChange={(machine) => data.setRulesMachine(machine)}
        onSelectProject={selectProjectByLabel}
        onMutated={() => void data.load({ background: true })}
      />
    {/key}
  {:else if data.inventory}
    <!-- Inventory-first ordering: once inventory has loaded once it keeps
         rendering through background reloads; loading/error below only
         apply before that first successful load. -->
    <div class="summary-strip">
      <span>{m.data_summary_projects({ count: data.inventory.total_projects })}</span>
      <span>{m.data_summary_sessions({ count: data.inventory.total_sessions })}</span>
      <span>{m.data_summary_governed({ count: data.inventory.governed_sessions })}</span>
    </div>

    {#if data.unknownProjectKey}
      <div class="notice" role="status">{m.data_unknown_project_key()}</div>
    {/if}

    {#if data.inventory.total_projects === 0}
      <div class="status">{m.data_empty()}</div>
    {:else}
      <div class="split" class:has-selection={data.selectedRow !== null}>
        <div class="pane-table">
          <ProjectInventoryTable
            inventory={data.inventory}
            selectedKey={data.selectedProjectKey}
            onSelect={(key) => data.selectProject(key)}
          />
        </div>
        {#if data.selectedRow}
          <div class="pane-detail">
            {#key `${data.selectedProjectKey}:${workspaceGeneration}`}
              <ProjectWorkspace
                row={data.selectedRow}
                projects={inventoryProjects}
                readOnly={dataReadOnly}
                onClose={closeWorkspace}
                onRefresh={(key, target) => data.refreshAfterApply(key, target)}
                onComplete={() => (workspaceGeneration += 1)}
                onOpenRules={(machine) => data.showRules(machine)}
              />
            {/key}
          </div>
        {/if}
      </div>
    {/if}
  {:else if data.loading}
    <div class="status">{m.data_loading()}</div>
  {:else if data.error}
    <div class="status error">
      <span>{data.error}</span>
      <button class="retry-btn" onclick={() => data.load()}>{m.shared_retry()}</button>
    </div>
  {/if}
</div>

<style>
  .data-page {
    display: flex;
    flex: 1;
    flex-direction: column;
    gap: 12px;
    padding: 12px;
    min-height: 0;
    overflow-y: auto;
  }

  .data-header {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: 12px;
  }

  h2 {
    margin: 0;
    font-size: 14px;
  }

  .summary-strip {
    display: flex;
    align-items: center;
    gap: 16px;
    font-size: 11px;
    color: var(--text-muted);
  }

  .split {
    display: flex;
    gap: 12px;
    min-height: 0;
    flex: 1;
  }

  .pane-table {
    flex: 1 1 55%;
    min-width: 0;
  }

  .pane-detail {
    flex: 1 1 45%;
    min-width: 0;
  }

  @media (max-width: 760px) {
    .split.has-selection .pane-table {
      display: none;
    }

    .pane-detail {
      flex-basis: 100%;
    }
  }

  .notice {
    padding: 8px 12px;
    border: 1px solid var(--border-muted);
    border-radius: var(--radius-sm);
    background: var(--bg-surface);
    color: var(--text-secondary);
    font-size: 11px;
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
