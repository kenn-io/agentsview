<script lang="ts">
  import {
    Button,
    Checkbox,
    Modal,
    TableHeaderCell,
    TextInput,
    Typeahead,
  } from "@kenn-io/kit-ui";
  import { onDestroy } from "svelte";
  import {
    DataService,
    SettingsService,
    type ApplyWorktreeMappingsResponse,
    type DataProjectRulesResponse,
    type DbProjectRule,
    type WorktreeMappingRequest,
  } from "../../api/generated/index";
  import { callGenerated, isAbortError } from "../../api/runtime.js";
  import { formatDateTime, m } from "../../i18n/index.js";
  import { LatestRead } from "../../utils/latest-read.js";

  interface Props {
    readOnly?: boolean;
    machine?: string;
    /**
     * When provided, machine selection is delegated to the host, which
     * remounts this component keyed on the machine; the remount resets local
     * state and performs the load. When omitted (standalone use), the
     * component switches machines locally.
     */
    onMachineChange?: (machine: string) => void;
    onSelectProject: (label: string) => void;
    /** Called after each successful create/update/delete/apply mutation. */
    onMutated?: () => void;
  }

  type Confirmation =
    | { kind: "delete"; mapping: DbProjectRule }
    | { kind: "disable"; id: number; input: WorktreeMappingRequest };

  const explicitLayout = "explicit";
  const repoDotWorktreesLayout = "repo_dot_worktrees";

  let {
    readOnly = false,
    machine: initialMachine = "",
    onMachineChange = undefined,
    onSelectProject,
    onMutated = undefined,
  }: Props = $props();

  // Deep link from the URL (via DataPage): preselect the rule's machine on
  // first load only, so later machine switches stay local until saved back
  // through onMachineChange.
  // svelte-ignore state_referenced_locally
  const requestedMachine = initialMachine;

  let localMachine = $state("");
  let machine = $state("");
  let machines: string[] = $state([]);
  let mappings: DbProjectRule[] = $state([]);
  let loading = $state(true);
  let saving = $state(false);
  let applying = $state(false);
  let error = $state("");
  let applyMessage = $state("");
  let editingId: number | null = $state(null);
  let editingWasEnabled = $state(false);
  let pathPrefix = $state("");
  let layout = $state(explicitLayout);
  let project = $state("");
  let enabled = $state(true);
  let confirmation: Confirmation | null = $state(null);
  const mappingsRead = new LatestRead();
  let machineGeneration = 0;
  let disposed = false;

  const machineOptions = $derived(
    machines.map((name) => ({ name, label: name, displayLabel: name })),
  );
  const isRepoDotWorktrees = $derived(layout === repoDotWorktreesLayout);
  const canSave = $derived(
    pathPrefix.trim() !== "" &&
      (layout === repoDotWorktreesLayout || project.trim() !== ""),
  );

  $effect(() => {
    void loadMappings(requestedMachine || undefined);
  });

  async function loadMappings(requestedMachine?: string) {
    if (disposed) return;
    const signal = mappingsRead.begin();
    loading = true;
    error = "";
    try {
      const res = await callGenerated(
        () =>
          DataService.getApiV1DataProjectRules({
            machine: requestedMachine || undefined,
          }),
        signal,
      ) as DataProjectRulesResponse;
      if (!mappingsRead.isCurrent(signal)) return;
      localMachine = res.local_machine;
      machine = res.machine;
      machines = Array.from(new Set([res.machine, ...((res.machines ?? []) as string[])]))
        .filter(Boolean);
      mappings = (res.rules ?? []) as DbProjectRule[];
    } catch (err) {
      if (isAbortError(err) || !mappingsRead.isCurrent(signal)) return;
      error = err instanceof Error ? err.message : m.worktree_failed_load();
    } finally {
      if (mappingsRead.finish(signal)) loading = false;
    }
  }

  onDestroy(() => {
    disposed = true;
    mappingsRead.cancel();
  });

  function resetForm() {
    editingId = null;
    editingWasEnabled = false;
    pathPrefix = "";
    layout = explicitLayout;
    project = "";
    enabled = true;
    confirmation = null;
  }

  function selectMachine(value: string) {
    if (!value || value === machine) return;
    if (onMachineChange) {
      // The host remounts this component keyed on the machine; the remount
      // resets local state and performs the load, so doing either here would
      // duplicate the work.
      onMachineChange(value);
      return;
    }
    machineGeneration += 1;
    machine = value;
    mappings = [];
    saving = false;
    applying = false;
    applyMessage = "";
    error = "";
    resetForm();
    void loadMappings(value);
  }

  function isCurrentMachine(initiatingMachine: string, generation: number) {
    return !disposed && machine === initiatingMachine && machineGeneration === generation;
  }

  function editMapping(mapping: DbProjectRule) {
    editingId = mapping.id;
    editingWasEnabled = mapping.enabled;
    pathPrefix = mapping.path_prefix;
    layout = mapping.layout || explicitLayout;
    project = mapping.project;
    enabled = mapping.enabled;
    applyMessage = "";
    error = "";
  }

  function mappingInput(): WorktreeMappingRequest | null {
    const input = {
      machine,
      path_prefix: pathPrefix.trim(),
      layout,
      project: project.trim(),
      enabled,
    } satisfies WorktreeMappingRequest;
    if (!input.path_prefix) return null;
    if (layout !== repoDotWorktreesLayout && !input.project) return null;
    return input;
  }

  async function requestSave() {
    const input = mappingInput();
    if (!input) return;
    if (editingId != null && editingWasEnabled && !input.enabled) {
      confirmation = { kind: "disable", id: editingId, input };
      return;
    }
    await saveMapping(input, editingId);
  }

  async function saveMapping(input: WorktreeMappingRequest, id: number | null) {
    const initiatingMachine = input.machine ?? machine;
    const generation = machineGeneration;
    saving = true;
    error = "";
    applyMessage = "";
    try {
      if (id == null) {
        await callGenerated(() =>
          SettingsService.postApiV1SettingsWorktreeMappings({ requestBody: input }),
        );
      } else {
        await callGenerated(() =>
          SettingsService.putApiV1SettingsWorktreeMappingsId({
            id: String(id),
            requestBody: input,
          }),
        );
      }
      // The mutation committed even if the machine selection has since
      // changed, so the host's cached inventory is stale either way.
      onMutated?.();
      if (!isCurrentMachine(initiatingMachine, generation)) return;
      resetForm();
      await loadMappings(initiatingMachine);
    } catch (err) {
      if (!isCurrentMachine(initiatingMachine, generation)) return;
      error = err instanceof Error ? err.message : m.worktree_failed_save();
    } finally {
      if (isCurrentMachine(initiatingMachine, generation)) saving = false;
    }
  }

  function requestDelete(mapping: DbProjectRule) {
    confirmation = { kind: "delete", mapping };
  }

  async function removeMapping(mapping: DbProjectRule) {
    const initiatingMachine = machine;
    const generation = machineGeneration;
    saving = true;
    error = "";
    applyMessage = "";
    try {
      await callGenerated(() =>
        SettingsService.deleteApiV1SettingsWorktreeMappingsId({
          id: String(mapping.id),
        }),
      );
      onMutated?.();
      if (!isCurrentMachine(initiatingMachine, generation)) return;
      if (editingId === mapping.id) resetForm();
      await loadMappings(initiatingMachine);
    } catch (err) {
      if (!isCurrentMachine(initiatingMachine, generation)) return;
      error = err instanceof Error ? err.message : m.worktree_failed_delete();
    } finally {
      if (isCurrentMachine(initiatingMachine, generation)) saving = false;
    }
  }

  async function confirmChange() {
    const pending = confirmation;
    confirmation = null;
    if (!pending) return;
    if (pending.kind === "delete") {
      await removeMapping(pending.mapping);
    } else {
      await saveMapping(pending.input, pending.id);
    }
  }

  async function applyMappings() {
    const initiatingMachine = machine;
    const generation = machineGeneration;
    applying = true;
    error = "";
    applyMessage = "";
    try {
      const res = await callGenerated(() =>
        SettingsService.postApiV1SettingsWorktreeMappingsApply({
          requestBody: { machine: initiatingMachine },
        }),
      ) as ApplyWorktreeMappingsResponse;
      onMutated?.();
      if (!isCurrentMachine(initiatingMachine, generation)) return;
      applyMessage = m.worktree_apply_result({
        updated: res.updated_sessions,
        matched: res.matched_sessions,
      });
    } catch (err) {
      if (!isCurrentMachine(initiatingMachine, generation)) return;
      error = err instanceof Error ? err.message : m.worktree_failed_apply();
    } finally {
      if (isCurrentMachine(initiatingMachine, generation)) applying = false;
    }
  }

  function derivedLayoutLabel(mapping: DbProjectRule): string {
    return mapping.layout === repoDotWorktreesLayout
      ? m.worktree_layout_repo_dot_worktrees({ repo: "repo", branch: "branch" })
      : m.worktree_layout_explicit({});
  }

  function fmtUpdated(ts: string): string {
    if (!ts) return "—";
    const d = new Date(ts);
    if (Number.isNaN(d.getTime())) return "—";
    return formatDateTime(d, { dateStyle: "medium" });
  }
