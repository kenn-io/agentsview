<script lang="ts">
  import {
    Button,
    Card,
    EmptyState,
    Modal,
    SearchInput,
    SegmentedControl,
    Table,
    TableHeaderCell,
    Typeahead,
    type SegmentedControlOption,
    type TypeaheadOption,
  } from "@kenn-io/kit-ui";
  import {
    activateRecallExtractionGeneration,
    fetchRecallEntries,
    fetchRecallExtractionProgress,
    fetchRecallExtractionStatus,
    reviewRecallEntry,
    retireRecallExtractionGeneration,
  } from "../../api/recall.js";
  import type {
    RecallEntry,
    RecallEvidence,
    RecallExtractGeneration,
    RecallExtractProgress,
    RecallExtractProgressState,
    RecallExtractionStatus,
    RecallReviewAction,
  } from "../../api/types/recall.js";
  import { ApiError, isAbortError } from "../../api/runtime.js";
  import { formatDateTime, m } from "../../i18n/index.js";
  import { ChevronDownIcon, ChevronRightIcon } from "../../icons.js";
  import { router } from "../../stores/router.svelte.js";
  import { sessions } from "../../stores/sessions.svelte.js";
  import { ui } from "../../stores/ui.svelte.js";
  import { LatestRead } from "../../utils/latest-read.js";
  import RefreshControl from "../shared/RefreshControl.svelte";

  const ENTRY_TYPES = [
    "fact",
    "decision",
    "procedure",
    "debugging_method",
    "warning",
    "preference",
    "open_question",
  ];
  const REVIEW_STATES = [
    "human_reviewed",
    "human_rejected",
    "unreviewed_auto",
    "calibrated_auto",
    "eval_raw",
  ];

  type GenerationAction = {
    kind: "activate" | "retire";
    generation: RecallExtractGeneration;
  };

  let entries = $state<RecallEntry[]>([]);
  let nextCursor = $state("");
  let resultCap = $state(0);
  let status = $state<RecallExtractionStatus | null>(null);
  let entriesLoading = $state(true);
  let entriesFailed = $state(false);
  let statusLoading = $state(true);
  let statusFailed = $state(false);
  let entriesUpdatedAt = $state<number | null>(null);
  let statusUpdatedAt = $state<number | null>(null);
  let progress = $state<RecallExtractProgress[]>([]);
  let progressExpanded = $state(false);
  let progressState = $state<"" | RecallExtractProgressState>("");
  let progressNextCursor = $state("");
  let progressLoading = $state(false);
  let progressFailed = $state(false);
  let generationAction = $state<GenerationAction | null>(null);
  let generationActionLoading = $state(false);
  let generationActionError = $state("");
  let reviewingEntryIds = $state<string[]>([]);
  let reviewErrors = $state<Record<string, string>>({});
  let archiveEntry = $state<RecallEntry | null>(null);
  let search = $state("");
  let query = $state("");
  let project = $state("");
  let entryType = $state("");
  let generation = $state("");
  let reviewState = $state("");
  let expandedEntryIds = $state<string[]>([]);
  let searchTimer: ReturnType<typeof setTimeout> | undefined;
  const entriesRead = new LatestRead();
  const statusRead = new LatestRead();
  const progressRead = new LatestRead();
  const lastUpdatedAt = $derived(
    entriesUpdatedAt !== null && statusUpdatedAt !== null
      ? Math.min(entriesUpdatedAt, statusUpdatedAt)
      : null,
  );

  const projectOptions = $derived.by((): TypeaheadOption[] => [
    {
      name: "",
      label: m.shared_all_projects(),
      displayLabel: m.shared_all_projects(),
    },
    ...sessions.projects
      .filter((item) => item.name !== "")
      .map((item) => ({
        name: item.name,
        label: item.name,
        displayLabel: item.name,
        count: item.session_count,
      })),
  ]);
  const typeOptions = $derived.by((): TypeaheadOption[] => [
    {
      name: "",
      label: m.recall_page_all_types(),
      displayLabel: m.recall_page_all_types(),
    },
    ...ENTRY_TYPES.map((name) => ({
      name,
      label: name,
      displayLabel: name,
    })),
  ]);
  const generationOptions = $derived.by((): TypeaheadOption[] => [
    {
      name: "",
      label: m.recall_page_all_generations(),
      displayLabel: m.recall_page_all_generations(),
    },
    ...(status?.source_runs ?? [])
      .map((sourceRun) => ({
        name: sourceRun,
        label: sourceRun,
        displayLabel: sourceRun,
      })),
  ]);
  const reviewOptions = $derived.by((): TypeaheadOption[] => [
    {
      name: "",
      label: m.recall_page_all_review_states(),
      displayLabel: m.recall_page_all_review_states(),
    },
    ...REVIEW_STATES.map((name) => ({
      name,
      label: reviewStateLabel(name),
      displayLabel: reviewStateLabel(name),
    })),
  ]);
  const progressStateOptions = $derived<SegmentedControlOption[]>([
    { value: "", label: m.recall_page_progress_all() },
    {
      value: "pending",
      label: m.recall_page_progress_pending(),
    },
    {
      value: "partial",
      label: m.recall_page_progress_partial(),
    },
    {
      value: "failed",
      label: m.recall_page_progress_failed(),
      tone: "danger",
    },
  ]);

  async function loadEntries(cursor = "") {
    const signal = entriesRead.begin();
    const appending = cursor !== "";
    entriesLoading = true;
    entriesFailed = false;
    try {
      const page = await fetchRecallEntries({
        query: query || undefined,
        project: project || undefined,
        type: entryType || undefined,
        sourceRunId: generation || undefined,
        status: reviewState === "human_rejected" ? "archived" : undefined,
        reviewState: reviewState || undefined,
        cursor: cursor || undefined,
      }, signal);
      if (!entriesRead.isCurrent(signal)) return;
      entries = appending
        ? [...entries, ...page.entries]
        : page.entries;
      nextCursor = page.nextCursor ?? "";
      resultCap = page.resultCap ?? 0;
      entriesUpdatedAt = Date.now();
    } catch (error) {
      if (isAbortError(error) || !entriesRead.isCurrent(signal)) return;
      if (appending && error instanceof ApiError && error.status === 409) {
        await loadEntries();
        return;
      }
      if (!appending) {
        entries = [];
        nextCursor = "";
        resultCap = 0;
        entriesFailed = true;
      }
    } finally {
      if (entriesRead.finish(signal)) entriesLoading = false;
    }
  }

  async function loadStatus() {
    const signal = statusRead.begin();
    statusLoading = true;
    statusFailed = false;
    try {
      const next = await fetchRecallExtractionStatus(signal);
      if (!statusRead.isCurrent(signal)) return;
      status = next;
      statusUpdatedAt = Date.now();
    } catch (error) {
      if (isAbortError(error) || !statusRead.isCurrent(signal)) return;
      status = null;
      statusFailed = true;
    } finally {
      if (statusRead.finish(signal)) statusLoading = false;
    }
  }

  async function loadProgress(cursor = "") {
    const signal = progressRead.begin();
    const appending = cursor !== "";
    progressLoading = true;
    progressFailed = false;
    try {
      const page = await fetchRecallExtractionProgress({
        generation: status?.fingerprint || undefined,
        state: progressState || undefined,
        cursor: cursor || undefined,
      }, signal);
      if (!progressRead.isCurrent(signal)) return;
      progress = appending
        ? [...progress, ...page.progress]
        : page.progress;
      progressNextCursor = page.nextCursor ?? "";
    } catch (error) {
      if (isAbortError(error) || !progressRead.isCurrent(signal)) return;
      if (!appending) {
        progress = [];
        progressNextCursor = "";
        progressFailed = true;
      }
    } finally {
      if (progressRead.finish(signal)) progressLoading = false;
    }
  }

  function scheduleSearch() {
    clearTimeout(searchTimer);
    searchTimer = setTimeout(() => {
      query = search.trim();
    }, 250);
  }

  async function refreshRecall() {
    await Promise.all([loadEntries(), loadStatus()]);
    if (progressExpanded) await loadProgress();
  }

  function toggleProgress() {
    progressExpanded = !progressExpanded;
    if (!progressExpanded) {
      progressRead.cancel();
      progressLoading = false;
    }
  }

  function requestGenerationAction(
    kind: GenerationAction["kind"],
    generation: RecallExtractGeneration,
  ) {
    generationActionError = "";
    generationAction = { kind, generation };
  }

  function closeGenerationAction() {
    if (generationActionLoading) return;
    generationAction = null;
    generationActionError = "";
  }

  async function confirmGenerationAction() {
    const action = generationAction;
    if (!action || generationActionLoading) return;
    generationActionLoading = true;
    generationActionError = "";
    try {
      if (action.kind === "activate") {
        await activateRecallExtractionGeneration();
      } else {
        await retireRecallExtractionGeneration(action.generation.fingerprint);
      }
      await refreshRecall();
      generationAction = null;
    } catch (error) {
      const detail = error instanceof Error ? error.message : String(error);
      generationActionError = m.recall_page_generation_action_error({
        error: detail,
      });
    } finally {
      generationActionLoading = false;
    }
  }

  function progressStateLabel(state: RecallExtractProgressState): string {
    switch (state) {
      case "pending":
        return m.recall_page_progress_pending();
      case "partial":
        return m.recall_page_progress_partial();
      case "failed":
        return m.recall_page_progress_failed();
    }
  }

  function reviewStateLabel(state: string): string {
    switch (state) {
      case "human_reviewed":
        return m.recall_page_review_state_human_reviewed();
      case "human_rejected":
        return m.recall_page_review_state_human_rejected();
      case "unreviewed_auto":
        return m.recall_page_review_state_unreviewed_auto();
      case "calibrated_auto":
        return m.recall_page_review_state_calibrated_auto();
      case "eval_raw":
        return m.recall_page_review_state_eval_raw();
      default:
        return state;
    }
  }

  function isReviewable(entry: RecallEntry): boolean {
    return entry.status === "accepted" &&
      entry.review_state === "unreviewed_auto";
  }

  function keepAfterReview(entry: RecallEntry): boolean {
    return entry.status === "accepted" &&
      (!reviewState || entry.review_state === reviewState);
  }

  async function submitReview(
    entry: RecallEntry,
    action: RecallReviewAction,
  ) {
    if (reviewingEntryIds.includes(entry.id)) return;
    reviewingEntryIds = [...reviewingEntryIds, entry.id];
    reviewErrors = { ...reviewErrors, [entry.id]: "" };
    try {
      const updated = await reviewRecallEntry(entry.id, action);
      const keep = keepAfterReview(updated);
      entries = keep
        ? entries.map((item) => item.id === updated.id ? updated : item)
        : entries.filter((item) => item.id !== updated.id);
      if (!keep) {
        expandedEntryIds = expandedEntryIds.filter((id) => id !== updated.id);
      }
      archiveEntry = null;
    } catch (error) {
      const detail = error instanceof Error ? error.message : String(error);
      reviewErrors = {
        ...reviewErrors,
        [entry.id]: m.recall_page_review_error({ error: detail }),
      };
    } finally {
      reviewingEntryIds = reviewingEntryIds.filter((id) => id !== entry.id);
    }
  }

  function closeArchiveReview() {
    if (archiveEntry && reviewingEntryIds.includes(archiveEntry.id)) return;
    archiveEntry = null;
  }

  function progressTimestamp(value: string): string {
    return formatDateTime(value, {
      dateStyle: "medium",
      timeStyle: "short",
    });
  }

  function evidenceLabel(evidence: RecallEvidence): string {
    return m.session_recall_evidence_range({
      start: evidence.message_start_ordinal,
      end: evidence.message_end_ordinal,
    });
  }

  function jumpToEvidence(evidence: RecallEvidence) {
    ui.scrollToOrdinal(
      evidence.message_start_ordinal,
      evidence.session_id,
    );
    router.navigateToSession(evidence.session_id);
  }

  function toggleEntry(entryId: string) {
    expandedEntryIds = expandedEntryIds.includes(entryId)
      ? expandedEntryIds.filter((id) => id !== entryId)
      : [...expandedEntryIds, entryId];
  }

  $effect(() => {
    query;
    project;
    entryType;
    generation;
    reviewState;
    void loadEntries();
  });

  $effect(() => {
    void loadStatus();
    return () => {
      clearTimeout(searchTimer);
      entriesRead.cancel();
      statusRead.cancel();
      progressRead.cancel();
    };
  });

  $effect(() => {
    if (!progressExpanded) return;
    progressState;
    status?.fingerprint;
    void loadProgress();
  });
