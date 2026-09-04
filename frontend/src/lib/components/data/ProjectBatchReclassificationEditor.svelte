<script lang="ts">
  import { Button, Chip } from "@kenn-io/kit-ui";
  import { onDestroy, onMount } from "svelte";
  import {
    DataService,
    SettingsService,
    type DbProjectInventoryRow,
    type DbWorktreeReclassificationCandidate,
    type DbWorktreeReclassificationPreview,
  } from "../../api/generated/index";
  import type { ProjectInfo } from "../../api/types/core.js";
  import { callGenerated, isAbortError } from "../../api/runtime.js";
  import { m } from "../../i18n/index.js";
  import { LatestRead } from "../../utils/latest-read.js";
  import ProjectTypeahead from "../layout/ProjectTypeahead.svelte";
  import { displayProjectLabel } from "./project-label.js";

  interface Props {
    rows: DbProjectInventoryRow[];
    projects: ProjectInfo[];
    readOnly?: boolean;
    onRefresh: (target: string) => Promise<boolean>;
    onComplete: (target: string, count: number) => void;
    onOpenRules?: (machine: string) => void;
    onCandidateCount?: (count: number) => void;
  }

  interface BatchCandidate {
    sourceKey: string;
    sourceLabel: string;
    candidate: DbWorktreeReclassificationCandidate;
  }

  interface CandidatePreview {
    entry: BatchCandidate;
    preview: DbWorktreeReclassificationPreview;
  }

  let {
    rows,
    projects,
    readOnly = false,
    onRefresh,
    onComplete,
    onOpenRules = undefined,
    onCandidateCount = undefined,
  }: Props = $props();

  let candidates = $state<BatchCandidate[]>([]);
  let candidatesLoading = $state(true);
  let candidatesError = $state("");
  let targetProject = $state("");
  let previews = $state<CandidatePreview[]>([]);
  let previewLoading = $state(false);
  let previewError = $state("");
  let applying = $state(false);
  let applyError = $state("");
  let applied = $state(false);
  let refreshing = $state(false);
  let savedCount = $state(0);
  let previewTimer: ReturnType<typeof setTimeout> | undefined;
  let disposed = false;
  const candidatesRead = new LatestRead();
  const previewRead = new LatestRead();

  const usableCandidates = $derived(candidates.filter((entry) => entry.candidate.available));
  const matchedSessions = $derived(
    previews.reduce((sum, item) => sum + item.preview.matched_sessions, 0),
  );
  const changingSessions = $derived(
    previews.reduce((sum, item) => sum + item.preview.updated_sessions, 0),
  );
  const canApply = $derived(
    !readOnly &&
      !applied &&
      !applying &&
      !previewLoading &&
      previews.length === usableCandidates.length &&
      previews.length > 0 &&
      previews.every((item) => item.preview.mapping_token),
  );

  onMount(() => void loadCandidates());
  onDestroy(() => {
    disposed = true;
    if (previewTimer !== undefined) clearTimeout(previewTimer);
    candidatesRead.cancel();
    previewRead.cancel();
  });

  async function loadCandidates() {
    const signal = candidatesRead.begin();
    candidatesLoading = true;
    candidatesError = "";
    try {
      const results = await Promise.all(
        rows.map(async (row) => ({
          row,
          response: await callGenerated(
            () => DataService.getApiV1DataProjectReclassificationCandidates({
              projectLabel: row.label,
              projectKey: row.project_key,
            }),
            signal,
          ),
        })),
      );
      if (!candidatesRead.isCurrent(signal)) return;
      candidates = results.flatMap(({ row, response }) =>
        ((response.candidates ?? []) as DbWorktreeReclassificationCandidate[]).map((candidate) => ({
          sourceKey: row.project_key,
          sourceLabel: row.label,
          candidate,
        })),
      );
      onCandidateCount?.(candidates.length);
    } catch (error) {
      if (isAbortError(error) || !candidatesRead.isCurrent(signal)) return;
      candidatesError = error instanceof Error
        ? error.message
        : m.data_reclassify_candidates_failed();
    } finally {
      if (candidatesRead.finish(signal)) candidatesLoading = false;
    }
  }

  function draft(entry: BatchCandidate) {
    return {
      machine: entry.candidate.machine,
      path_prefix: entry.candidate.suggested_prefix,
      project: targetProject.trim(),
      original_project: entry.sourceLabel,
      layout: "explicit",
      enabled: true,
    };
  }

  function evidenceLabel(kind: string): string {
    switch (kind) {
      case "snapshot":
        return m.data_reclassify_evidence_snapshot();
      case "aggregate":
        return m.data_reclassify_evidence_aggregate();
      case "fallback":
        return m.data_reclassify_evidence_exact_cwd();
      default:
        return m.data_reclassify_evidence_suggestion();
    }
  }

  function selectTarget(value: string) {
    if (readOnly) return;
    targetProject = value.trim();
    clearPreview();
    schedulePreview();
  }

  function editTargetQuery(value: string) {
    if (value === "") return;
    clearPreview();
  }

  function clearPreview() {
    previewRead.cancel();
    previews = [];
    previewLoading = false;
    previewError = "";
    if (previewTimer !== undefined) clearTimeout(previewTimer);
    previewTimer = undefined;
  }

  function schedulePreview() {
    if (!targetProject.trim() || usableCandidates.length === 0) return;
    previewTimer = setTimeout(() => void loadPreviews(), 300);
  }

  async function loadPreviews() {
    previewTimer = undefined;
    const signal = previewRead.begin();
    previewLoading = true;
    previewError = "";
    try {
      const results = await Promise.all(
        usableCandidates.map(async (entry) => ({
          entry,
          preview: await callGenerated(
            () => SettingsService.postApiV1SettingsWorktreeMappingsPreview({
              requestBody: draft(entry),
            }),
            signal,
          ),
        })),
      );
      if (!previewRead.isCurrent(signal)) return;
      previews = results;
    } catch (error) {
      if (isAbortError(error) || !previewRead.isCurrent(signal)) return;
      previewError = error instanceof Error
        ? error.message
        : m.data_reclassify_preview_failed();
    } finally {
      if (previewRead.finish(signal)) previewLoading = false;
    }
  }

  async function applyAll() {
    if (!canApply) return;
    applying = true;
    applyError = "";
    savedCount = 0;
    const target = previews[0]?.preview.normalized_project || targetProject.trim();
    try {
      for (const entry of usableCandidates) {
        const requestBody = draft(entry);
        const current = await callGenerated(() =>
          SettingsService.postApiV1SettingsWorktreeMappingsPreview({ requestBody }),
        );
        await callGenerated(() =>
          SettingsService.postApiV1SettingsWorktreeMappingsReclassify({
            requestBody: { ...requestBody, mapping_token: current.mapping_token },
          }),
        );
        savedCount += 1;
      }
      applied = true;
      refreshing = true;
      const refreshed = await onRefresh(target);
      if (disposed) return;
      refreshing = false;
      if (refreshed) onComplete(target, savedCount);
    } catch (error) {
      if (disposed) return;
      applyError = error instanceof Error ? error.message : m.data_reclassify_apply_failed();
    } finally {
      if (!disposed) {
        applying = false;
        refreshing = false;
      }
    }
  }

  function cancel() {
    targetProject = "";
    clearPreview();
  }
