<script lang="ts">
  import { Button, TextInput, Typeahead, type TypeaheadOption } from "@kenn-io/kit-ui";
  import { onDestroy, onMount } from "svelte";
  import {
    DataService,
    SettingsService,
    type DbWorktreeReclassificationCandidate,
    type DbWorktreeReclassificationPreview,
  } from "../../api/generated/index";
  import { callGenerated, isAbortError } from "../../api/runtime.js";
  import { m } from "../../i18n/index.js";
  import type { ProjectInfo } from "../../api/types/core.js";
  import { LatestRead } from "../../utils/latest-read.js";
  import ProjectTypeahead from "../layout/ProjectTypeahead.svelte";

  // Candidates load once on mount; there is no reactive reload when the
  // project identity changes. Hosts MUST remount this component whenever
  // projectLabel or projectKey change, e.g. via a {#key} block keyed on the
  // project identity (DataPage does this through ProjectWorkspace).
  interface Props {
    projectLabel: string;
    projectKey: string;
    projects: ProjectInfo[];
    readOnly?: boolean;
    onRefresh: (appliedTarget: string) => Promise<boolean>;
    onComplete: () => void;
    onOpenRules?: (machine: string) => void;
  }

  let {
    projectLabel,
    projectKey,
    projects,
    readOnly = false,
    onRefresh,
    onComplete,
    onOpenRules = undefined,
  }: Props = $props();

  let candidates = $state<DbWorktreeReclassificationCandidate[]>([]);
  let candidatesLoading = $state(true);
  let candidatesError = $state("");
  let selectedCandidateId = $state("");
  let machine = $state("");
  let pathPrefix = $state("");
  let targetProject = $state("");
  let preview = $state<DbWorktreeReclassificationPreview | null>(null);
  let previewLoading = $state(false);
  let previewError = $state("");
  let conflict = $state(false);
  let applying = $state(false);
  let applied = $state(false);
  let appliedTarget = $state("");
  let refreshing = $state(false);
  let applyError = $state("");
  let previewTimer: ReturnType<typeof setTimeout> | undefined;
  let disposed = false;
  const candidatesRead = new LatestRead();
  const previewRead = new LatestRead();

  const selectedCandidate = $derived(
    candidates.find((candidate) => candidate.id === selectedCandidateId),
  );
  const candidateOptions = $derived.by((): TypeaheadOption[] =>
    candidates.map((candidate) => ({
      name: candidate.id,
      label: `${candidate.machine} · ${candidate.suggested_prefix || m.data_reclassify_path_unavailable()} · ${m.data_reclassify_candidate_sessions({ count: candidate.contributing_sessions })}`,
      displayLabel: candidateDisplayLabel(candidate),
    })),
  );

  // The collapsed control shows only this label, so machine name alone cannot
  // distinguish two worktrees on the same machine.
  function candidateDisplayLabel(
    candidate: DbWorktreeReclassificationCandidate,
  ): string {
    const tail = candidate.suggested_prefix
      .split("/")
      .filter(Boolean)
      .slice(-2)
      .join("/");
    return tail ? `${candidate.machine} · ${tail}` : candidate.machine;
  }
  const canApply = $derived(
    !applied &&
      !applying &&
      !previewLoading &&
      !!preview?.mapping_token &&
      preview.matched_sessions > 0,
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
      const response = await callGenerated(
        () => DataService.getApiV1DataProjectReclassificationCandidates({
          projectLabel,
          projectKey,
        }),
        signal,
      );
      if (!candidatesRead.isCurrent(signal)) return;
      candidates = (response.candidates ?? []) as DbWorktreeReclassificationCandidate[];
      if (candidates.length === 1) selectCandidate(candidates[0]!.id);
    } catch (error) {
      if (isAbortError(error) || !candidatesRead.isCurrent(signal)) return;
      candidatesError = error instanceof Error
        ? error.message
        : m.data_reclassify_candidates_failed();
    } finally {
      if (candidatesRead.finish(signal)) candidatesLoading = false;
    }
  }

  function clearAcceptedPreview() {
    previewRead.cancel();
    previewLoading = false;
    preview = null;
    previewError = "";
    conflict = false;
    if (previewTimer !== undefined) clearTimeout(previewTimer);
    previewTimer = undefined;
  }

  function selectCandidate(id: string) {
    clearAcceptedPreview();
    selectedCandidateId = id;
    const candidate = candidates.find((item) => item.id === id);
    machine = candidate?.machine ?? "";
    pathPrefix = candidate?.suggested_prefix ?? "";
    schedulePreview();
  }

  function editPrefix(value: string) {
    if (readOnly) return;
    pathPrefix = value;
    clearAcceptedPreview();
    schedulePreview();
  }

  function selectTarget(value: string) {
    if (readOnly) return;
    targetProject = value.trim();
    clearAcceptedPreview();
    schedulePreview();
  }

  function editTargetQuery(value: string) {
    // Typeahead reports an empty query whenever it opens or closes, and a real
    // browser can report the close reset more than once during focus handoff.
    // That does not change the selected target. Non-empty edits still make an
    // accepted preview stale immediately; selecting a value clears and
    // reschedules the preview in selectTarget above.
    if (value === "") return;
    clearAcceptedPreview();
  }

  function draft() {
    return {
      machine,
      path_prefix: pathPrefix.trim(),
      project: targetProject.trim(),
      original_project: projectLabel,
      layout: "explicit",
      enabled: true,
    };
  }

  function schedulePreview(delay = 300) {
    if (readOnly) return;
    if (previewTimer !== undefined) clearTimeout(previewTimer);
    previewTimer = undefined;
    if (!selectedCandidate?.available || !machine || !pathPrefix.trim() || !targetProject.trim()) {
      return;
    }
    previewTimer = setTimeout(() => void loadPreview(), delay);
  }

  async function loadPreview() {
    previewTimer = undefined;
    const requestBody = draft();
    if (!requestBody.machine || !requestBody.path_prefix || !requestBody.project) return;
    const signal = previewRead.begin();
    previewLoading = true;
    previewError = "";
    try {
      const result = await callGenerated(
        () => SettingsService.postApiV1SettingsWorktreeMappingsPreview({ requestBody }),
        signal,
      );
      if (!previewRead.isCurrent(signal)) return;
      preview = result;
    } catch (error) {
      if (isAbortError(error) || !previewRead.isCurrent(signal)) return;
      preview = null;
      previewError = error instanceof Error
        ? error.message
        : m.data_reclassify_preview_failed();
    } finally {
      if (previewRead.finish(signal)) previewLoading = false;
    }
  }

  async function apply() {
    if (readOnly) return;
    const token = preview?.mapping_token;
    if (!canApply || !token) return;
    applying = true;
    applyError = "";
    // Capture the request body and applied target before awaiting: edits made
    // while the request is in flight mutate preview/draft state and must not
    // change what was actually applied.
    const requestBody = { ...draft(), mapping_token: token };
    const target = preview?.normalized_project || requestBody.project;
    try {
      await callGenerated(() =>
        SettingsService.postApiV1SettingsWorktreeMappingsReclassify({ requestBody }),
      );
      if (disposed) {
        // The mutation committed even though the editor unmounted mid-flight;
        // fire the store-level refresh so the inventory does not go stale,
        // and skip all component state (including onComplete).
        void onRefresh(target);
        return;
      }
      appliedTarget = target;
      applied = true;
      // Keep dismissal blocked through the initial refresh as well as the
      // mutation request. The refresh has already started after commit, and
      // the editor remains present until it can show either completion or the
      // refresh-only retry state.
      await refreshInventory();
    } catch (error) {
      if (disposed) return;
      if (typeof error === "object" && error !== null && "status" in error && error.status === 409) {
        clearAcceptedPreview();
        conflict = true;
        await loadPreview();
      } else {
        applyError = error instanceof Error
          ? error.message
          : m.data_reclassify_apply_failed();
      }
    } finally {
      if (!disposed) applying = false;
    }
  }

  async function retryRefresh() {
    if (!applied || refreshing) return;
    await refreshInventory();
  }

  async function refreshInventory() {
    refreshing = true;
    let refreshed = false;
    try {
      refreshed = await onRefresh(appliedTarget);
    } catch {
      // The mutation has committed; a refresh error must stay on the
      // refresh-only path rather than being misreported as an apply failure.
    }
    if (disposed) return;
    refreshing = false;
    if (refreshed) onComplete();
  }
