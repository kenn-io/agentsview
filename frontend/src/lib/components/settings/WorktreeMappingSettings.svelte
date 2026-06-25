<script lang="ts">
  import { m } from "../../i18n/index.js";
  import SettingsSection from "./SettingsSection.svelte";
  import {
    SettingsService,
    type ApplyWorktreeMappingsResponse,
    type DbWorktreeProjectMapping,
    type WorktreeMappingRequest,
  } from "../../api/generated/index";
  import { callGenerated } from "../../api/runtime.js";

  interface WorktreeMappingsResponse {
    machine: string;
    mappings: DbWorktreeProjectMapping[];
  }

  interface Props {
    readOnly?: boolean;
  }

  let { readOnly = false }: Props = $props();

  let machine = $state("");
  let mappings: DbWorktreeProjectMapping[] = $state([]);
  let loading = $state(true);
  let saving = $state(false);
  let applying = $state(false);
  let error = $state("");
  let applyMessage = $state("");
  let editingId: number | null = $state(null);
  let pathPrefix = $state("");
  let project = $state("");
  let enabled = $state(true);

  $effect(() => {
    if (readOnly) {
      loading = false;
      return;
    }
    loadMappings();
  });

  async function loadMappings() {
    loading = true;
    error = "";
    try {
      const res =
        await callGenerated(() =>
          SettingsService.getApiV1SettingsWorktreeMappings(),
        ) as unknown as WorktreeMappingsResponse;
      machine = res.machine;
      mappings = res.mappings;
    } catch (err) {
      error = err instanceof Error ? err.message : m.worktree_failed_load();
    } finally {
      loading = false;
    }
  }

  function resetForm() {
    editingId = null;
    pathPrefix = "";
    project = "";
    enabled = true;
  }

  function editMapping(mapping: DbWorktreeProjectMapping) {
    editingId = mapping.id;
    pathPrefix = mapping.path_prefix;
    project = mapping.project;
    enabled = mapping.enabled;
    applyMessage = "";
    error = "";
  }

  async function saveMapping() {
    const input = {
      path_prefix: pathPrefix.trim(),
      project: project.trim(),
      enabled,
    } satisfies WorktreeMappingRequest;
    if (!input.path_prefix || !input.project) return;

    saving = true;
    error = "";
    applyMessage = "";
    try {
      if (editingId == null) {
        await callGenerated(() =>
          SettingsService.postApiV1SettingsWorktreeMappings({
            requestBody: input,
          }),
        );
      } else {
        await callGenerated(() =>
          SettingsService.putApiV1SettingsWorktreeMappingsId({
            id: String(editingId),
            requestBody: input,
          }),
        );
      }
      resetForm();
      await loadMappings();
    } catch (err) {
      error = err instanceof Error ? err.message : m.worktree_failed_save();
    } finally {
      saving = false;
    }
  }

  async function removeMapping(id: number) {
    saving = true;
    error = "";
    applyMessage = "";
    try {
      await callGenerated(() =>
        SettingsService.deleteApiV1SettingsWorktreeMappingsId({
          id: String(id),
        }),
      );
      if (editingId === id) resetForm();
      await loadMappings();
    } catch (err) {
      error = err instanceof Error ? err.message : m.worktree_failed_delete();
    } finally {
      saving = false;
    }
  }

  async function applyMappings() {
    applying = true;
    error = "";
    applyMessage = "";
    try {
      const res =
        await callGenerated(() =>
          SettingsService.postApiV1SettingsWorktreeMappingsApply(),
        ) as ApplyWorktreeMappingsResponse;
      applyMessage = m.worktree_apply_result({ updated: res.updated_sessions, matched: res.matched_sessions });
    } catch (err) {
      error = err instanceof Error ? err.message : m.worktree_failed_apply();
    } finally {
      applying = false;
    }
  }

  let canSave = $derived(pathPrefix.trim() !== "" && project.trim() !== "");
</script>

<SettingsSection
  title={m.worktree_title()}
  description={m.worktree_description()}
