<script lang="ts">
  import { Card, Checkbox } from "@kenn-io/kit-ui";
  import { m } from "../../i18n/index.js";
  import SettingsSection from "./SettingsSection.svelte";
  import {
    SettingsService,
    type ApplyWorktreeMappingsResponse,
    type DbWorktreeProjectMapping,
    type WorktreeMappingRequest,
  } from "../../api/generated/index";
  import { callGenerated, isAbortError } from "../../api/runtime.js";
  import { LatestRead } from "../../utils/latest-read.js";
  import { onDestroy } from "svelte";

  interface WorktreeMappingsResponse {
    machine: string;
    mappings: DbWorktreeProjectMapping[];
  }

  interface Props {
    readOnly?: boolean;
  }

  const explicitLayout = "explicit";
  const repoDotWorktreesLayout = "repo_dot_worktrees";

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
  let layout = $state(explicitLayout);
  let project = $state("");
  let enabled = $state(true);
  const mappingsRead = new LatestRead();
  let disposed = false;

  $effect(() => {
    if (readOnly) {
      mappingsRead.cancel();
      loading = false;
      return;
    }
    loadMappings();
  });

  async function loadMappings() {
    if (disposed) return;
    const signal = mappingsRead.begin();
    loading = true;
    error = "";
    try {
      const res =
        await callGenerated(() =>
          SettingsService.getApiV1SettingsWorktreeMappings(),
          signal,
        ) as unknown as WorktreeMappingsResponse;
      if (!mappingsRead.isCurrent(signal)) return;
      machine = res.machine;
      mappings = res.mappings;
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
    pathPrefix = "";
    layout = explicitLayout;
    project = "";
    enabled = true;
  }

  function editMapping(mapping: DbWorktreeProjectMapping) {
    editingId = mapping.id;
    pathPrefix = mapping.path_prefix;
    layout = mapping.layout || explicitLayout;
    project = mapping.project;
    enabled = mapping.enabled;
    applyMessage = "";
    error = "";
  }

  async function saveMapping() {
    const input = {
      path_prefix: pathPrefix.trim(),
      layout,
      project: project.trim(),
      enabled,
    } satisfies WorktreeMappingRequest;
    if (!input.path_prefix) return;
    if (layout !== repoDotWorktreesLayout && !input.project) return;

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

  let isRepoDotWorktrees = $derived(layout === repoDotWorktreesLayout);
  let canSave = $derived(
    pathPrefix.trim() !== "" &&
      (layout === repoDotWorktreesLayout || project.trim() !== ""),
  );
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
            <Card
              level="inset"
              padding="none"
              class={!mapping.enabled ? "mapping-row disabled" : "mapping-row"}
            >
              <div class="mapping-main">
                <div class="mapping-project">
                  {mapping.project || (mapping.layout === repoDotWorktreesLayout ? m.worktree_layout_repo_dot_worktrees({ repo: "repo", branch: "branch" }) : m.worktree_layout_explicit({}))}
                </div>
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
          </Card>
        {/each}
      {/if}
    </div>

    <div class="form-grid">
      <label class="field">
        <span>{m.worktree_layout()}</span>
        <div class="layout-options" role="group" aria-label={m.worktree_layout()}>
          <button
            type="button"
            class:active={layout === explicitLayout}
            onclick={() => (layout = explicitLayout)}
          >
            {m.worktree_layout_explicit({})}
          </button>
          <button
            type="button"
            class:active={layout === repoDotWorktreesLayout}
            onclick={() => (layout = repoDotWorktreesLayout)}
          >
            {m.worktree_layout_repo_dot_worktrees({
              repo: "repo",
              branch: "branch",
            })}
          </button>
        </div>
      </label>
      <label class="field">
        <span>{isRepoDotWorktrees ? m.worktree_parent_directory() : m.worktree_path_prefix()}</span>
        <input
          type="text"
          bind:value={pathPrefix}
          placeholder={isRepoDotWorktrees ? "/Users/me" : "/Users/me/project.worktrees"}
        />
        {#if isRepoDotWorktrees}
          <div class="hint">{m.worktree_parent_directory_hint()}</div>
        {/if}
      </label>
      <label class="field">
        <span>{m.worktree_project()}</span>
        <input
          type="text"
          bind:value={project}
          placeholder="project-name"
          disabled={isRepoDotWorktrees}
        />
        <div class="hint">
          {isRepoDotWorktrees ? m.worktree_project_derived() : m.worktree_project_required()}
        </div>
      </label>
      <Checkbox bind:checked={enabled} label={m.worktree_enabled()} />
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

  .mapping-list :global(.mapping-row) {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: 12px;
    min-height: 48px;
    padding: 8px 10px;
  }

  .mapping-list :global(.mapping-row > .kit-card__body) {
    display: contents;
  }

  .mapping-list :global(.mapping-row.disabled) {
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
    grid-template-columns: repeat(3, minmax(0, 1fr));
    gap: var(--space-5);
  }

  .field {
    display: flex;
    flex-direction: column;
    gap: var(--space-3);
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

  .layout-options {
    display: grid;
    grid-template-columns: repeat(2, minmax(0, 1fr));
    gap: 4px;
  }

  .layout-options button {
    min-height: 30px;
    padding: 4px 8px;
    border: 1px solid var(--border-muted);
    border-radius: var(--radius-sm);
    background: var(--bg-inset);
    color: var(--text-secondary);
    font-size: 12px;
    cursor: pointer;
  }

  .layout-options button.active {
    border-color: var(--accent-blue);
    color: var(--text-primary);
  }

  .field input:focus {
    outline: none;
    border-color: var(--accent-blue);
  }

  .hint {
    color: var(--text-muted);
    font-size: 11px;
    line-height: 1.3;
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
    .mapping-list :global(.mapping-row),
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