</script>

<div class="editor">
  <!-- Display-only fallback for empty labels (absolute-path/URL-scheme
       projects); the raw label still feeds candidates, preview, and apply
       requests. -->
  <p class="original">{m.data_reclassify_original({ project: projectLabel || m.shared_unknown() })}</p>

  {#if candidatesLoading}
    <p class="muted">{m.data_reclassify_candidates_loading()}</p>
  {:else if candidatesError}
    <p class="error-text">{candidatesError}</p>
  {:else if candidates.length === 0}
    <p class="muted">{m.data_reclassify_no_candidates()}</p>
  {:else}
    {#if candidates.length > 1}
      <label class="field">
        <span>{m.data_reclassify_choose_worktree()}</span>
        <Typeahead
          options={candidateOptions}
          value={selectedCandidateId}
          fallbackLabel={m.data_reclassify_choose_worktree()}
          placeholder={m.data_reclassify_choose_worktree()}
          title={m.data_reclassify_choose_worktree()}
          emptyLabel={m.data_reclassify_no_candidates()}
          onselect={selectCandidate}
        />
      </label>
    {/if}

    {#if selectedCandidate}
      <div class="candidate-summary">
        <strong>{selectedCandidate.machine}</strong>
        <span>{m.data_reclassify_candidate_sessions({ count: selectedCandidate.contributing_sessions })}</span>
      </div>
      {#if !selectedCandidate.available}
        <p class="warning" role="alert">
          {m.data_reclassify_cwd_unavailable()}
        </p>
      {:else if !readOnly}
        <label class="field">
          <span>{m.data_reclassify_path_prefix()}</span>
          <TextInput
            value={pathPrefix}
            block
            ariaLabel={m.data_reclassify_path_prefix()}
            oninput={editPrefix}
          />
        </label>
        <label class="field target-field">
          <span>{m.data_reclassify_target_project()}</span>
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
        </label>
      {/if}
    {/if}
  {/if}

  {#if !readOnly}
    {#if previewLoading}
      <p class="muted">{m.data_reclassify_previewing()}</p>
    {:else if preview}
      <strong class="impact-title">{m.data_reclassify_full_archive_impact()}</strong>
      <div class="impact" aria-live="polite">
        <span>{m.data_reclassify_sessions_matched({ count: preview.matched_sessions })}</span>
        <span>{m.data_reclassify_sessions_changing({ count: preview.updated_sessions })}</span>
        <span>{m.data_reclassify_projects_affected({ count: preview.distinct_projects })}</span>
      </div>
      {#if preview.normalized_project && preview.normalized_project !== targetProject.trim()}
        <p class="normalized">
          {m.data_reclassify_normalized_target({ project: preview.normalized_project })}
        </p>
      {/if}
      {#if preview.distinct_projects > 1}
        <div class="warning" role="alert">
          {m.data_reclassify_multiple_projects()}
          {#if preview.project_samples?.length}
            <ul>
              {#each preview.project_samples as sample}
                <li>{sample.project} ({m.data_reclassify_project_sample_sessions({ count: sample.count })})</li>
              {/each}
            </ul>
          {/if}
        </div>
      {:else if preview.matched_sessions === 0}
        <p class="error-text">{m.data_reclassify_zero_matches()}</p>
      {/if}
    {/if}

    {#if conflict}
      <p class="warning">{m.data_reclassify_conflict()}</p>
    {/if}
    {#if previewError}<p class="error-text">{previewError}</p>{/if}
    {#if applyError}<p class="error-text">{applyError}</p>{/if}
    {#if applied && !refreshing}
      <p class="warning" role="status">{m.data_reclassify_applied_refresh_failed()}</p>
    {/if}
  {:else}
    <p class="warning" role="note">{m.data_reclassify_read_only()}</p>
  {/if}

  {#if onOpenRules}
    <p class="rules-note">
      {m.data_reclassify_managed_in_rules()}
      <button class="link-btn" onclick={() => onOpenRules?.(machine)}>
        {m.data_reclassify_open_rules()}
      </button>
    </p>
  {/if}

  {#if !readOnly}
    <div class="action-row">
      {#if applied}
        <Button
          label={refreshing
            ? m.data_reclassify_refreshing()
            : m.data_reclassify_retry_refresh()}
          disabled={refreshing}
          tone="info"
          surface="solid"
          onclick={retryRefresh}
        />
      {:else}
        <Button
          label={applying
            ? m.data_reclassify_applying()
            : m.data_reclassify_apply()}
          disabled={!canApply}
          tone="info"
          surface="solid"
          onclick={apply}
        />
      {/if}
    </div>
  {/if}
</div>

<style>
  .editor,
  .field {
    display: flex;
    flex-direction: column;
  }

  .editor { gap: 12px; }
  .field { gap: var(--space-2); font-size: 12px; }
  .impact-title { font-size: 12px; }
  .target-field { --typeahead-min-width: 100%; }
  .original { margin: 0; color: var(--text-secondary); }
  .muted, .rules-note { color: var(--text-muted); font-size: 11px; }
  .candidate-summary, .impact { display: flex; gap: 12px; font-size: 12px; }
  .candidate-summary { justify-content: space-between; }
  .impact { flex-wrap: wrap; padding: 8px; background: var(--bg-inset); border-radius: var(--radius-sm); }
  .normalized { color: var(--text-secondary); font-size: 12px; }
  .warning { color: var(--accent-orange); font-size: 12px; }
  .error-text { color: var(--accent-red); font-size: 12px; }
  .warning, .error-text, .muted, .normalized, .rules-note { margin: 0; }
  .warning ul { margin: 6px 0 0; padding-left: 18px; }
  .action-row { display: flex; justify-content: flex-end; gap: 8px; }
  .link-btn {
    margin-left: 4px;
    padding: 0;
    border: none;
    background: none;
    color: var(--accent-blue);
    font: inherit;
    font-size: 11px;
    cursor: pointer;
  }
</style>