>
  {#if readOnly}
    <div class="muted">{m.worktree_local_only()}</div>
  {:else if loading}
    <div class="muted">{m.worktree_loading()}</div>
  {:else if error && mappings.length === 0}
    <div class="error-text">{error}</div>
  {:else}
    <div class="machine-row">
      <span class="label">{m.worktree_machine()}</span>
      <code>{machine || "local"}</code>
    </div>

    <div class="mapping-list">
      {#if mappings.length === 0}
        <div class="empty">{m.worktree_no_mappings()}</div>
      {:else}
        {#each mappings as mapping (mapping.id)}
          <div class="mapping-row" class:disabled={!mapping.enabled}>
            <div class="mapping-main">
              <div class="mapping-project">{mapping.project}</div>
              <div class="mapping-path">{mapping.path_prefix}</div>
            </div>
            <div class="mapping-actions">
              <span class="status">{mapping.enabled ? m.worktree_on() : m.worktree_off()}</span>
              <button class="small-btn" onclick={() => editMapping(mapping)}>
                {m.worktree_edit()}
              </button>
              <button class="small-btn danger" onclick={() => removeMapping(mapping.id)}>
                {m.worktree_delete()}
              </button>
            </div>
          </div>
        {/each}
      {/if}
    </div>

    <div class="form-grid">
      <label class="field">
        <span>{m.worktree_path_prefix()}</span>
        <input
          type="text"
          bind:value={pathPrefix}
          placeholder="/Users/me/project.worktrees"
        />
      </label>
      <label class="field">
        <span>{m.worktree_project()}</span>
        <input type="text" bind:value={project} placeholder="project-name" />
      </label>
      <label class="enabled-toggle">
        <input type="checkbox" bind:checked={enabled} />
        {m.worktree_enabled()}
      </label>
    </div>

    {#if error}
      <div class="error-text">{error}</div>
    {/if}
    {#if applyMessage}
      <div class="success-text">{applyMessage}</div>
    {/if}

    <div class="button-row">
      <button
        class="primary-btn"
        disabled={!canSave || saving}
        onclick={saveMapping}
      >
        {saving ? m.worktree_saving() : editingId == null ? m.worktree_add_mapping() : m.worktree_save_mapping()}
      </button>
      {#if editingId != null}
        <button class="secondary-btn" onclick={resetForm}>{m.worktree_cancel()}</button>
      {/if}
      <button
        class="secondary-btn"
        disabled={applying || mappings.length === 0}
        onclick={applyMappings}
      >
        {applying ? m.worktree_applying() : m.worktree_apply_mappings()}
      </button>
    </div>
  {/if}
</SettingsSection>

<style>
  .machine-row,
  .button-row,
  .mapping-actions {
    display: flex;
    align-items: center;
    gap: 8px;
  }

  .machine-row {
    justify-content: space-between;
    font-size: 12px;
  }

  .label,
  .field span {
    color: var(--text-secondary);
    font-size: 12px;
    font-weight: 500;
  }

  code {
    font-family: var(--font-mono, monospace);
    font-size: 11px;
    color: var(--text-muted);
  }

  .mapping-list {
    display: flex;
    flex-direction: column;
    gap: 6px;
  }

  .mapping-row {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: 12px;
    min-height: 48px;
    padding: 8px 10px;
    border: 1px solid var(--border-muted);
    border-radius: var(--radius-sm);
    background: var(--bg-inset);
  }

  .mapping-row.disabled {
    opacity: 0.65;
  }

  .mapping-main {
    min-width: 0;
  }

  .mapping-project {
    color: var(--text-primary);
    font-size: 12px;
    font-weight: 600;
  }

  .mapping-path {
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
    color: var(--text-muted);
    font-family: var(--font-mono, monospace);
    font-size: 11px;
  }

  .status {
    color: var(--text-muted);
    font-size: 11px;
  }

  .form-grid {
    display: grid;
    grid-template-columns: 1fr 160px;
    gap: 10px;
  }

  .field {
    display: flex;
    flex-direction: column;
    gap: 5px;
    min-width: 0;
  }

  .field input {
    height: 30px;
    min-width: 0;
    padding: 0 10px;
    border: 1px solid var(--border-muted);
    border-radius: var(--radius-sm);
    background: var(--bg-inset);
    color: var(--text-primary);
    font-size: 12px;
  }

  .field input:focus {
    outline: none;
    border-color: var(--accent-blue);
  }

  .enabled-toggle {
    display: flex;
    align-items: center;
    gap: 7px;
    color: var(--text-secondary);
    font-size: 12px;
  }

  .small-btn,
  .primary-btn,
  .secondary-btn {
    height: 28px;
    padding: 0 10px;
    border: 1px solid var(--border-muted);
    border-radius: var(--radius-sm);
    background: var(--bg-surface);
    color: var(--text-secondary);
    font-size: 12px;
    font-weight: 500;
    cursor: pointer;
  }

  .small-btn {
    height: 24px;
    font-size: 11px;
  }

  .primary-btn {
    border-color: var(--accent-blue);
    background: var(--accent-blue);
    color: var(--accent-blue-foreground);
  }

  .danger {
    color: var(--color-danger, #d14);
  }

  button:disabled {
    cursor: default;
    opacity: 0.55;
  }

  .muted,
  .empty,
  .error-text,
  .success-text {
    font-size: 12px;
  }

  .muted,
  .empty {
    color: var(--text-muted);
  }

  .error-text {
    color: var(--color-danger, #d14);
  }

  .success-text {
    color: var(--accent-green, #16834a);
  }

  @media (max-width: 640px) {
    .mapping-row,
    .button-row {
      align-items: stretch;
      flex-direction: column;
    }

    .mapping-actions {
      justify-content: flex-end;
    }

    .form-grid {
      grid-template-columns: 1fr;
    }
  }
</style>
