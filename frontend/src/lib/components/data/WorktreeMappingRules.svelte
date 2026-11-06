<script lang="ts">
  import {
    Button,
    Modal,
    SegmentedControl,
    TableHeaderCell,
    TableSort,
    TextInput,
    Toggle,
    Typeahead,
  } from "@kenn-io/kit-ui";
  import { onDestroy } from "svelte";
  import {
    DataService,
    SettingsService,
    type DbProjectRule,
    type WorktreeMappingRequest,
  } from "../../api/generated/index";
  import { isAbortError } from "../../api/runtime.js";
  import { formatDateTime, m } from "../../i18n/index.js";
  import { LatestRead } from "../../utils/latest-read.js";
  import { sessions } from "../../stores/sessions.svelte.js";
  import { LockIcon } from "../../icons.js";
  import StateNotice from "../shared/StateNotice.svelte";

  interface Props {
    readOnly?: boolean;
    machine?: string;
    /** Monotonic host signal that server-backed rules may have changed. */
    refreshVersion?: number;
    /**
     * When provided, machine selection is delegated to the host, which
     * remounts this component keyed on the machine; the remount resets local
     * state and performs the load. When omitted (standalone use), the
     * component switches machines locally.
     */
    onMachineChange?: (machine: string) => void;
    onSelectProject?: (label: string) => void;
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
    refreshVersion = 0,
    onMachineChange = undefined,
    onSelectProject = undefined,
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
  // Apply failures show beside the Apply action above the list; save and
  // delete failures keep showing under the form.
  let applyError = $state("");
  let editingId: number | null = $state(null);
  let editingWasEnabled = $state(false);
  let pathPrefix = $state("");
  let layout = $state(explicitLayout);
  let project = $state("");
  let enabled = $state(true);
  let confirmation: Confirmation | null = $state(null);
  const sort = new TableSort<DbProjectRule>({
    prefix: row => row.path_prefix,
    project: row => row.project || derivedLayoutLabel(row),
    original: row => row.original_project || null,
    enabled: row => row.enabled ? m.worktree_on() : m.worktree_off(),
    updated: { value: row => new Date(row.updated_at), firstDirection: "desc" },
    governed: { value: row => row.governed_sessions, firstDirection: "desc" },
  });
  const uid = $props.id();
  const formTitleId = `${uid}-form-title`;
  const mappingsRead = new LatestRead();
  let machineGeneration = 0;
  let disposed = false;
  let observedRefreshVersion = 0;
  let refreshVersionInitialized = false;

  const machineOptions = $derived(
    machines.map((name) => ({
      name,
      label: sessions.machineLabel(name),
      displayLabel: sessions.machineLabel(name),
      meta: sessions.machineLabel(name) !== name ? name : undefined,
    })),
  );
  const layoutOptions = $derived([
    { value: explicitLayout, label: m.worktree_layout_explicit({}) },
    {
      value: repoDotWorktreesLayout,
      label: m.worktree_layout_repo_dot_worktrees({ repo: "repo", branch: "branch" }),
    },
  ]);
  const isRepoDotWorktrees = $derived(layout === repoDotWorktreesLayout);
  const canSave = $derived(
    pathPrefix.trim() !== "" &&
      (layout === repoDotWorktreesLayout || project.trim() !== ""),
  );

  $effect(() => {
    void loadMappings(requestedMachine || undefined);
  });

  $effect(() => {
    if (!refreshVersionInitialized) {
      observedRefreshVersion = refreshVersion;
      refreshVersionInitialized = true;
      return;
    }
    if (refreshVersion === observedRefreshVersion) return;
    observedRefreshVersion = refreshVersion;
    void loadMappings(machine || requestedMachine || undefined);
  });

  async function loadMappings(requestedMachine?: string) {
    if (disposed) return;
    const signal = mappingsRead.begin();
    loading = true;
    error = "";
    try {
      const res = await DataService.getApiV1DataProjectRules({
            machine: requestedMachine || undefined,
          }, { signal });
      if (!mappingsRead.isCurrent(signal)) return;
      localMachine = res.local_machine;
      machine = res.machine;
      machines = Array.from(new Set([res.machine, ...(res.machines ?? [])]))
        .filter(Boolean);
      mappings = res.rules ?? [];
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
    applyError = "";
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
    applyError = "";
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
    applyError = "";
    try {
      if (id == null) {
        await SettingsService.postApiV1SettingsWorktreeMappings(input);
      } else {
        await SettingsService.putApiV1SettingsWorktreeMappingsById({ id: String(id) }, input);
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
    applyError = "";
    try {
      await SettingsService.deleteApiV1SettingsWorktreeMappingsById({
          id: String(mapping.id),
        });
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
    applyError = "";
    try {
      const res = await SettingsService.postApiV1SettingsWorktreeMappingsApply({ machine: initiatingMachine });
      onMutated?.();
      if (!isCurrentMachine(initiatingMachine, generation)) return;
      applyMessage = m.worktree_apply_result({
        updated: res.updated_sessions,
        matched: res.matched_sessions,
      });
    } catch (err) {
      if (!isCurrentMachine(initiatingMachine, generation)) return;
      applyError = err instanceof Error ? err.message : m.worktree_failed_apply();
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
    <!-- Applying acts on every rule listed for the machine, so it sits with
         the machine picker above the list rather than with the form. -->
    <div class="list-toolbar">
      <div class="machine-field">
        <span class="label">{m.worktree_machine()}</span>
        <Typeahead
          options={machineOptions}
          value={machine}
          fallbackLabel={sessions.machineLabel(machine || localMachine)}
          placeholder={m.worktree_select_machine()}
          title={m.worktree_select_machine()}
          emptyLabel={m.worktree_no_machines()}
          onselect={selectMachine}
        />
      </div>
      {#if !readOnly}
        <Button
          label={applying ? m.worktree_applying() : m.worktree_apply_mappings()}
          disabled={applying || loading || mappings.length === 0}
          onclick={applyMappings}
        />
      {/if}
    </div>
    {#if applyError}
      <div class="error-text">{applyError}</div>
    {/if}
    {#if applyMessage}
      <div class="success-text">{applyMessage}</div>
    {/if}

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
                <TableHeaderCell label={m.data_rules_col_prefix()} {sort} column="prefix" />
                <TableHeaderCell label={m.data_rules_col_project()} {sort} column="project" />
                <TableHeaderCell label={m.data_rules_col_original()} {sort} column="original" />
                <TableHeaderCell label={m.data_rules_col_enabled()} {sort} column="enabled" />
                <TableHeaderCell label={m.data_rules_col_updated()} {sort} column="updated" />
                <TableHeaderCell label={m.data_rules_col_governed()} {sort} column="governed" numeric />
                {#if !readOnly}
                  <TableHeaderCell><span class="kit-sr-only">{m.data_rules_col_actions()}</span></TableHeaderCell>
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
              {#each sort.sort(mappings) as mapping (`${mapping.source_archive_id}:${mapping.path_prefix}`)}
                <tr class="rule-row" class:disabled={!mapping.enabled}>
                  <td class="col-prefix" title={mapping.path_prefix}>{mapping.path_prefix}</td>
                  <td>
                    {#if mapping.project && onSelectProject}
                      <button
                        class="link-btn"
                        title={m.data_rules_view_project({ project: mapping.project })}
                        onclick={() => onSelectProject?.(mapping.project)}
                      >{mapping.project}</button>
                    {:else if mapping.project}
                      <span>{mapping.project}</span>
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
      <StateNotice role="note" description={m.data_rules_read_only()}>
        {#snippet icon()}
          <LockIcon size="16" strokeWidth="2" />
        {/snippet}
      </StateNotice>
    {:else}
      <section class="mapping-form" aria-labelledby={formTitleId}>
        <h4 id={formTitleId}>
          {editingId == null ? m.worktree_form_new_title() : m.worktree_form_edit_title()}
        </h4>
        <div class="field">
          <span>{m.worktree_layout()}</span>
          <SegmentedControl
            options={layoutOptions}
            value={layout}
            ariaLabel={m.worktree_layout()}
            onchange={(value) => (layout = value)}
          />
        </div>
        <div class="field-row">
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
        </div>
        <Toggle bind:checked={enabled} label={m.worktree_enabled()} />
        <div class="form-actions">
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
        </div>
      </section>
    {/if}

    {#if error}
      <div class="error-text">{error}</div>
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

  /* Section heading under the page title. */
  h3 {
    margin: 0;
    color: var(--text-primary);
    font-size: 14px;
    font-weight: 600;
  }

  .description {
    margin: 0;
    color: var(--text-secondary);
    font-size: 13px;
  }

  .form-actions,
  .row-actions {
    display: flex;
    align-items: center;
    gap: 8px;
  }

  /* Machine picker on the left, the list-wide Apply action on the right,
     both on the picker's control line. */
  .list-toolbar {
    display: flex;
    flex-wrap: wrap;
    align-items: flex-end;
    justify-content: space-between;
    gap: var(--space-4);
  }

  /* Label above the control, as in the Recall scope fields. The machine is
     an opaque ID, so it reads in monospace and truncates. */
  .machine-field {
    display: flex;
    flex-direction: column;
    align-items: flex-start;
    flex: 0 1 320px;
    min-width: 0;
    gap: var(--space-3);
    --typeahead-min-width: 240px;
    --typeahead-max-width: 320px;
  }

  .machine-field :global(.kit-typeahead) {
    width: 100%;
  }

  .machine-field :global(.kit-typeahead__value-text) {
    font-family: var(--font-mono);
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
  .muted {
    color: var(--text-muted);
    font-size: 11px;
  }

  .empty {
    color: var(--text-secondary);
    font-size: 13px;
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

  /* One framed form under the list: layout, then the two path fields side
     by side, the switch, and the actions, all on one left edge. */
  .mapping-form {
    display: flex;
    flex-direction: column;
    align-items: flex-start;
    gap: var(--space-6);
    padding: var(--space-6);
    border: 1px solid var(--border-muted);
    border-radius: var(--radius-sm);
    background: var(--bg-surface);
  }

  .mapping-form h4 {
    margin: 0;
    color: var(--text-primary);
    font-size: 13px;
    font-weight: 600;
  }

  .field-row {
    display: grid;
    grid-template-columns: repeat(2, minmax(0, 1fr));
    gap: var(--space-6);
    align-self: stretch;
  }

  .field {
    display: flex;
    flex-direction: column;
    gap: var(--space-3);
    min-width: 0;
  }

  .mapping-form :global(.kit-toggle__label) {
    color: var(--text-secondary);
    font-size: 12px;
    font-weight: 500;
  }

  /* Disabled secondary actions (Apply with no rules, Cancel while saving)
     use app.css's disabled ink and edge, as in Settings, instead of fading
     the whole button with opacity. */
  .rules-view :global(.kit-button:disabled:not(.kit-button--solid.kit-button--info)) {
    opacity: 1;
    background: transparent;
    border-color: var(--disabled-edge);
    box-shadow: none;
    color: var(--disabled-ink);
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
    .field-row {
      grid-template-columns: 1fr;
    }
  }
</style>
