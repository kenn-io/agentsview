<script lang="ts">
  import { Button, Card } from "@kenn-io/kit-ui";
  import { fetchSessionRecall } from "../../api/recall.js";
  import type {
    RecallEntry,
    RecallEvidence,
  } from "../../api/types/recall.js";
  import { isAbortError } from "../../api/runtime.js";
  import { m } from "../../i18n/index.js";
  import { ui } from "../../stores/ui.svelte.js";
  import { LatestRead } from "../../utils/latest-read.js";

  interface Props {
    sessionId: string;
  }

  let { sessionId }: Props = $props();
  let entries = $state<RecallEntry[]>([]);
  let loading = $state(true);
  let failed = $state(false);
  const read = new LatestRead();

  async function load(id: string) {
    const signal = read.begin();
    loading = true;
    failed = false;
    try {
      const next = await fetchSessionRecall(id, signal);
      if (!read.isCurrent(signal)) return;
      entries = next;
    } catch (error) {
      if (isAbortError(error) || !read.isCurrent(signal)) return;
      entries = [];
      failed = true;
    } finally {
      if (read.finish(signal)) loading = false;
    }
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
      evidence.session_id || sessionId,
    );
  }

  $effect(() => {
    const id = sessionId;
    void load(id);
    return () => read.cancel();
  });
</script>

<section class="recall-panel" aria-labelledby="session-recall-title">
  <header class="recall-header">
    <span id="session-recall-title">{m.session_recall_title()}</span>
    {#if !loading && !failed}
      <span class="recall-count">{entries.length}</span>
    {/if}
  </header>

  {#if loading}
    <p class="recall-state">{m.session_recall_loading()}</p>
  {:else if failed}
    <p class="recall-state recall-error">{m.session_recall_error()}</p>
  {:else if entries.length === 0}
    <p class="recall-state">{m.session_recall_empty()}</p>
  {:else}
    <div class="recall-list">
      {#each entries as entry (entry.id)}
        <Card level="inset" padding="none" class="recall-entry">
          <div class="recall-entry-head">
            <span class="recall-type">{entry.type}</span>
          </div>
          <h3>{entry.title}</h3>
          <p class="recall-body">{entry.body}</p>
          <dl class="recall-meta">
            <div>
              <dt>{m.session_recall_generation()}</dt>
              <dd title={entry.source_run_id || undefined}>
                {entry.source_run_id || "—"}
              </dd>
            </div>
            <div>
              <dt>{m.session_recall_review_state()}</dt>
              <dd>{entry.review_state}</dd>
            </div>
          </dl>
          {#if entry.evidence?.length}
            <div class="recall-evidence">
              {#each entry.evidence as evidence (evidence.id)}
                <Button
                  class="recall-evidence-button"
                  size="sm"
                  tone="neutral"
                  surface="outline"
                  label={evidenceLabel(evidence)}
                  title={m.session_recall_jump_evidence({
                    start: evidence.message_start_ordinal,
                    end: evidence.message_end_ordinal,
                  })}
                  onclick={() => jumpToEvidence(evidence)}
                />
              {/each}
            </div>
          {/if}
        </Card>
      {/each}
    </div>
  {/if}
</section>

<style>
  .recall-panel {
    padding: 12px 14px 14px;
    border-bottom: 1px solid var(--border-muted);
  }

  .recall-header {
    display: flex;
    align-items: center;
    justify-content: space-between;
    margin-bottom: 9px;
    color: var(--text-muted);
    font-size: 9px;
    font-weight: 500;
    letter-spacing: 0.6px;
    text-transform: uppercase;
  }

  .recall-count {
    font-family: var(--font-mono);
    letter-spacing: 0;
  }

  .recall-state {
    margin: 0;
    color: var(--text-muted);
    font-size: 10px;
    line-height: 1.45;
  }

  .recall-error {
    color: var(--slow-fg);
  }

  .recall-list {
    display: grid;
    gap: var(--space-5);
  }

  :global(.recall-entry) {
    min-width: 0;
    padding: var(--space-4);
  }

  .recall-entry-head {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: 8px;
    margin-bottom: 6px;
    color: var(--text-muted);
    font-family: var(--font-mono);
    font-size: 9px;
  }

  .recall-type {
    color: var(--accent-blue);
    text-transform: uppercase;
  }

  h3 {
    margin: 0;
    color: var(--text-primary);
    font-size: 11px;
    font-weight: 600;
    line-height: 1.35;
  }

  .recall-body {
    margin: 5px 0 0;
    color: var(--text-secondary);
    font-size: 10px;
    line-height: 1.45;
    overflow-wrap: anywhere;
  }

  .recall-meta {
    display: grid;
    gap: var(--space-2);
    margin: 8px 0 0;
    padding-top: 7px;
    border-top: 1px solid var(--border-muted);
    font-family: var(--font-mono);
    font-size: 9px;
  }

  .recall-meta > div {
    display: grid;
    grid-template-columns: 68px minmax(0, 1fr);
    gap: 6px;
  }

  dt {
    color: var(--text-muted);
  }

  dd {
    min-width: 0;
    margin: 0;
    overflow: hidden;
    color: var(--text-primary);
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  .recall-evidence {
    display: flex;
    flex-wrap: wrap;
    gap: var(--space-2);
    margin-top: 8px;
  }

  :global(.recall-evidence-button) {
    font-family: var(--font-mono);
    font-size: 9px;
  }
</style>
