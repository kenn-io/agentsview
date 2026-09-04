<script lang="ts">
  import { Button, Chip, TextInput } from "@kenn-io/kit-ui";
  import { onDestroy, onMount } from "svelte";
  import {
    DataService,
    SettingsService,
    type DbWorktreeReclassificationCandidate,
    type DbWorktreeReclassificationPreview,
    type DbWorktreeReclassificationSessionSample,
  } from "../../api/generated/index";
  import { callGenerated, isAbortError } from "../../api/runtime.js";
  import { m } from "../../i18n/index.js";
  import type { ProjectInfo } from "../../api/types/core.js";
  import { LatestRead } from "../../utils/latest-read.js";
  import ProjectTypeahead from "../layout/ProjectTypeahead.svelte";
  import ProjectSessionPreviewCarousel from "./ProjectSessionPreviewCarousel.svelte";
  import { ChevronDownIcon, ChevronRightIcon } from "../../icons.js";

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
    onComplete: (target: string) => void;
    onOpenRules?: (machine: string) => void;
    onCandidateCount?: (count: number) => void;
  }

  let {
    projectLabel,
    projectKey,
    projects,
    readOnly = false,
    onRefresh,
    onComplete,
    onOpenRules = undefined,
    onCandidateCount = undefined,
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
  let reviewing = $state(false);
  let suggestionsExpanded = $state(true);
  let previewTimer: ReturnType<typeof setTimeout> | undefined;
  let disposed = false;
  const candidatesRead = new LatestRead();
  const previewRead = new LatestRead();

  const selectedCandidate = $derived(
    candidates.find((candidate) => candidate.id === selectedCandidateId),
  );
  const canApply = $derived(
    !applied &&
      !applying &&
      !previewLoading &&
      !!preview?.mapping_token &&
      preview.matched_sessions > 0,
  );
  const requiresReview = $derived(
    !!preview && (preview.existing_mapping_id != null || preview.distinct_projects > 1),
  );
  const sessionSamples = $derived(
    (preview?.session_samples ?? []) as DbWorktreeReclassificationSessionSample[],
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
      onCandidateCount?.(candidates.length);
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
    reviewing = false;
    if (previewTimer !== undefined) clearTimeout(previewTimer);
    previewTimer = undefined;
  }

  function selectCandidate(id: string) {
    clearAcceptedPreview();
    selectedCandidateId = id;
    const candidate = candidates.find((item) => item.id === id);
    machine = candidate?.machine ?? "";
    pathPrefix = candidate?.suggested_prefix ?? "";
    targetProject = candidate?.available ? projectLabel : "";
    schedulePreview();
  }

  function cancelCorrection() {
    clearAcceptedPreview();
    selectedCandidateId = "";
    machine = "";
    pathPrefix = "";
    targetProject = "";
  }

  function reviewImpact() {
    if (canApply && requiresReview) reviewing = true;
  }

  function backToCorrection() {
    reviewing = false;
  }

  function evidenceLabel(kind: string): string {
    switch (kind) {
      case "snapshot":
        return m.data_reclassify_evidence_snapshot();
      case "aggregate":
        return m.data_reclassify_evidence_aggregate();
      case "fallback":
        return m.data_reclassify_evidence_exact_cwd();
      case "unavailable":
        return m.data_reclassify_evidence_unavailable();
      default:
        return m.data_reclassify_evidence_suggestion();
    }
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
    if (!canApply || !token || (requiresReview && !reviewing)) return;
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
    if (refreshed) onComplete(appliedTarget);
  }
</script>

<div class="editor">
  <section class="suggestions">
    <Button
      size="sm"
      class="suggestions-toggle"
      label={m.data_mapping_observed_folders()}
      ariaExpanded={suggestionsExpanded}
      onclick={() => (suggestionsExpanded = !suggestionsExpanded)}
    >
      {#snippet trailing()}
        {#if suggestionsExpanded}
          <ChevronDownIcon size="13" strokeWidth="2.2" aria-hidden="true" />
        {:else}
          <ChevronRightIcon size="13" strokeWidth="2.2" aria-hidden="true" />
        {/if}
      {/snippet}
    </Button>

    {#if suggestionsExpanded}
      <p class="suggestions-intro">{m.data_reclassify_suggestions_intro()}</p>
      {#if candidatesLoading}
        <p class="muted">{m.data_reclassify_candidates_loading()}</p>
      {:else if candidatesError}
        <p class="error-text">{candidatesError}</p>
      {:else if candidates.length === 0}
        <p class="muted">{m.data_reclassify_no_candidates()}</p>
      {:else}
        <div class="folder-list">
          {#each candidates as candidate (candidate.id)}
            <div class="folder-row" class:selected={candidate.id === selectedCandidateId}>
              <Button
                size="sm"
                class="folder-choice"
                ariaLabel={candidate.suggested_prefix || m.data_reclassify_path_unavailable()}
                title={candidate.suggested_prefix || m.data_reclassify_path_unavailable()}
                tone={candidate.id === selectedCandidateId ? "info" : "neutral"}
                surface={candidate.id === selectedCandidateId ? "soft" : "outline"}
                onclick={() => selectCandidate(candidate.id)}
              >
                {#snippet children()}
                  <span class="folder-path">
                    {candidate.suggested_prefix || m.data_reclassify_path_unavailable()}
                  </span>
                  <span class="folder-details">
                    <span>{candidate.machine}</span>
                    <span>{m.data_reclassify_candidate_sessions({ count: candidate.contributing_sessions })}</span>
                  </span>
                  <span class="folder-footer">
                    {#if candidate.available}
                      <span class="folder-action">{m.data_reclassify_use_for_project()}</span>
                    {/if}
                    <Chip size="xs" tone={candidate.available ? "muted" : "warning"} uppercase={false}>
                      {evidenceLabel(candidate.evidence_kind)}
                    </Chip>
                  </span>
                {/snippet}
              </Button>
            </div>
          {/each}
        </div>
      {/if}
    {/if}
  </section>

  {#if selectedCandidate}
    <section class="composer">
      {#if !selectedCandidate.available}
        <p class="warning" role="alert">{m.data_reclassify_cwd_unavailable()}</p>
      {:else if !readOnly}
        <div class="composer-heading">
          <h4>{m.data_mapping_definition()}</h4>
          <Chip size="xs" tone="workspace" uppercase={false}>{machine}</Chip>
        </div>
        <div class="mapping-row">
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
        </div>

        <div class="impact-slot" aria-live="polite">
          {#if previewLoading}
            <p class="muted">{m.data_reclassify_previewing()}</p>
          {:else if preview}
            <div class="impact">
              <span>{m.data_reclassify_sessions_matched({ count: preview.matched_sessions })}</span>
              <span>{m.data_reclassify_sessions_changing({ count: preview.updated_sessions })}</span>
              <span>{m.data_reclassify_projects_affected({ count: preview.distinct_projects })}</span>
            </div>
          {/if}
        </div>

        {#if preview}
          {#if preview.normalized_project && preview.normalized_project !== targetProject.trim()}
            <p class="normalized">{m.data_reclassify_normalized_target({ project: preview.normalized_project })}</p>
          {/if}
          {#if requiresReview}
            <div class="impact-warning" role="alert">
              <strong>{preview.existing_mapping_id != null
                ? m.data_reclassify_replaces_rule()
                : m.data_reclassify_multiple_projects()}</strong>
              {#if reviewing && preview.project_samples?.length}
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

        {#if conflict}<p class="warning">{m.data_reclassify_conflict()}</p>{/if}
        {#if previewError}<p class="error-text">{previewError}</p>{/if}
        {#if applyError}<p class="error-text">{applyError}</p>{/if}
        {#if applied && !refreshing}
          <p class="warning" role="status">{m.data_reclassify_applied_refresh_failed()}</p>
        {/if}

        {#if onOpenRules}
          <p class="rules-note">
            {m.data_reclassify_managed_in_rules()}
            <button class="link-btn" onclick={() => onOpenRules?.(machine)}>{m.data_reclassify_open_rules()}</button>
          </p>
        {/if}

        <div class="action-row">
          {#if applied}
            <Button
              label={refreshing ? m.data_reclassify_refreshing() : m.data_reclassify_retry_refresh()}
              disabled={refreshing}
              tone="info"
              surface="solid"
              onclick={retryRefresh}
            />
          {:else if reviewing}
            <Button label={m.data_reclassify_back()} onclick={backToCorrection} />
            <Button
              label={applying ? m.data_reclassify_applying() : m.data_reclassify_confirm_save()}
              disabled={!canApply}
              tone="info"
              surface="solid"
              onclick={apply}
            />
          {:else}
            <Button label={m.data_reclassify_cancel()} onclick={cancelCorrection} />
            <Button
              label={applying
                ? m.data_reclassify_applying()
                : requiresReview
                  ? m.data_reclassify_review_impact()
                  : m.data_reclassify_apply()}
              disabled={!canApply}
              tone="info"
              surface="solid"
              onclick={requiresReview ? reviewImpact : apply}
            />
          {/if}
        </div>

        {#if preview && sessionSamples.length > 0}
          {#key preview.mapping_token}
            <ProjectSessionPreviewCarousel samples={sessionSamples} />
          {/key}
        {/if}
      {:else}
        <p class="warning" role="note">{m.data_reclassify_read_only()}</p>
      {/if}
    </section>
  {:else if readOnly}
    <section class="composer"><p class="warning" role="note">{m.data_reclassify_read_only()}</p></section>
  {/if}
</div>

<style>
  .editor,
  .field {
    display: flex;
    flex-direction: column;
  }

  .editor { flex: 1; min-height: 0; overflow-y: auto; }
  .field { gap: var(--space-2); font-size: 12px; }
  .target-field { --typeahead-min-width: 100%; }
  .muted, .rules-note { color: var(--text-muted); font-size: 11px; }
  .suggestions {
    display: flex;
    flex-direction: column;
    gap: var(--space-4);
    padding: 10px 12px;
    border-bottom: 1px solid var(--border-muted);
  }
  .composer-heading {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: 12px;
  }
  h4 { margin: 0; color: var(--text-primary); font-size: 12px; }
  .suggestions :global(.suggestions-toggle.kit-button) {
    width: 100%;
    min-height: 28px;
    justify-content: space-between;
    padding: 0 2px;
    border: 0;
    border-radius: 0;
    background: transparent;
    color: var(--text-primary);
    font-size: 12px;
    font-weight: 650;
    transform: none;
  }
  .suggestions :global(.suggestions-toggle.kit-button:hover:not(:disabled)) {
    border-color: transparent;
    background: transparent;
    color: var(--text-primary);
  }
  .suggestions-intro { margin: 0; color: var(--text-muted); font-size: 10px; }
  .composer {
    display: flex;
    flex-direction: column;
    gap: var(--space-4);
    padding: 11px 12px;
    background: var(--bg-inset);
  }
  .folder-list {
    max-height: min(260px, 34vh);
    overflow-y: auto;
    border: 1px solid var(--border-muted);
    border-radius: var(--radius-sm);
  }
  .folder-row {
    min-height: 72px;
    padding: 4px;
    border-bottom: 1px solid var(--border-muted);
  }
  .folder-row:last-child {
    border-bottom: 0;
  }
  .folder-row.selected { background: var(--bg-inset); }
  .folder-row :global(.folder-choice) {
    width: 100%;
    min-width: 0;
    flex-direction: column;
    align-items: stretch;
    justify-content: flex-start;
    text-align: left;
    padding-inline: 8px;
    border-color: transparent;
    background: transparent;
    white-space: normal;
  }
  .folder-path {
    display: block;
    font-family: var(--font-mono);
    font-weight: 400;
    line-height: 1.45;
    overflow-wrap: anywhere;
    white-space: normal;
  }
  .folder-details {
    display: flex;
    gap: var(--space-4);
    margin-top: 2px;
    color: var(--text-muted);
    font-family: var(--font-sans);
    font-size: 10px;
  }
  .folder-action {
    color: var(--accent-blue);
    font-family: var(--font-sans);
    font-size: 10px;
    font-weight: 600;
  }
  .folder-footer {
    display: flex;
    min-height: 18px;
    align-items: center;
    justify-content: space-between;
    gap: 8px;
    margin-top: 3px;
  }
  .mapping-row {
    display: flex;
    flex-direction: column;
    align-items: stretch;
    gap: 8px;
  }
  .impact-slot {
    display: flex;
    min-height: 16px;
    align-items: center;
  }
  .impact { display: flex; flex-wrap: wrap; gap: var(--space-5); color: var(--text-secondary); font-size: 10px; }
  .normalized { color: var(--text-secondary); font-size: 12px; }
  .warning { color: var(--accent-orange); font-size: 12px; }
  .error-text { color: var(--accent-red); font-size: 12px; }
  .warning, .error-text, .muted, .normalized, .rules-note { margin: 0; }
  .impact-warning { padding: 8px; border: 1px solid color-mix(in srgb, var(--accent-orange) 35%, var(--border-muted)); border-radius: var(--radius-sm); background: color-mix(in srgb, var(--accent-orange) 7%, var(--bg-surface)); color: var(--text-secondary); font-size: 11px; line-height: 1.45; }
  .impact-warning ul { margin: 6px 0 0; padding-left: 18px; }
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