</script>

{#snippet generationActionFooter()}
  {#if generationAction}
    <span class="generation-modal-actions">
      <Button
        label={m.recall_page_generation_cancel()}
        tone="neutral"
        surface="outline"
        disabled={generationActionLoading}
        onclick={closeGenerationAction}
      />
      <Button
        label={generationAction.kind === "activate"
          ? m.recall_page_generation_activate_title()
          : m.recall_page_generation_retire_title()}
        tone={generationAction.kind === "activate" ? "info" : "danger"}
        surface="solid"
        disabled={generationActionLoading}
        onclick={confirmGenerationAction}
      />
    </span>
  {/if}
{/snippet}

{#snippet archiveReviewFooter()}
  {#if archiveEntry}
    {@const busy = reviewingEntryIds.includes(archiveEntry.id)}
    <span class="review-modal-actions">
      <Button
        label={m.recall_page_review_cancel()}
        tone="neutral"
        surface="outline"
        disabled={busy}
        onclick={closeArchiveReview}
      />
      <Button
        label={m.recall_page_review_archive()}
        tone="danger"
        surface="solid"
        disabled={busy}
        onclick={() => submitReview(archiveEntry!, "archive")}
      />
    </span>
  {/if}
{/snippet}

<div class="recall-corpus-panel">
  <header class="recall-page-header">
    <div>
      <h2>{m.recall_page_title()}</h2>
      <p>{m.recall_page_subtitle()}</p>
    </div>
    <div class="header-actions">
      {#if !entriesLoading && !entriesFailed}
        <span class="entry-count">
          {m.recall_page_entries_shown({
            countLabel: entries.length.toLocaleString(),
          })}
        </span>
      {/if}
      <RefreshControl
        {lastUpdatedAt}
        busy={entriesLoading || statusLoading || progressLoading}
        onRefresh={refreshRecall}
        label={m.shared_refresh()}
      />
    </div>
  </header>

  <Card level="default" padding="none" class="extraction-card">
    <div class="extraction-content">
      <div class="extraction-heading">
        <h3>{m.recall_page_extraction_title()}</h3>
        <div class="extraction-actions">
          {#if status?.configured && status.generations?.length}
            <span class="generation-state">
              {status.generations.find(
                (item) => item.fingerprint === status?.fingerprint,
              )?.state ?? status.generations[0]?.state}
            </span>
          {/if}
          {#if !statusLoading && !statusFailed && status?.progress_available}
            <Button
              size="sm"
              tone="neutral"
              surface="outline"
              label={progressExpanded
                ? m.recall_page_progress_hide()
                : m.recall_page_progress_show()}
              onclick={toggleProgress}
            />
          {/if}
        </div>
      </div>
      {#if statusLoading}
        <p class="status-state">{m.recall_page_loading()}</p>
      {:else if statusFailed}
        <p class="status-state status-error">
          {m.recall_page_extraction_error()}
        </p>
      {:else if !status?.configured}
        <p class="status-state">
          {m.recall_page_extraction_unconfigured()}
        </p>
      {:else if status.stats}
        <div class="status-metrics">
          <span>{m.recall_page_status_done({
            countLabel: status.stats.done.toLocaleString(),
          })}</span>
          <span>{m.recall_page_status_failed({
            countLabel: status.stats.failed.toLocaleString(),
          })}</span>
          <span>{m.recall_page_status_eligible({
            countLabel: (status.eligible_backlog ?? 0).toLocaleString(),
          })}</span>
          <span>{m.recall_page_status_pending({
            countLabel: status.stats.pending.toLocaleString(),
          })}</span>
          <span>{m.recall_page_status_partial({
            countLabel: status.stats.partial.toLocaleString(),
          })}</span>
          <span>{m.recall_page_status_units({
            doneLabel: status.stats.units_done.toLocaleString(),
            totalLabel: status.stats.units_total.toLocaleString(),
          })}</span>
          <span>{m.recall_page_status_entries({
            countLabel: status.stats.entries.toLocaleString(),
          })}</span>
        </div>
      {/if}

      {#if status?.management_available && status.generations?.length}
        <section class="generation-management">
          <h4>{m.recall_page_generations_title()}</h4>
          <div class="generation-list">
            {#each status.generations as item (item.fingerprint)}
              <div class="generation-row">
                <div class="generation-identity">
                  <span class="generation-model">{item.model}</span>
                  <code title={item.fingerprint}>
                    {item.fingerprint.slice(0, 12)}
                  </code>
                  <span class="generation-state">{item.state}</span>
                </div>
                {#if item.fingerprint === status.fingerprint &&
                item.state !== "active"}
                  <div class="generation-actions">
                    <Button
                      size="sm"
                      tone="info"
                      surface="outline"
                      label={m.recall_page_generation_activate()}
                      onclick={() => requestGenerationAction("activate", item)}
                    />
                  </div>
                {:else if item.state === "building"}
                  <div class="generation-actions">
                    <Button
                      size="sm"
                      tone="danger"
                      surface="outline"
                      label={m.recall_page_generation_retire()}
                      onclick={() => requestGenerationAction("retire", item)}
                    />
                  </div>
                {/if}
              </div>
            {/each}
          </div>
        </section>
      {/if}

      {#if progressExpanded}
        <div class="progress-panel">
          <SegmentedControl
            options={progressStateOptions}
            value={progressState}
            ariaLabel={m.recall_page_progress_filter_label()}
            onchange={(value) => {
              progressState = value as "" | RecallExtractProgressState;
            }}
          />
          {#if progressLoading && progress.length === 0}
            <p class="progress-state">{m.recall_page_loading()}</p>
          {:else if progressFailed}
            <p class="progress-state status-error">
              {m.recall_page_progress_error()}
            </p>
          {:else if progress.length === 0}
            <p class="progress-state">{m.recall_page_progress_empty()}</p>
          {:else}
            <div class="progress-table-shell">
              <Table
                ariaLabel={m.recall_page_progress_table_label()}
                stickyHeader={false}
                zebra={false}
                class="progress-table"
              >
                {#snippet header()}
                  <TableHeaderCell
                    label={m.recall_page_progress_session_column()}
                  />
                  <TableHeaderCell
                    label={m.recall_page_progress_state_column()}
                  />
                  <TableHeaderCell
                    label={m.recall_page_progress_units_column()}
                  />
                  <TableHeaderCell
                    label={m.recall_page_progress_updated_column()}
                  />
                {/snippet}
                {#snippet children()}
                  {#each progress as item (`${item.generation_fingerprint}:${item.session_id}`)}
                    <tr>
                      <td class="progress-session-cell">
                        <a
                          class="progress-session-link"
                          href={router.buildSessionHref(item.session_id)}
                          onclick={(event) => {
                            if (
                              event.metaKey || event.ctrlKey ||
                              event.shiftKey || event.altKey ||
                              event.button !== 0
                            ) return;
                            event.preventDefault();
                            router.navigateToSession(item.session_id);
                          }}
                        >{item.session_title}</a>
                        <span class="progress-session-meta">
                          {item.project} · {item.agent}
                        </span>
                        {#if item.last_error}
                          <span class="progress-error">{item.last_error}</span>
                        {/if}
                      </td>
                      <td>
                        <span class="progress-state-label" data-state={item.state}>
                          {progressStateLabel(item.state)}
                        </span>
                        {#if item.retry_eligible}
                          <span class="retry-state">
                            {m.recall_page_progress_retry_ready()}
                          </span>
                        {:else if item.retry_at}
                          <span class="retry-state">
                            {m.recall_page_progress_retry_after({
                              date: progressTimestamp(item.retry_at),
                            })}
                          </span>
                        {/if}
                      </td>
                      <td class="progress-units">
                        {item.unit_cursor.toLocaleString()} /
                        {item.units_total.toLocaleString()}
                      </td>
                      <td class="progress-updated">
                        {progressTimestamp(item.updated_at)}
                      </td>
                    </tr>
                  {/each}
                {/snippet}
              </Table>
            </div>
            {#if progressNextCursor}
              <div class="progress-load-more">
                <Button
                  size="sm"
                  tone="neutral"
                  surface="outline"
                  label={progressLoading
                    ? m.recall_page_progress_loading_more()
                    : m.recall_page_progress_load_more()}
                  disabled={progressLoading}
                  onclick={() => loadProgress(progressNextCursor)}
                />
              </div>
            {/if}
          {/if}
        </div>
      {/if}
    </div>
  </Card>

  {#if generationAction}
    <Modal
      title={generationAction.kind === "activate"
        ? m.recall_page_generation_activate_title()
        : m.recall_page_generation_retire_title()}
      closeLabel={m.recall_page_generation_close()}
      tone={generationAction.kind === "activate" ? "info" : "danger"}
      width="460px"
      closeOnOverlayClick={!generationActionLoading}
      onclose={closeGenerationAction}
      footer={generationActionFooter}
    >
      <p class="generation-modal-copy">
        {generationAction.kind === "activate"
          ? m.recall_page_generation_activate_message({
              model: generationAction.generation.model,
              fingerprint: generationAction.generation.fingerprint,
            })
          : m.recall_page_generation_retire_message({
              model: generationAction.generation.model,
              fingerprint: generationAction.generation.fingerprint,
            })}
      </p>
      {#if generationActionError}
        <p class="generation-action-error">{generationActionError}</p>
      {/if}
    </Modal>
  {/if}

  {#if archiveEntry}
    <Modal
      title={m.recall_page_review_archive_title()}
      closeLabel={m.recall_page_review_close()}
      tone="danger"
      width="460px"
      closeOnOverlayClick={!reviewingEntryIds.includes(archiveEntry.id)}
      onclose={closeArchiveReview}
      footer={archiveReviewFooter}
    >
      <p class="review-modal-copy">
        {m.recall_page_review_archive_message({ title: archiveEntry.title })}
      </p>
    </Modal>
  {/if}

  <div class="recall-toolbar">
    <SearchInput
      class="recall-search"
      bind:value={search}
      oninput={scheduleSearch}
      placeholder={m.recall_page_search_placeholder()}
      ariaLabel={m.recall_page_search_placeholder()}
      clearLabel={m.recall_page_clear_search()}
      block
    />
    <Typeahead
      options={projectOptions}
      value={project}
      fallbackLabel={m.shared_all_projects()}
      placeholder={m.shared_project_filter_placeholder()}
      title={m.shared_select_project()}
      emptyLabel={m.shared_no_matching_projects()}
      onselect={(value) => {
        project = value;
      }}
    />
    <Typeahead
      options={typeOptions}
      value={entryType}
      fallbackLabel={m.recall_page_all_types()}
      placeholder={m.recall_page_type_filter()}
      title={m.recall_page_type_filter()}
      emptyLabel={m.recall_page_all_types()}
      onselect={(value) => {
        entryType = value;
      }}
    />
    <Typeahead
      options={generationOptions}
      value={generation}
      fallbackLabel={m.recall_page_all_generations()}
      placeholder={m.recall_page_generation_filter()}
      title={m.recall_page_generation_filter()}
      emptyLabel={m.recall_page_all_generations()}
      onselect={(value) => {
        generation = value;
      }}
    />
    <Typeahead
      options={reviewOptions}
      value={reviewState}
      fallbackLabel={m.recall_page_all_review_states()}
      placeholder={m.recall_page_review_filter()}
      title={m.recall_page_review_filter()}
      emptyLabel={m.recall_page_all_review_states()}
      onselect={(value) => {
        reviewState = value;
      }}
    />
  </div>

  {#if resultCap > 0}
    <p class="result-cap">
      {m.recall_page_ranked_result_cap({
        countLabel: resultCap.toLocaleString(),
      })}
    </p>
  {/if}

  {#if entriesLoading && entries.length === 0}
    <p class="entries-state">{m.recall_page_loading()}</p>
  {:else if entriesFailed}
    <p class="entries-state status-error">{m.recall_page_error()}</p>
  {:else if entries.length === 0}
    <EmptyState title={m.recall_page_empty()} />
  {:else}
    <div class="recall-table-shell">
      <Table
        ariaLabel={m.recall_page_table_label()}
        stickyHeader={false}
        zebra={false}
        class="recall-table"
      >
        {#snippet header()}
          <TableHeaderCell class="expand-column" />
          <TableHeaderCell label={m.recall_page_fact_column()} />
          <TableHeaderCell label={m.recall_page_type_filter()} />
          <TableHeaderCell label={m.recall_page_project_column()} />
          <TableHeaderCell label={m.recall_page_review_filter()} />
        {/snippet}
        {#snippet children()}
          {#each entries as entry (entry.id)}
            {@const expanded = expandedEntryIds.includes(entry.id)}
            <tr class:entry-row-expanded={expanded}>
              <td class="expand-cell">
                <button
                  type="button"
                  class="expand-button"
                  aria-expanded={expanded}
                  aria-label={expanded
                    ? m.recall_page_collapse_entry({ title: entry.title })
                    : m.recall_page_expand_entry({ title: entry.title })}
                  onclick={() => toggleEntry(entry.id)}
                >
                  {#if expanded}
                    <ChevronDownIcon size="14" strokeWidth="2.2" aria-hidden="true" />
                  {:else}
                    <ChevronRightIcon size="14" strokeWidth="2.2" aria-hidden="true" />
                  {/if}
                </button>
              </td>
              <td class="fact-cell">
                <span class="entry-title">{entry.title}</span>
                {#if !entry.provenance_ok}
                  <span class="provenance-revoked">
                    {m.session_recall_provenance_revoked()}
                  </span>
                {/if}
              </td>
              <td><span class="entry-type">{entry.type}</span></td>
              <td class="project-cell">{entry.project ?? "—"}</td>
              <td class="review-cell">{reviewStateLabel(entry.review_state)}</td>
            </tr>
            {#if expanded}
              <tr class="entry-detail-row">
                <td colspan="5">
                  <div class="entry-detail">
                    <p class="entry-body">{entry.body}</p>
                    {#if entry.trigger}
                      <div class="detail-section">
                        <h4>{m.recall_page_trigger_label()}</h4>
                        <p>{entry.trigger}</p>
                      </div>
                    {/if}
                    {#if entry.uncertainty}
                      <div class="detail-section">
                        <h4>{m.recall_page_uncertainty_label()}</h4>
                        <p>{entry.uncertainty}</p>
                      </div>
                    {/if}
                    <div class="entry-meta">
                      <span>{entry.scope}</span>
                      {#if entry.agent}<span>{entry.agent}</span>{/if}
                      {#if entry.extractor_method}
                        <span>{entry.extractor_method}</span>
                      {/if}
                      {#if entry.model}<span>{entry.model}</span>{/if}
                      {#if entry.source_run_id}
                        <span>{m.session_recall_generation()}: {entry.source_run_id}</span>
                      {/if}
                    </div>
                    {#if entry.evidence?.length}
                      <div class="entry-evidence">
                        {#each entry.evidence as evidence (evidence.id)}
                          {#if entry.provenance_ok}
                            <Button
                              class="evidence-button"
                              size="sm"
                              tone="neutral"
                              surface="outline"
                              label={evidenceLabel(evidence)}
                              onclick={() => jumpToEvidence(evidence)}
                              title={m.session_recall_jump_evidence({
                                start: evidence.message_start_ordinal,
                                end: evidence.message_end_ordinal,
                              })}
                            />
                          {:else}
                            <span>{evidenceLabel(evidence)}</span>
                          {/if}
                        {/each}
                      </div>
                    {/if}
                    {#if isReviewable(entry)}
                      {@const busy = reviewingEntryIds.includes(entry.id)}
                      <div class="entry-review">
                        <div class="entry-review-actions">
                          <Button
                            size="sm"
                            tone="info"
                            surface="solid"
                            label={m.recall_page_review_approve()}
                            disabled={busy || !entry.provenance_ok}
                            title={!entry.provenance_ok
                              ? m.recall_page_review_approve_disabled()
                              : undefined}
                            onclick={() => submitReview(entry, "approve")}
                          />
                          <Button
                            size="sm"
                            tone="danger"
                            surface="outline"
                            label={m.recall_page_review_archive()}
                            disabled={busy}
                            onclick={() => {
                              reviewErrors = {
                                ...reviewErrors,
                                [entry.id]: "",
                              };
                              archiveEntry = entry;
                            }}
                          />
                        </div>
                        {#if !entry.provenance_ok}
                          <p class="entry-review-note">
                            {m.recall_page_review_approve_disabled()}
                          </p>
                        {/if}
                        {#if reviewErrors[entry.id]}
                          <p class="entry-review-error">
                            {reviewErrors[entry.id]}
                          </p>
                        {/if}
                      </div>
                    {/if}
                  </div>
                </td>
              </tr>
            {/if}
          {/each}
        {/snippet}
      </Table>
    </div>
    {#if nextCursor}
      <div class="load-more">
        <Button
          size="sm"
          tone="neutral"
          surface="outline"
          label={entriesLoading
            ? m.recall_page_loading_more()
            : m.recall_page_load_more()}
          disabled={entriesLoading}
          onclick={() => loadEntries(nextCursor)}
        />
      </div>
    {/if}
  {/if}
</div>

<style>
  .recall-corpus-panel {
    max-width: 1400px;
    margin: 0 auto;
    padding: 48px 40px 56px;
  }

  .recall-page-header {
    display: flex;
    align-items: flex-end;
    justify-content: space-between;
    gap: var(--space-6);
    margin-bottom: var(--space-6);
  }

  h2,
  h3,
  p {
    margin: 0;
  }

  .recall-page-header h2 {
    color: var(--text-primary);
    font-size: 20px;
    font-weight: 600;
  }

  .recall-page-header p {
    margin-top: var(--space-2);
    color: var(--text-muted);
    font-size: 12px;
  }

  .entry-count {
    color: var(--text-muted);
    font-family: var(--font-mono);
    font-size: 10px;
  }

  .header-actions {
    display: flex;
    align-items: center;
    gap: var(--space-4);
  }

  .extraction-content {
    padding: 18px 20px;
  }

  .extraction-heading {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: var(--space-4);
  }

  .extraction-actions {
    display: flex;
    align-items: center;
    gap: var(--space-4);
  }

  .extraction-heading h3 {
    color: var(--text-primary);
    font-size: 12px;
    font-weight: 600;
  }

  .generation-state,
  .entry-type {
    color: var(--accent-blue);
    font-family: var(--font-mono);
    font-size: 9px;
    text-transform: uppercase;
  }

  .status-state,
  .entries-state {
    padding: var(--space-6) 0;
    color: var(--text-muted);
    font-size: 12px;
    text-align: center;
  }

  .status-state {
    padding-bottom: 0;
    text-align: left;
  }

  .status-error,
  .provenance-revoked {
    color: var(--slow-fg);
  }

  .status-metrics {
    display: flex;
    flex-wrap: wrap;
    gap: var(--space-3) var(--space-6);
    margin-top: var(--space-4);
    color: var(--text-secondary);
    font-family: var(--font-mono);
    font-size: 10px;
  }

  .generation-management {
    margin-top: var(--space-5);
    padding-top: var(--space-5);
    border-top: 1px solid var(--border-default);
  }

  .generation-management h4 {
    margin: 0 0 var(--space-3);
    color: var(--text-secondary);
    font-size: 10px;
    font-weight: 600;
  }

  .generation-list {
    display: grid;
    gap: var(--space-2);
  }

  .generation-row {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: var(--space-4);
    min-height: 38px;
    padding: var(--space-2) var(--space-3);
    background: var(--bg-inset);
    border: 1px solid var(--border-default);
    border-radius: var(--radius-md);
  }

  .generation-identity,
  .generation-actions {
    display: flex;
    align-items: center;
    gap: var(--space-3);
  }

  .generation-model {
    color: var(--text-primary);
    font-size: 11px;
    font-weight: 600;
  }

  .generation-identity code {
    color: var(--text-muted);
    font-family: var(--font-mono);
    font-size: 9px;
  }

  .generation-modal-actions {
    display: contents;
  }

  .review-modal-actions {
    display: contents;
  }

  .generation-modal-copy,
  .generation-action-error,
  .review-modal-copy {
    margin: 0;
    color: var(--text-secondary);
    font-size: 12px;
    line-height: 1.55;
    overflow-wrap: anywhere;
  }

  .generation-action-error {
    margin-top: var(--space-4);
    color: var(--slow-fg);
  }

  .progress-panel {
    margin-top: var(--space-6);
    padding-top: var(--space-5);
    border-top: 1px solid var(--border-default);
  }

  .progress-state {
    padding: var(--space-6) 0 var(--space-2);
    color: var(--text-muted);
    font-size: 12px;
    text-align: center;
  }

  .progress-table-shell {
    margin-top: var(--space-4);
    overflow-x: auto;
    border: 1px solid var(--border-default);
    border-radius: var(--radius-md);
  }

  :global(.progress-table table) {
    min-width: 680px;
  }

  :global(.progress-table thead th) {
    padding: 9px 12px;
  }

  :global(.progress-table tbody td) {
    padding: 11px 12px;
    vertical-align: top;
  }

  .progress-session-cell {
    width: 52%;
  }

  .progress-session-link {
    display: block;
    color: var(--accent-blue);
    font-size: 11px;
    font-weight: 600;
    text-decoration: none;
  }

  .progress-session-link:hover {
    text-decoration: underline;
  }

  .progress-session-meta,
  .progress-error,
  .retry-state {
    display: block;
    margin-top: var(--space-1);
    color: var(--text-muted);
    font-size: 9px;
  }

  .progress-error {
    color: var(--slow-fg);
    overflow-wrap: anywhere;
  }

  .progress-state-label,
  .progress-units,
  .progress-updated {
    color: var(--text-secondary);
    font-family: var(--font-mono);
    font-size: 9px;
  }

  .progress-state-label[data-state="failed"] {
    color: var(--slow-fg);
  }

  .progress-load-more {
    display: flex;
    justify-content: center;
    margin-top: var(--space-4);
  }

  .recall-toolbar {
    display: grid;
    grid-template-columns: minmax(240px, 2fr) repeat(4, minmax(130px, 1fr));
    gap: var(--space-4);
    margin: var(--space-7) 0;
  }

  .recall-table-shell {
    overflow-x: auto;
    border: 1px solid var(--border-default);
    border-radius: var(--radius-md);
    background: var(--bg-surface);
  }

  .result-cap {
    margin: calc(-1 * var(--space-3)) 0 var(--space-5);
    color: var(--text-muted);
    font-size: 11px;
  }

  :global(.recall-table table) {
    min-width: 760px;
  }

  :global(.recall-table thead th) {
    padding: 10px 14px;
  }

  :global(.recall-table tbody td) {
    padding: 12px 14px;
    vertical-align: middle;
  }

  :global(.recall-table tbody tr:last-child td) {
    border-bottom: 0;
  }

  :global(.recall-table .expand-column),
  .expand-cell {
    width: 42px;
  }

  :global(.recall-table tbody td.expand-cell) {
    padding-right: 0;
  }

  .expand-button {
    display: inline-flex;
    align-items: center;
    justify-content: center;
    width: 26px;
    height: 26px;
    padding: 0;
    color: var(--text-muted);
    background: transparent;
    border: 0;
    border-radius: var(--radius-sm);
    cursor: pointer;
  }

  .expand-button:hover {
    color: var(--text-primary);
    background: var(--bg-surface-hover);
  }

  .expand-button:focus-visible {
    outline: 2px solid var(--accent-blue);
    outline-offset: 1px;
  }

  .fact-cell {
    width: 52%;
  }

  .project-cell,
  .review-cell {
    color: var(--text-muted);
    font-family: var(--font-mono);
    font-size: 10px;
  }

  .provenance-revoked {
    display: block;
    margin-top: var(--space-1);
    font-family: var(--font-mono);
    font-size: 9px;
  }

  .entry-title {
    color: var(--text-primary);
    font-size: 12px;
    font-weight: 600;
    line-height: 1.4;
  }

  :global(.recall-table tbody tr.entry-row-expanded td) {
    border-bottom: 0;
    background: var(--bg-surface-hover);
  }

  :global(.recall-table tbody tr.entry-detail-row:hover),
  :global(.recall-table tbody tr.entry-detail-row td) {
    background: var(--bg-surface-hover);
  }

  :global(.recall-table tbody tr.entry-detail-row td) {
    padding: 0 24px 22px 68px;
  }

  .entry-detail {
    max-width: 920px;
  }

  .entry-body,
  .detail-section p {
    color: var(--text-secondary);
    font-size: 12px;
    line-height: 1.65;
    overflow-wrap: anywhere;
  }

  .detail-section {
    margin-top: var(--space-5);
  }

  .detail-section h4 {
    margin: 0 0 var(--space-2);
    color: var(--text-muted);
    font-size: 10px;
    font-weight: 600;
    letter-spacing: 0.04em;
    text-transform: uppercase;
  }

  .entry-meta,
  .entry-evidence {
    display: flex;
    flex-wrap: wrap;
    gap: var(--space-2) var(--space-4);
    margin-top: var(--space-5);
    color: var(--text-muted);
    font-family: var(--font-mono);
    font-size: 9px;
  }

  :global(.evidence-button) {
    font-family: var(--font-mono);
    font-size: 9px;
  }

  .entry-review {
    margin-top: var(--space-5);
    padding-top: var(--space-5);
    border-top: 1px solid var(--border-default);
  }

  .entry-review-actions {
    display: flex;
    flex-wrap: wrap;
    gap: var(--space-3);
  }

  .entry-review-note,
  .entry-review-error {
    margin-top: var(--space-3);
    color: var(--text-muted);
    font-size: 10px;
    line-height: 1.5;
  }

  .entry-review-error {
    color: var(--slow-fg);
  }

  .load-more {
    display: flex;
    justify-content: center;
    margin-top: var(--space-6);
  }

  @media (max-width: 900px) {
    .recall-toolbar {
      grid-template-columns: repeat(2, minmax(0, 1fr));
    }

    :global(.recall-search) {
      grid-column: 1 / -1;
    }
  }

  @media (max-width: 640px) {
    .recall-corpus-panel {
      padding: 28px 16px 40px;
    }

    .recall-page-header {
      align-items: flex-start;
      flex-direction: column;
    }

    .recall-toolbar {
      grid-template-columns: 1fr;
    }

    :global(.recall-search) {
      grid-column: auto;
    }

    .generation-row,
    .generation-identity {
      align-items: flex-start;
      flex-direction: column;
    }
  }
</style>