</script>

<div class="editor">
  <section class="suggestions">
    <div class="section-heading">
      <div>
        <h4>{m.data_batch_folders_heading()}</h4>
        <p>{m.data_batch_folders_intro()}</p>
      </div>
    </div>

    {#if candidatesLoading}
      <p class="muted">{m.data_reclassify_candidates_loading()}</p>
    {:else if candidatesError}
      <p class="error-text">{candidatesError}</p>
    {:else if candidates.length === 0}
      <p class="muted">{m.data_reclassify_no_candidates()}</p>
    {:else}
      <div class="folder-list">
        {#each candidates as entry (`${entry.sourceKey}:${entry.candidate.id}`)}
          <article class="folder-row" class:unavailable={!entry.candidate.available}>
            <div class="folder-source">{displayProjectLabel(entry.sourceLabel)}</div>
            <div class="folder-path" title={entry.candidate.suggested_prefix}>
              {entry.candidate.suggested_prefix || m.data_reclassify_path_unavailable()}
            </div>
            <div class="folder-meta">
              <span>{entry.candidate.machine}</span>
              <span>{m.data_reclassify_candidate_sessions({ count: entry.candidate.contributing_sessions })}</span>
              <Chip size="xs" tone={entry.candidate.available ? "muted" : "warning"} uppercase={false}>
                {entry.candidate.available
                  ? evidenceLabel(entry.candidate.evidence_kind)
                  : m.data_reclassify_evidence_unavailable()}
              </Chip>
            </div>
          </article>
        {/each}
      </div>
    {/if}
  </section>

  <section class="composer">
    <div class="composer-heading">
      <div>
        <h4>{m.data_batch_correction_heading()}</h4>
        <p>{m.data_batch_correction_intro()}</p>
      </div>
      <Chip size="xs" tone="workspace" uppercase={false}>
        {m.data_batch_folder_count({ count: usableCandidates.length })}
      </Chip>
    </div>

    {#if readOnly}
      <p class="warning" role="note">{m.data_reclassify_read_only()}</p>
    {:else}
      <div class="target-field">
        <span>{m.data_batch_target_project()}</span>
        <ProjectTypeahead
          {projects}
          value={targetProject}
          onselect={selectTarget}
          onquery={editTargetQuery}
          includeAll={false}
          allowCustom={true}
          customLabel={m.data_reclassify_use_custom_project({ query: "{query}" })}
          placeholder={m.data_reclassify_target_project()}
          title={m.data_reclassify_target_project()}
        />
      </div>

      {#if previewLoading}
        <p class="muted">{m.data_reclassify_previewing()}</p>
      {:else if previews.length > 0}
        <div class="impact" aria-live="polite">
          <span>{m.data_batch_folder_count({ count: previews.length })}</span>
          <span>{m.data_reclassify_sessions_matched({ count: matchedSessions })}</span>
          <span>{m.data_reclassify_sessions_changing({ count: changingSessions })}</span>
          <span>{m.data_reclassify_projects_affected({ count: rows.length })}</span>
        </div>
      {/if}

      {#if previewError}<p class="error-text">{previewError}</p>{/if}
      {#if applyError}
        <p class="error-text">
          {applyError}
          {#if savedCount > 0}{m.data_batch_partial_save({ saved: savedCount, count: usableCandidates.length })}{/if}
        </p>
      {/if}
      {#if applied && !refreshing}
        <p class="warning" role="status">{m.data_reclassify_applied_refresh_failed()}</p>
      {/if}

      {#if onOpenRules && usableCandidates[0]}
        <p class="rules-note">
          {m.data_reclassify_managed_in_rules()}
          <button class="link-btn" onclick={() => onOpenRules?.(usableCandidates[0]!.candidate.machine)}>
            {m.data_reclassify_open_rules()}
          </button>
        </p>
      {/if}

      <div class="action-row">
        <Button label={m.data_reclassify_cancel()} disabled={applying} onclick={cancel} />
        <Button
          label={applying || refreshing
            ? m.data_batch_saving()
            : m.data_batch_save({ count: usableCandidates.length })}
          disabled={!canApply || applying || refreshing}
          tone="info"
          surface="solid"
          onclick={applyAll}
        />
      </div>
    {/if}
  </section>
</div>

<style>
  .editor {
    display: flex;
    min-height: 0;
    flex: 1;
    flex-direction: column;
    overflow-y: auto;
  }

  .suggestions,
  .composer {
    display: flex;
    flex-direction: column;
    gap: var(--space-5);
    padding: 12px 14px;
  }

  .suggestions { border-bottom: 1px solid var(--border-muted); }
  .composer { background: var(--bg-inset); }

  .section-heading,
  .composer-heading,
  .folder-meta,
  .impact,
  .action-row {
    display: flex;
    align-items: center;
  }

  .section-heading,
  .composer-heading {
    justify-content: space-between;
    gap: 12px;
  }

  h4 { margin: 0; color: var(--text-primary); font-size: 12px; }
  .section-heading p,
  .composer-heading p {
    margin: 3px 0 0;
    color: var(--text-muted);
    font-size: 10px;
  }

  .folder-list {
    max-height: min(390px, 45vh);
    overflow-y: auto;
    border: 1px solid var(--border-muted);
    border-radius: var(--radius-sm);
  }

  .folder-row {
    display: grid;
    gap: var(--space-3);
    padding: 10px 11px;
    border-bottom: 1px solid var(--border-muted);
  }
  .folder-row:last-child { border-bottom: 0; }
  .folder-row.unavailable { opacity: 0.65; }

  .folder-source {
    color: var(--text-primary);
    font-size: 11px;
    font-weight: 650;
  }

  .folder-path {
    color: var(--text-secondary);
    font-family: var(--font-mono);
    font-size: 10px;
    line-height: 1.45;
    overflow-wrap: anywhere;
  }

  .folder-meta {
    flex-wrap: wrap;
    gap: 8px;
    color: var(--text-muted);
    font-size: 10px;
  }

  .target-field {
    display: grid;
    grid-template-columns: minmax(110px, 0.45fr) minmax(0, 1fr);
    align-items: center;
    gap: var(--space-5);
    font-size: 12px;
    --typeahead-min-width: 100%;
  }

  .impact {
    flex-wrap: wrap;
    gap: 12px;
    color: var(--text-secondary);
    font-size: 10px;
  }

  .action-row { justify-content: flex-end; gap: 8px; }
  .muted,
  .rules-note { color: var(--text-muted); font-size: 11px; }
  .warning { color: var(--accent-orange); font-size: 11px; }
  .error-text { color: var(--accent-red); font-size: 11px; }
  .muted, .rules-note, .warning, .error-text { margin: 0; }

  .link-btn {
    margin-left: 4px;
    padding: 0;
    border: 0;
    background: none;
    color: var(--accent-blue);
    font: inherit;
    cursor: pointer;
  }

  @media (max-width: 760px) {
    .target-field { grid-template-columns: 1fr; }
  }
</style>