</script>

{#snippet confirmationActions()}
  <Button
    label={m.worktree_cancel()}
    tone="neutral"
    surface="outline"
    disabled={saving}
    onclick={() => (confirmation = null)}
  />
  <Button
    label={confirmation?.kind === "delete"
      ? m.worktree_delete_confirm_action()
      : m.worktree_disable_confirm_action()}
    tone="danger"
    surface="solid"
    disabled={saving}
    onclick={confirmChange}
  />
{/snippet}

<section class="rules-view">
  <div class="heading-row">
    <h3>{m.worktree_title()}</h3>
    <p class="description">{m.worktree_description()}</p>
  </div>

  {#if loading && machine === ""}
    <div class="muted">{m.worktree_loading()}</div>
  {:else if error && machine === ""}
    <div class="error-text">{error}</div>
  {:else}
    <div class="machine-row">
      <span class="label">{m.worktree_machine()}</span>
      <Typeahead
        options={machineOptions}
        value={machine}
        fallbackLabel={machine || localMachine}
        placeholder={m.worktree_select_machine()}
        title={m.worktree_select_machine()}
        emptyLabel={m.worktree_no_machines()}
        onselect={selectMachine}
      />
    </div>

    <div class="rules-list" aria-busy={loading}>
      {#if loading}
        <div class="muted">{m.worktree_loading()}</div>
      {:else if mappings.length === 0}
        <div class="empty">{m.worktree_no_mappings()}</div>
      {:else}
        <div class="table-scroll">
          <table class="table">
            <thead>
              <tr>
                <TableHeaderCell label={m.data_rules_col_prefix()} />
                <TableHeaderCell label={m.data_rules_col_project()} />
                <TableHeaderCell label={m.data_rules_col_original()} />
                <TableHeaderCell label={m.data_rules_col_enabled()} />
                <TableHeaderCell label={m.data_rules_col_updated()} />
                <TableHeaderCell label={m.data_rules_col_governed()} numeric />
                {#if !readOnly}
                  <TableHeaderCell label={m.data_rules_col_actions()} />
                {/if}
              </tr>
            </thead>
            <tbody>
              <!-- Mirror-served rules all carry id 0, and two source archives
                   can each publish a rule for the same (machine, path_prefix)
                   pair (see internal/db.GovernedRuleKey and
                   internal/postgres.ListProjectRules), so the key uses the
                   documented replication identity (source_archive_id,
                   path_prefix) instead of id. That pair is unique on SQLite
                   too, since a single archive enforces UNIQUE(machine,
                   path_prefix). -->
              {#each mappings as mapping (`${mapping.source_archive_id}:${mapping.path_prefix}`)}
                <tr class="rule-row" class:disabled={!mapping.enabled}>
                  <td class="col-prefix" title={mapping.path_prefix}>{mapping.path_prefix}</td>
                  <td>
                    {#if mapping.project}
                      <button
                        class="link-btn"
                        title={m.data_rules_view_project({ project: mapping.project })}
                        onclick={() => onSelectProject(mapping.project)}
                      >{mapping.project}</button>
                    {:else}
                      <span class="derived-label">{derivedLayoutLabel(mapping)}</span>
                    {/if}
                  </td>
                  <td>{mapping.original_project || "—"}</td>
                  <td>{mapping.enabled ? m.worktree_on() : m.worktree_off()}</td>
                  <td>{fmtUpdated(mapping.updated_at)}</td>
                  <td class="col-num">{mapping.governed_sessions}</td>
                  {#if !readOnly}
                    <td>
                      <div class="row-actions">
                        <Button
                          size="sm"
                          label={m.worktree_edit()}
                          onclick={() => editMapping(mapping)}
                        />
                        <Button
                          size="sm"
                          tone="danger"
                          label={m.worktree_delete()}
                          onclick={() => requestDelete(mapping)}
                        />
                      </div>
                    </td>
                  {/if}
                </tr>
              {/each}
            </tbody>
          </table>
        </div>
      {/if}
    </div>

    {#if readOnly}
      <p class="warning" role="note">{m.data_rules_read_only()}</p>
    {:else}
      <div class="form-grid">
        <div class="field">
          <span>{m.worktree_layout()}</span>
          <div class="layout-options" role="group" aria-label={m.worktree_layout()}>
            <Button
              label={m.worktree_layout_explicit({})}
              tone={layout === explicitLayout ? "info" : "neutral"}
              surface={layout === explicitLayout ? "soft" : "outline"}
              onclick={() => (layout = explicitLayout)}
            />
            <Button
              label={m.worktree_layout_repo_dot_worktrees({ repo: "repo", branch: "branch" })}
              tone={layout === repoDotWorktreesLayout ? "info" : "neutral"}
              surface={layout === repoDotWorktreesLayout ? "soft" : "outline"}
              onclick={() => (layout = repoDotWorktreesLayout)}
            />
          </div>
        </div>
        <label class="field">
          <span>{isRepoDotWorktrees ? m.worktree_parent_directory() : m.worktree_path_prefix()}</span>
          <TextInput
            bind:value={pathPrefix}
            block
            ariaLabel={isRepoDotWorktrees ? m.worktree_parent_directory() : m.worktree_path_prefix()}
            placeholder={isRepoDotWorktrees ? "/Users/me" : "/Users/me/project.worktrees"}
          />
          {#if isRepoDotWorktrees}
            <div class="hint">{m.worktree_parent_directory_hint()}</div>
          {/if}
        </label>
        <label class="field">
          <span>{m.worktree_project()}</span>
          <TextInput
            bind:value={project}
            block
            ariaLabel={m.worktree_project()}
            placeholder="project-name"
            disabled={isRepoDotWorktrees}
          />
          <div class="hint">
            {isRepoDotWorktrees ? m.worktree_project_derived() : m.worktree_project_required()}
          </div>
        </label>
        <Checkbox bind:checked={enabled} label={m.worktree_enabled()} />
      </div>
    {/if}

    {#if error}
      <div class="error-text">{error}</div>
    {/if}
    {#if applyMessage}
      <div class="success-text">{applyMessage}</div>
    {/if}

    {#if !readOnly}
      <div class="button-row">
        <Button
          label={saving
            ? m.worktree_saving()
            : editingId == null
              ? m.worktree_add_mapping()
              : m.worktree_save_mapping()}
          tone="info"
          surface="solid"
          disabled={!canSave || saving}
          onclick={requestSave}
        />
        {#if editingId != null}
          <Button label={m.worktree_cancel()} disabled={saving} onclick={resetForm} />
        {/if}
        <Button
          label={applying ? m.worktree_applying() : m.worktree_apply_mappings()}
          disabled={applying || loading || mappings.length === 0}
          onclick={applyMappings}
        />
      </div>
    {/if}
  {/if}
</section>

{#if confirmation}
  <Modal
    title={confirmation.kind === "delete"
      ? m.worktree_delete_confirm_title()
      : m.worktree_disable_confirm_title()}
    closeLabel={m.worktree_confirmation_close()}
    tone="danger"
    width="440px"
    onclose={() => (confirmation = null)}
    footer={confirmationActions}
  >
    <p class="confirmation-copy">{m.worktree_change_warning()}</p>
  </Modal>
{/if}

<style>
  .rules-view {
    display: flex;
    flex-direction: column;
    gap: 12px;
    min-width: 0;
  }

  .heading-row {
    display: flex;
    flex-direction: column;
    gap: 4px;
  }

  h3 {
    margin: 0;
    font-size: 13px;
  }

  .description {
    margin: 0;
    color: var(--text-secondary);
    font-size: 12px;
  }

  .machine-row,
  .button-row,
  .row-actions {
    display: flex;
    align-items: center;
    gap: 8px;
  }

  .machine-row {
    justify-content: space-between;
    font-size: 12px;
  }

  .label,
  .field > span {
    color: var(--text-secondary);
    font-size: 12px;
    font-weight: 500;
  }

  .table-scroll {
    max-height: 480px;
    overflow-y: auto;
    border: 1px solid var(--border-muted);
    border-radius: var(--radius-sm);
  }

  .table {
    width: 100%;
    border-collapse: collapse;
    font-size: 11px;
  }

  /* The header cells come from kit-ui TableHeaderCell; the local table
     shell keeps them pinned while the body scrolls. */
  .table :global(thead th) {
    position: sticky;
    top: 0;
    z-index: 1;
  }

  tbody td {
    padding: 5px 8px;
    border-bottom: 1px solid var(--border-muted);
    color: var(--text-secondary);
    white-space: nowrap;
  }

  .rule-row:last-child td {
    border-bottom: none;
  }

  .rule-row.disabled {
    opacity: 0.65;
  }

  .col-prefix {
    max-width: 260px;
    overflow: hidden;
    text-overflow: ellipsis;
    font-family: var(--font-mono, monospace);
  }

  .col-num {
    text-align: right;
    font-family: var(--font-mono);
  }

  .derived-label,
  .hint,
  .muted,
  .empty {
    color: var(--text-muted);
    font-size: 11px;
  }

  .link-btn {
    padding: 0;
    border: none;
    background: none;
    color: var(--accent-blue);
    font: inherit;
    font-size: 11px;
    cursor: pointer;
  }

  .form-grid {
    display: grid;
    grid-template-columns: repeat(3, minmax(0, 1fr));
    gap: var(--space-5);
  }

  .field {
    display: flex;
    flex-direction: column;
    gap: var(--space-3);
    min-width: 0;
  }

  /* Stacked: the long repo_dot_worktrees label does not fit two-up inside a
     one-third-width form column. */
  .layout-options {
    display: flex;
    flex-direction: column;
    align-items: stretch;
    gap: 4px;
  }

  .warning {
    margin: 0;
    color: var(--accent-orange);
    font-size: 12px;
  }

  .error-text,
  .success-text {
    font-size: 12px;
  }

  .error-text {
    color: var(--accent-red);
  }

  .success-text {
    color: var(--accent-green);
  }

  .confirmation-copy {
    margin: 0;
    color: var(--text-secondary);
    font-size: 13px;
    line-height: 1.5;
  }

  @media (max-width: 760px) {
    .form-grid {
      grid-template-columns: 1fr;
    }
  }
</style>
