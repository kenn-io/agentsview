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
  import type { DbProjectInfo as ProjectInfo } from "../../api/generated/index.js";
  import { isAbortError } from "../../api/runtime.js";
  import { m } from "../../i18n/index.js";
  import { data } from "../../stores/data.svelte.js";
  import { LatestRead } from "../../utils/latest-read.js";
  import ProjectTypeahead from "../layout/ProjectTypeahead.svelte";
  import CandidateEvidence from "./CandidateEvidence.svelte";
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
  let reviewing = $state(false);
  let conflict = $state(false);
  let previewTimer: ReturnType<typeof setTimeout> | undefined;
  let disposed = false;
  const candidatesRead = new LatestRead();
  const previewRead = new LatestRead();

  const usableCandidates = $derived(candidates.filter((entry) => entry.candidate.available));
  const saveLabel = $derived(refreshing
    ? m.data_reclassify_refreshing()
    : applying
    ? m.data_batch_saving_progress({ saved: savedCount, count: usableCandidates.length })
    : reviewing
    ? m.data_reclassify_confirm_save()
    : m.data_batch_save({ count: usableCandidates.length }));
  const matchedSessions = $derived(
    new Set(previews.flatMap(({ preview }) => preview.matched_session_ids)).size,
  );
  const changingSessions = $derived(
    new Set(previews.flatMap(({ preview }) => preview.updated_session_ids)).size,
  );
  const affectedProjects = $derived(
    new Set(previews.flatMap(({ preview }) => preview.matched_projects)).size,
  );
  const reachesUnselectedProjects = $derived(
    previews.some(({ preview }) => preview.matched_project_keys.some(
      (key) => !rows.some((row) => row.project_key === key),
    )),
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
          response: await DataService.getApiV1DataProjectReclassificationCandidates({
              project_label: row.label,
              project_key: row.project_key,
              ...data.dateParams,
            }, { signal }),
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
      case "worktree":
        return m.data_candidate_worktree_prefix();
      case "parent":
        return m.data_candidate_common_parent();
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
    if (readOnly || applying || applied) return;
    targetProject = value.trim();
    clearPreview();
    schedulePreview();
  }

  function editTargetQuery(value: string) {
    if (value === "") return;
    clearPreview();
  }

  function clearPreview() {
    reviewing = false;
    conflict = false;
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
          preview: await SettingsService.postApiV1SettingsWorktreeMappingsPreview(draft(entry), { signal }),
        })),
      );
      if (!previewRead.isCurrent(signal)) return;
      previews = results;
    } catch (error) {
      if (isAbortError(error) || !previewRead.isCurrent(signal)) return;
      previews = [];
      previewError = error instanceof Error
        ? error.message
        : m.data_reclassify_preview_failed();
    } finally {
      if (previewRead.finish(signal)) previewLoading = false;
    }
  }

  async function refreshChangedImpact() {
    conflict = true;
    await loadPreviews();
    reviewing = true;
  }

  function sameIDs(actual: string[], expected: string[]) {
    const ids = new Set(expected);
    return actual.length === ids.size && actual.every((id) => ids.has(id));
  }

  async function applyAll() {
    if (!canApply) return;
    if ((reachesUnselectedProjects || conflict) && !reviewing) {
      reviewing = true;
      return;
    }
    applying = true;
    conflict = false;
    applyError = "";
    savedCount = 0;
    const target = previews[0]?.preview.normalized_project || targetProject.trim();
    try {
      const requests = previews.map(({ entry, preview }) => ({ requestBody: draft(entry), accepted: preview }));
      const savedRuleStates = new Map<string, string>();
      const savedSessionIDs = new Set<string>();
      for (const { requestBody, accepted } of requests) {
        const current = await SettingsService.postApiV1SettingsWorktreeMappingsPreview(requestBody);
        const savedState = savedRuleStates.get(requestBody.machine);
        // Before our first write on a machine, require the exact reviewed token.
        // Later writes may only account for our own rule edits and sessions
        // already moved to this batch's destination, including overlapping rules.
        const unchanged = savedState === undefined
          ? current.mapping_token === accepted.mapping_token
          : current.mapping_set_token === savedState &&
            current.normalized_project === accepted.normalized_project &&
            sameIDs(current.matched_session_ids, accepted.matched_session_ids) &&
            sameIDs(current.updated_session_ids, accepted.updated_session_ids.filter((id) => !savedSessionIDs.has(id))) &&
            current.matched_projects.every((project) =>
              project === target || accepted.matched_projects.includes(project));
        if (!unchanged) {
          await refreshChangedImpact();
          return;
        }
        const result = await SettingsService.postApiV1SettingsWorktreeMappingsReclassify({
            ...requestBody, mapping_token: current.mapping_token,
          });
        savedRuleStates.set(requestBody.machine, result.result.mapping_set_token);
        for (const id of current.updated_session_ids) savedSessionIDs.add(id);
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
      if (typeof error === "object" && error !== null && "status" in error && error.status === 409) {
        await refreshChangedImpact();
      } else {
        applyError = error instanceof Error ? error.message : m.data_reclassify_apply_failed();
      }
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
      <span class="destination-label">{m.data_batch_target_project()}</span>
      <div class="destination-actions">
        <div class="target-field">
          <ProjectTypeahead
            {projects}
            disabled={applying || refreshing || applied}
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

        <div class="action-row">
          {#if reviewing && !applying && !applied}
            <Button label={m.data_reclassify_back()} onclick={() => reviewing = false} />
          {/if}
          <Button
            class="bulk-save"
            label={saveLabel}
            ariaLabel={saveLabel}
            disabled={!canApply || applying || refreshing}
            tone="info"
            surface="solid"
            onclick={applyAll}
          >
            {#if applying || refreshing}
              <span
                class="save-progress"
                role="progressbar"
                aria-label={m.data_batch_saving()}
                aria-valuemin="0"
                aria-valuemax={usableCandidates.length}
                aria-valuenow={savedCount}
                style:width={`${usableCandidates.length ? savedCount / usableCandidates.length * 100 : 0}%`}
              ></span>
            {/if}
          </Button>
        </div>
      </div>

      {#if previewLoading}
        <p class="muted">{m.data_reclassify_previewing()}</p>
      {:else if previews.length > 0}
        <div class="impact" aria-live="polite">
          <span>{m.data_batch_folder_count({ count: previews.length })}</span>
          <span>{m.data_reclassify_sessions_matched({ count: matchedSessions })}</span>
          <span>{m.data_reclassify_sessions_changing({ count: changingSessions })}</span>
          <span>{m.data_reclassify_projects_affected({ count: affectedProjects })}</span>
        </div>
      {/if}

      {#if previewError}<p class="error-text">{previewError}</p>{/if}
      {#if reviewing && reachesUnselectedProjects}
        <p class="warning" role="alert">{m.data_batch_outside_selection()}</p>
      {/if}
      {#if conflict}
        <p class="warning" role="alert">
          {m.data_reclassify_conflict()}
          {#if savedCount > 0}{m.data_batch_partial_save({ saved: savedCount, count: usableCandidates.length })}{/if}
        </p>
      {/if}
      {#if applyError}
        <p class="error-text">
          {applyError}
          {#if savedCount > 0}{m.data_batch_partial_save({ saved: savedCount, count: usableCandidates.length })}{/if}
        </p>
      {/if}
      {#if applied && !refreshing}
        <p class="warning" role="status">{m.data_reclassify_applied_refresh_failed()}</p>
      {/if}
    {/if}
  </section>
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
            {#if rows.length > 1}
              <div class="folder-source">{displayProjectLabel(entry.sourceLabel)}</div>
            {/if}
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
            <CandidateEvidence candidate={entry.candidate} />
          </article>
        {/each}
      </div>
    {/if}
    <div class="secondary-actions">
      {#if onOpenRules && usableCandidates[0]}
        <p class="rules-note">
          {m.data_reclassify_managed_in_rules()}
          <button class="link-btn" onclick={() => onOpenRules?.(usableCandidates[0]!.candidate.machine)}>
            {m.data_reclassify_open_rules()}
          </button>
        </p>
      {/if}
      {#if !readOnly}
        <Button label={m.data_reclassify_cancel()} disabled={applying || refreshing || applied} onclick={cancel} />
      {/if}
    </div>
  </section>
</div>

<style>
  .action-row :global(.bulk-save) { position: relative; overflow: hidden; }
  .save-progress {
    position: absolute;
    inset-inline-start: 0;
    bottom: 0;
    height: 3px;
    background: currentColor;
    pointer-events: none;
  }
  .editor {
    display: flex;
    min-height: 0;
    flex: 1;
    flex-direction: column;
    overflow: hidden;
  }

  .suggestions,
  .composer {
    display: flex;
    flex-direction: column;
    gap: var(--space-5);
    padding: 12px 14px;
  }

  .suggestions { min-height: 0; overflow-y: auto; }
  .composer { flex: none; background: var(--bg-inset); border-bottom: 1px solid var(--border-muted); }

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
    flex: none;
    min-height: 0;
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
    display: flex;
    flex: 1;
    min-width: 0;
    flex-direction: column;
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

  .destination-label { font-size: var(--font-size-sm); }
  .secondary-actions { display: flex; align-items: center; justify-content: space-between; gap: var(--space-4); }
  .destination-actions { display: flex; align-items: flex-end; flex-wrap: wrap; gap: var(--space-5); }
  .action-row { flex: none; justify-content: flex-end; gap: var(--space-4); }
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

</style>
