<script lang="ts">
  import { Button } from "@kenn-io/kit-ui";
  import type {
    SessionToolSequence,
    SessionToolSequenceCall,
    SessionToolSequencesResponse,
  } from "../../api/generated/index.js";
  import { m } from "../../i18n/index.js";
  import { ui } from "../../stores/ui.svelte.js";
  import { formatDuration } from "../../utils/duration.js";

  interface Props {
    data: SessionToolSequencesResponse | null;
    sessionId: string;
    loading: boolean;
    failed: boolean;
  }

  let { data, sessionId, loading, failed }: Props = $props();

  function countArgs(count: number) {
    return { count, countLabel: count.toLocaleString() };
  }

  function toolsIn(sequence: SessionToolSequence): string {
    return [...new Set(sequence.calls.map((call) => call.tool_name))].join(", ");
  }

  function endingLabel(ending: SessionToolSequence["ending"]): string {
    switch (ending) {
      case "recovered": return m.tool_sequences_ending_recovered();
      case "abandoned": return m.tool_sequences_ending_abandoned();
      case "open": return m.tool_sequences_ending_open();
      case "unknown": return m.tool_sequences_ending_unknown();
    }
  }

  function endingExplanation(ending: SessionToolSequence["ending"]): string {
    switch (ending) {
      case "recovered": return m.tool_sequences_recovered_explanation();
      case "abandoned": return m.tool_sequences_abandoned_explanation();
      case "open": return m.tool_sequences_open_explanation();
      case "unknown": return m.tool_sequences_unknown_explanation();
    }
  }

  function outcomeLabel(outcome: SessionToolSequenceCall["outcome"]): string {
    switch (outcome) {
      case "errored": return m.tool_sequences_outcome_errored();
      case "empty": return m.tool_sequences_outcome_empty();
      case "content": return m.tool_sequences_outcome_content();
      case "unknown": return m.tool_sequences_outcome_unknown();
    }
  }

  function repeatLabel(repeat: SessionToolSequenceCall["repeat"]): string | null {
    switch (repeat) {
      case "identical": return m.tool_sequences_repeat_identical();
      case "near_identical": return m.tool_sequences_repeat_near_identical();
      case "none": return null;
    }
  }

  function jumpToCall(call: SessionToolSequenceCall) {
    ui.scrollToOrdinal(call.ordinal, sessionId);
  }
</script>

<section
  class="tool-sequences-panel"
  aria-labelledby="tool-sequences-title"
  aria-busy={loading}
