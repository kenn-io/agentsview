<script lang="ts">
  import { onMount } from "svelte";
  import { m } from "../../i18n/index.js";
  import { data } from "../../stores/data.svelte.js";
  import { sync } from "../../stores/sync.svelte.js";
  import type { DbProjectInventoryRow } from "../../api/generated/index";
  import type { ProjectInfo } from "../../api/types/core.js";
  import ProjectInventoryTable from "./ProjectInventoryTable.svelte";
  import ProjectBatchWorkspace from "./ProjectBatchWorkspace.svelte";
  import ProjectWorkspace from "./ProjectWorkspace.svelte";
  import WorktreeMappingRules from "./WorktreeMappingRules.svelte";
  import {
    FlashBanner,
    SegmentedControl,
    showFlash,
    type SegmentedControlOption,
  } from "@kenn-io/kit-ui";
  import { PROJECT_MAPPING_WORKSPACE_ENABLED } from "../../feature-flags.js";

  interface Props {
    projectWorkspaceEnabled?: boolean;
  }

  let {
    projectWorkspaceEnabled = PROJECT_MAPPING_WORKSPACE_ENABLED,
  }: Props = $props();

  const viewOptions: SegmentedControlOption[] = $derived([
    { value: "inventory", label: m.data_view_inventory() },
    { value: "rules", label: m.data_view_rules() },
  ]);

  let workspaceGeneration = $state(0);
  let selectedProjectKeys = $state<string[]>([]);

  const dataReadOnly = $derived(sync.serverVersion === null || sync.readOnly);

  const inventoryProjects = $derived.by((): ProjectInfo[] => {
    const rows = (data.inventory?.projects ?? []) as DbProjectInventoryRow[];
    return rows.map((row) => ({ name: row.label, session_count: row.sessions }));
  });

  const tableSelectedKeys = $derived(
    selectedProjectKeys.length > 0
      ? selectedProjectKeys
      : data.selectedProjectKey
        ? [data.selectedProjectKey]
        : [],
  );

  const selectedRows = $derived.by((): DbProjectInventoryRow[] => {
    const rows = (data.inventory?.projects ?? []) as DbProjectInventoryRow[];
    const keys = new Set(tableSelectedKeys);
    return rows.filter((row) => keys.has(row.project_key));
  });

  function onViewChange(value: string) {
    if (value === "rules") {
      selectedProjectKeys = [];
      data.showRules();
    }
    else data.showInventory();
  }

  function openRules(machine: string) {
    selectedProjectKeys = [];
    data.showRules(machine);
  }

  function selectProjectByLabel(label: string) {
    const rows = (data.inventory?.projects ?? []) as DbProjectInventoryRow[];
    const row = rows.find((r) => r.label === label);
    if (row) {
      selectedProjectKeys = [row.project_key];
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
    selectedProjectKeys = [];
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

  function selectProjects(activeKey: string, keys: string[]) {
    selectedProjectKeys = keys;
    data.selectProject(activeKey);
  }

  async function refreshSingleCorrection(key: string, target: string): Promise<boolean> {
    const refreshed = await data.refreshAfterApply(key, target);
    if (refreshed) {
      selectedProjectKeys = data.selectedProjectKey ? [data.selectedProjectKey] : [];
    }
    return refreshed;
  }

  async function refreshBatchCorrection(target: string): Promise<boolean> {
    const refreshed = await data.load({ background: true });
    if (!refreshed) return false;
    const rows = (data.inventory?.projects ?? []) as DbProjectInventoryRow[];
    const targetRow = rows.find((row) => row.label === target);
    selectedProjectKeys = targetRow ? [targetRow.project_key] : [];
    if (targetRow) data.selectProject(targetRow.project_key);
    else data.clearSelection();
    return true;
  }

  function completeCorrection(target: string) {
    workspaceGeneration += 1;
    showFlash(m.data_reclassify_saved({ project: target }), { tone: "success" });
  }

  function completeBatchCorrection(target: string, _count: number) {
    workspaceGeneration += 1;
    showFlash(m.data_batch_saved({ project: target }), { tone: "success" });
  }

  onMount(() => {
    const detach = data.attach(projectWorkspaceEnabled);
    const clearRangeSelection = () => {
      selectedProjectKeys = [];
    };
    window.addEventListener("popstate", clearRangeSelection);
    if (projectWorkspaceEnabled) void data.load();
    return () => {
      window.removeEventListener("popstate", clearRangeSelection);
      data.cancelInFlightReads();
      detach();
    };
  });
</script>

<div class="data-page">
  <FlashBanner toneLabels={{ success: m.data_flash_success_label() }} />
  {#if projectWorkspaceEnabled}
    <div class="data-header">
      <h2>{m.data_projects_heading()}</h2>
      <SegmentedControl
        options={viewOptions}
        value={data.view}
        ariaLabel={m.data_view_toggle_label()}
        onchange={onViewChange}
      />
    </div>
  {/if}

  {#if !projectWorkspaceEnabled || data.view === "rules"}
    <!-- The rules component captures its machine prop once at mount, so
         store-driven machine changes remount it and reset machine-specific
         form state. Background refreshes stay in place so drafts survive. -->
    {#key data.rulesMachine}
      <WorktreeMappingRules
        readOnly={dataReadOnly}
        machine={data.rulesMachine}
        refreshVersion={data.rulesRefreshVersion}
        onMachineChange={(machine) => data.setRulesMachine(machine)}
        onSelectProject={projectWorkspaceEnabled ? selectProjectByLabel : undefined}
        onMutated={projectWorkspaceEnabled
          ? () => void data.load({ background: true })
          : undefined}
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
      <div class="split" class:has-selection={selectedRows.length > 0}>
        <div class="pane-table">
          <ProjectInventoryTable
            inventory={data.inventory}
            selectedKeys={tableSelectedKeys}
            onSelect={selectProjects}
            onClear={closeWorkspace}
          />
        </div>
        {#if selectedRows.length > 1}
          <div class="pane-detail">
            {#key `${tableSelectedKeys.join(":")}:${workspaceGeneration}`}
              <ProjectBatchWorkspace
                rows={selectedRows}
                projects={inventoryProjects}
                readOnly={dataReadOnly}
                onClose={closeWorkspace}
                onRefresh={refreshBatchCorrection}
                onComplete={completeBatchCorrection}
                onOpenRules={openRules}
              />
            {/key}
          </div>
        {:else if selectedRows[0]}
          <div class="pane-detail">
            {#key `${data.selectedProjectKey}:${workspaceGeneration}`}
              <ProjectWorkspace
                row={selectedRows[0]}
                projects={inventoryProjects}
                readOnly={dataReadOnly}
                onClose={closeWorkspace}
                onRefresh={refreshSingleCorrection}
                onComplete={completeCorrection}
                onOpenRules={openRules}
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
    overflow: hidden;
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
    padding: 0 2px;
  }

  .split {
    display: grid;
    grid-template-columns: minmax(0, 1fr);
    gap: var(--space-5);
    min-height: 0;
    flex: 1;
  }

  .split.has-selection {
    grid-template-columns: minmax(380px, 1.15fr) minmax(340px, 0.85fr);
  }

  .pane-table,
  .pane-detail {
    min-width: 0;
    min-height: 0;
    overflow: hidden;
    border: 1px solid var(--border-muted);
    border-radius: var(--radius-sm);
    background: var(--bg-surface);
  }

  @media (max-width: 760px) {
    .split.has-selection {
      grid-template-columns: minmax(0, 1fr);
    }

    .split.has-selection .pane-table {
      display: none;
    }

    .pane-detail {
      grid-column: 1 / -1;
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
