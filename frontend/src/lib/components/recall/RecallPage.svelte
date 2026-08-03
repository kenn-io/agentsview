<script lang="ts">
  import {
    Button,
    Card,
    EmptyState,
    SearchInput,
    Table,
    TableHeaderCell,
    Typeahead,
    type TypeaheadOption,
  } from "@kenn-io/kit-ui";
  import {
    fetchRecallEntries,
    fetchRecallExtractionStatus,
  } from "../../api/recall.js";
  import type {
    RecallEntry,
    RecallEvidence,
    RecallExtractionStatus,
  } from "../../api/types/recall.js";
  import { ApiError, isAbortError } from "../../api/runtime.js";
  import { m } from "../../i18n/index.js";
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
    "unreviewed_auto",
    "calibrated_auto",
    "eval_raw",
  ];

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
      label: name,
      displayLabel: name,
    })),
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

  function scheduleSearch() {
    clearTimeout(searchTimer);
    searchTimer = setTimeout(() => {
      query = search.trim();
    }, 250);
  }

  async function refreshRecall() {
    await Promise.all([loadEntries(), loadStatus()]);
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
    };
  });
</script>

<div class="recall-page">
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
        busy={entriesLoading || statusLoading}
        onRefresh={refreshRecall}
        label={m.shared_refresh()}
      />
    </div>
  </header>

  <Card level="default" padding="none" class="extraction-card">
    <div class="extraction-content">
      <div class="extraction-heading">
        <h3>{m.recall_page_extraction_title()}</h3>
        {#if status?.configured && status.generations?.length}
          <span class="generation-state">
            {status.generations.find(
              (item) => item.fingerprint === status?.fingerprint,
            )?.state ?? status.generations[0]?.state}
          </span>
        {/if}
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
    </div>
  </Card>

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
              <td class="review-cell">{entry.review_state}</td>
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
  .recall-page {
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
    .recall-page {
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
  }
</style>