>
  <header class="panel-header">
    <h2 id="tool-sequences-title">{m.tool_sequences_title()}</h2>
    {#if data && !loading && !failed}
      <div class="panel-counts">
        <span>{m.tool_sequences_sequence_count(countArgs(data.total_sequences))}</span>
        <span>{m.tool_sequences_call_count(countArgs(data.total_tool_calls))}</span>
      </div>
    {/if}
  </header>
  <p class="panel-description">{m.tool_sequences_description()}</p>

  {#if loading}
    <p class="panel-state">{m.tool_sequences_loading()}</p>
  {:else if failed}
    <p class="panel-state panel-error" role="alert">{m.tool_sequences_error()}</p>
  {:else if data}
    {#if data.omitted_sequences > 0}
      <p class="omission-note">
        {m.tool_sequences_sequences_omitted(countArgs(data.omitted_sequences))}
      </p>
    {/if}
    {#if data.omitted_calls > 0}
      <p class="omission-note">
        {m.tool_sequences_total_calls_omitted(countArgs(data.omitted_calls))}
      </p>
    {/if}

    {#if data.total_tool_calls === 0}
      <p class="panel-state">{m.tool_sequences_none_recorded()}</p>
    {:else if data.total_sequences === 0}
      <p class="panel-state">{m.tool_sequences_no_sequences()}</p>
    {:else}
      <div class="sequence-list">
        {#each data.sequences as sequence, index (`${sessionId}-${index}`)}
          <details class="sequence">
            <summary class="sequence-summary">
              <span class="sequence-tools">{toolsIn(sequence)}</span>
              <span class="sequence-count">
                {m.tool_sequences_call_count(countArgs(sequence.total_calls))}
              </span>
              <span class="sequence-ending">{endingLabel(sequence.ending)}</span>
              {#if sequence.omitted_calls > 0}
                <span class="sequence-omission">
                  {m.tool_sequences_calls_omitted(countArgs(sequence.omitted_calls))}
                </span>
              {/if}
            </summary>
            <div class="sequence-content">
              <p class="sequence-explanation">{endingExplanation(sequence.ending)}</p>
              {#if sequence.identical}
                <p class="sequence-fact">{m.tool_sequences_has_identical_repeat()}</p>
              {/if}
              {#if sequence.near_identical}
                <p class="sequence-fact">{m.tool_sequences_has_near_identical_repeat()}</p>
              {/if}
              {#if sequence.tool_changed}
                <p class="sequence-fact">{m.tool_sequences_has_tool_switch()}</p>
              {/if}

              {#each sequence.calls as call (`${call.ordinal}-${call.call_index}-${call.tool_use_id}`)}
                <article class="sequence-call">
                  <div class="call-heading">
                    <Button
                      size="sm"
                      tone="neutral"
                      surface="outline"
                      label={m.tool_sequences_open_call({ ordinal: call.ordinal, tool: call.tool_name })}
                      title={m.tool_sequences_open_call({ ordinal: call.ordinal, tool: call.tool_name })}
                      onclick={() => jumpToCall(call)}
                    />
                    <strong class="tool-name">{call.tool_name}</strong>
                    <span class="outcome">{outcomeLabel(call.outcome)}</span>
                    {#if repeatLabel(call.repeat)}
                      <span class="call-fact">{repeatLabel(call.repeat)}</span>
                    {/if}
                    {#if call.tool_changed}
                      <span class="call-fact">{m.tool_sequences_tool_changed()}</span>
                    {/if}
                    <span class="duration">
                      {call.duration_ms === null
                        ? m.tool_sequences_not_measured()
                        : formatDuration(call.duration_ms)}
                    </span>
                  </div>
                  <p class="call-identity">
                    {m.tool_sequences_call_identity({ ordinal: call.ordinal, id: call.tool_use_id || m.tool_sequences_missing_identity() })}
                  </p>
                  <dl class="call-evidence">
                    <div class="evidence-row">
                      <dt>{m.tool_sequences_input()}</dt>
                      <dd>
                        {#if call.input_preview}
                          <pre>{call.input_preview}</pre>
                        {:else}
                          <span class="empty-preview">{m.tool_sequences_no_input()}</span>
                        {/if}
                        <span class="byte-count">
                          {m.tool_sequences_byte_count(countArgs(call.input_bytes))}
                        </span>
                        {#if call.input_omitted_bytes > 0}
                          <span class="omission-note">
                            {m.tool_sequences_input_omitted(countArgs(call.input_omitted_bytes))}
                          </span>
                        {/if}
                      </dd>
                    </div>
                    <div class="evidence-row">
                      <dt>{m.tool_sequences_result()}</dt>
                      <dd>
                        {#if call.result_content_unknown}
                          <span class="unknown-note">{m.tool_sequences_result_unknown()}</span>
                        {/if}
                        {#if call.result_preview}
                          <pre>{call.result_preview}</pre>
                        {:else if call.result_bytes !== null && call.result_bytes > 0}
                          <span class="empty-preview">
                            {m.tool_sequences_result_unavailable(countArgs(call.result_bytes))}
                          </span>
                        {:else if call.result_bytes === 0}
                          <span class="empty-preview">{m.tool_sequences_result_empty()}</span>
                        {:else}
                          <span class="empty-preview">{m.tool_sequences_result_size_unknown()}</span>
                        {/if}
                        {#if call.result_bytes !== null}
                          <span class="byte-count">
                            {m.tool_sequences_byte_count(countArgs(call.result_bytes))}
                          </span>
                        {/if}
                        {#if call.result_omitted_bytes !== null && call.result_omitted_bytes > 0}
                          <span class="omission-note">
                            {m.tool_sequences_result_omitted(countArgs(call.result_omitted_bytes))}
                          </span>
                        {/if}
                      </dd>
                    </div>
                  </dl>
                </article>
              {/each}
            </div>
          </details>
        {/each}
      </div>
    {/if}
  {/if}
</section>

<style>
  .tool-sequences-panel {
    max-height: clamp(10rem, 34vh, 24rem);
    min-width: 0;
    overflow: auto;
    padding: 10px 12px;
    background: var(--bg-inset);
    border-bottom: 1px solid var(--border-muted);
    color: var(--text-primary);
    font-size: 12px;
  }

  .panel-header,
  .panel-counts,
  .call-heading {
    display: flex;
    align-items: center;
    flex-wrap: wrap;
    gap: 6px 8px;
    min-width: 0;
  }

  .panel-header {
    justify-content: space-between;
  }

  h2 {
    margin: 0;
    font-size: 13px;
  }

  .panel-counts,
  .panel-description,
  .panel-state,
  .sequence-explanation,
  .sequence-fact,
  .call-identity {
    color: var(--text-muted);
  }

  .panel-description,
  .panel-state,
  .sequence-explanation,
  .sequence-fact,
  .call-identity {
    margin: 6px 0 0;
  }

  .panel-error,
  .unknown-note {
    color: var(--accent-amber);
  }

  .sequence-list {
    display: grid;
    gap: 6px;
    min-width: 0;
  }

  .sequence {
    min-width: 0;
    border: 1px solid var(--border-muted);
    border-radius: 4px;
    background: var(--bg-surface);
  }

  .sequence-summary {
    display: flex;
    align-items: baseline;
    flex-wrap: wrap;
    gap: 4px 8px;
    min-width: 0;
    padding: 7px 9px;
    cursor: pointer;
  }

  .sequence-tools,
  .tool-name {
    min-width: 0;
    overflow-wrap: anywhere;
  }

  .sequence-tools {
    flex: 1 1 10rem;
    font-weight: 600;
  }

  .sequence-count,
  .sequence-ending,
  .outcome,
  .duration,
  .byte-count {
    color: var(--text-secondary);
    font-variant-numeric: tabular-nums;
  }

  .sequence-ending,
  .outcome {
    color: var(--accent-blue);
  }

  .sequence-omission,
  .omission-note {
    color: var(--accent-amber);
  }

  .sequence-content {
    display: grid;
    gap: 8px;
    min-width: 0;
    padding: 0 9px 9px;
  }

  .sequence-call {
    min-width: 0;
    padding-top: 8px;
    border-top: 1px solid var(--border-muted);
  }

  .call-heading {
    gap: 6px;
  }

  .tool-name {
    font-size: 12px;
  }

  .outcome,
  .call-fact,
  .duration,
  .byte-count {
    overflow-wrap: anywhere;
  }

  .call-fact {
    color: var(--text-muted);
  }

  .duration {
    margin-left: auto;
  }

  .call-identity {
    overflow-wrap: anywhere;
  }

  .call-evidence {
    display: grid;
    gap: 8px;
    margin: 8px 0 0;
    min-width: 0;
  }

  .evidence-row {
    display: grid;
    grid-template-columns: minmax(4rem, 6rem) minmax(0, 1fr);
    gap: 8px;
    min-width: 0;
  }

  dt {
    color: var(--text-secondary);
    font-weight: 600;
  }

  dd {
    display: grid;
    gap: 4px;
    min-width: 0;
    margin: 0;
  }

  pre {
    max-width: 100%;
    min-width: 0;
    margin: 0;
    padding: 5px 7px;
    border-radius: 3px;
    background: var(--bg-inset);
    color: var(--text-primary);
    font: 11px/1.4 var(--font-mono);
    white-space: pre-wrap;
    overflow-wrap: anywhere;
    word-break: break-word;
  }

  .empty-preview,
  .byte-count {
    color: var(--text-muted);
  }

  @media (max-width: 640px) {
    .panel-header {
      align-items: flex-start;
      flex-direction: column;
    }

    .panel-counts {
      gap: 4px 8px;
    }

    .evidence-row {
      grid-template-columns: minmax(0, 1fr);
      gap: 4px;
    }

    .duration {
      margin-left: 0;
    }
  }
</style>
