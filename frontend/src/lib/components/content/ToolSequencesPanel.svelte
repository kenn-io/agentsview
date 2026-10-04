<script lang="ts">
  import { Button, Chip, type ChipTone } from "@kenn-io/kit-ui";
  import { SvelteSet } from "svelte/reactivity";
  import type {
    DbSessionTiming,
    SessionToolSequence,
    SessionToolSequenceCall,
    SessionToolSequencesResponse,
  } from "../../api/generated/index.js";
  import { getLocale, m } from "../../i18n/index.js";
  import { ChevronRightIcon, InfoIcon, WorkflowIcon } from "../../icons.js";
  import { router } from "../../stores/router.svelte.js";
  import { ui } from "../../stores/ui.svelte.js";
  import { formatDuration } from "../../utils/duration.js";
  import { summarizeToolInputPreview } from "../../utils/tool-summary.js";

  interface Props {
    data: SessionToolSequencesResponse | null;
    sessionId: string;
    loading: boolean;
    failed: boolean;
    unavailable?: boolean;
    onretry?: (() => void) | undefined;
    /** The session's live timing, the same snapshot the timing view shows. */
    timing?: DbSessionTiming | null;
    /** True only when data comes from the transcript revision the message list holds. */
    linked?: boolean;
  }

  let {
    data,
    sessionId,
    loading,
    failed,
    unavailable = false,
    onretry = undefined,
    timing = null,
    linked = false,
  }: Props = $props();

  type Outcome = SessionToolSequenceCall["outcome"];
  type Ending = SessionToolSequence["ending"];

  const uid = $props.id();
  const OUTCOMES: Outcome[] = ["errored", "empty", "content", "unknown"];
  const OUTCOME_TONES: Record<Outcome, string | undefined> = {
    errored: "danger",
    empty: "warning",
    content: "success",
    unknown: undefined,
  };
  const ENDING_TONES: Record<Ending, ChipTone> = {
    recovered: "success",
    abandoned: "danger",
    open: "warning",
    unknown: "neutral",
  };

  // Timing lists each turn's calls in call order, so a call's position in its message finds its duration even when tool IDs repeat or are blank.
  const timingByPosition = $derived(
    new Map(
      (timing?.turns ?? []).flatMap((turn) =>
        turn.calls.map((call, index) => [`${turn.ordinal}:${index}`, call] as const),
      ),
    ),
  );

  function durationOf(call: SessionToolSequenceCall): number | null {
    const timed = timingByPosition.get(`${call.ordinal}:${call.call_index}`);
    // Timing can lag or lead the sequences by a sync, so a call whose ID disagrees stays unmeasured.
    if (!timed || (timed.tool_use_id ?? "") !== (call.tool_use_id ?? "")) return null;
    return timed.duration_ms;
  }

  const openSequences = new SvelteSet<string>();
  const openCalls = new SvelteSet<string>();

  function countArgs(count: number) {
    return { count, countLabel: count.toLocaleString(getLocale()) };
  }

  function toggle(set: SvelteSet<string>, key: string) {
    if (set.has(key)) set.delete(key);
    else set.add(key);
  }

  function endingLabel(ending: Ending): string {
    switch (ending) {
      case "recovered": return m.tool_sequences_ending_recovered();
      case "abandoned": return m.tool_sequences_ending_abandoned();
      case "open": return m.tool_sequences_ending_open();
      case "unknown": return m.tool_sequences_ending_unknown();
    }
  }

  function endingExplanation(ending: Ending): string {
    switch (ending) {
      case "recovered": return m.tool_sequences_recovered_explanation();
      case "abandoned": return m.tool_sequences_abandoned_explanation();
      case "open": return m.tool_sequences_open_explanation();
      case "unknown": return m.tool_sequences_unknown_explanation();
    }
  }

  function outcomeLabel(outcome: Outcome): string {
    switch (outcome) {
      case "errored": return m.tool_sequences_outcome_errored();
      case "empty": return m.tool_sequences_outcome_empty();
      case "content": return m.tool_sequences_outcome_content();
      case "unknown": return m.tool_sequences_outcome_unknown();
    }
  }

  function callTag(call: SessionToolSequenceCall): { label: string; title?: string } | null {
    if (call.repeat === "identical") return { label: m.tool_sequences_repeat_identical() };
    if (call.repeat === "near_identical") {
      return {
        label: m.tool_sequences_repeat_near_identical(),
        title: m.tool_sequences_repeat_near_identical_title(),
      };
    }
    if (call.tool_changed) return { label: m.tool_sequences_tool_changed() };
    return null;
  }

  function sequenceFlags(sequence: SessionToolSequence): string[] {
    const flags: string[] = [];
    if (sequence.identical) flags.push(m.tool_sequences_flag_identical());
    if (sequence.near_identical) flags.push(m.tool_sequences_flag_near_identical());
    if (sequence.tool_changed) flags.push(m.tool_sequences_flag_tool_switch());
    return flags;
  }

  type Step = { tool: string; outcome: Outcome; count: number } | { more: number };

  // The server keeps the first calls and the last one, so omitted calls sit before the last shown call.
  function hiddenBefore(sequence: SessionToolSequence): number {
    return sequence.omitted_calls > 0 && sequence.calls.length > 1 ? sequence.calls.length - 1 : -1;
  }

  // Back-to-back calls with the same tool and outcome collapse into one step with a count.
  function steps(sequence: SessionToolSequence): Step[] {
    const out: Step[] = [];
    const gap = hiddenBefore(sequence);
    sequence.calls.forEach((call, index) => {
      if (index === gap) out.push({ more: sequence.omitted_calls });
      const last = out.at(-1);
      if (last && "tool" in last && last.tool === call.tool_name && last.outcome === call.outcome) last.count++;
      else out.push({ tool: call.tool_name, outcome: call.outcome, count: 1 });
    });
    if (sequence.omitted_calls > 0 && gap < 0) out.push({ more: sequence.omitted_calls });
    return out;
  }

  function messageRange(sequence: SessionToolSequence): string {
    const first = sequence.calls[0]?.ordinal;
    const last = sequence.calls.at(-1)?.ordinal;
    if (first === undefined || last === undefined) return "";
    if (first === last) return m.tool_sequences_message({ ordinal: first });
    return m.tool_sequences_messages_range({ first, last });
  }

  function resultSummary(call: SessionToolSequenceCall): string | null {
    return call.result_bytes === null ? null : m.tool_sequences_byte_count(countArgs(call.result_bytes));
  }

  function previewNote(total: number, omitted: number) {
    return m.tool_sequences_preview_shows({ ...countArgs(total), shownLabel: (total - omitted).toLocaleString(getLocale()) });
  }

  function jumpHref(call: SessionToolSequenceCall): string {
    // The revision rides along so a link opened in another tab is checked against the transcript it loads.
    const rev = data?.transcript_revision;
    return router.buildSessionHref(sessionId, { msg: String(call.ordinal), ...(rev ? { rev } : {}) });
  }

  function jumpToCall(event: MouseEvent, call: SessionToolSequenceCall) {
    if (event.button !== 0 || event.metaKey || event.ctrlKey || event.shiftKey || event.altKey) return;
    event.preventDefault();
    ui.scrollToOrdinal(call.ordinal, sessionId, data?.transcript_revision);
  }
</script>

{#snippet dot(outcome: Outcome)}
  <i class="dot" class:hollow={outcome === "unknown"} data-kit-tone={OUTCOME_TONES[outcome]} aria-hidden="true"></i>
{/snippet}

<section
  class="tool-sequences-panel"
  aria-labelledby="{uid}-title"
  aria-busy={loading}
>
  <header class="panel-head">
    <div class="panel-title-block">
      <div class="panel-heading">
        <WorkflowIcon size={14} aria-hidden="true" />
        <h2 id="{uid}-title">{m.tool_sequences_title()}</h2>
        {#if data && !failed && data.total_tool_calls > 0}
          <span class="count" title={m.tool_sequences_sequence_count(countArgs(data.total_sequences))}>
            <span aria-hidden="true">{data.total_sequences.toLocaleString(getLocale())}</span>
            <span class="kit-sr-only">{m.tool_sequences_sequence_count(countArgs(data.total_sequences))}</span>
          </span>
        {/if}
      </div>
      {#if loading && !data}
        <p class="panel-sub">{m.tool_sequences_loading()}</p>
      {:else if data && !failed && data.total_tool_calls > 0}
        <p class="panel-sub">
          {m.tool_sequences_sequence_calls(countArgs(data.total_sequence_calls))} ·
          {m.tool_sequences_session_calls(countArgs(data.total_tool_calls))}
        </p>
      {/if}
    </div>
    {#if data && !failed && data.sequences.length > 0}
      <ul class="legend" aria-label={m.tool_sequences_legend()}>
        {#each OUTCOMES as outcome (outcome)}
          <li>{@render dot(outcome)}{outcomeLabel(outcome)}</li>
        {/each}
      </ul>
    {/if}
  </header>

  {#if loading && !data}
    <div class="box skeleton" aria-hidden="true">
      <div class="skel"></div>
      <div class="skel"></div>
      <div class="skel"></div>
    </div>
  {:else if unavailable && !data}
    <p class="state">{m.tool_sequences_unavailable()}</p>
  {:else if failed && !data}
    <div class="state state-error" data-kit-tone="danger" role="alert">
      <span>{m.tool_sequences_error()}</span>
      {#if onretry}
        <Button size="sm" tone="neutral" surface="outline" label={m.tool_sequences_retry()} onclick={onretry} />
      {/if}
    </div>
  {:else if data}
    {#if data.total_tool_calls === 0}
      <p class="state">{m.tool_sequences_none_recorded()}</p>
    {:else if data.total_sequences === 0}
      <p class="state">{m.tool_sequences_no_sequences()}</p>
    {:else}
      <div class="box">
        {#each data.sequences as sequence, index (`${sessionId}-${index}`)}
          {@const key = `${sessionId}-${index}`}
          {@const open = openSequences.has(key)}
          {@const flags = sequenceFlags(sequence)}
          {@const gap = hiddenBefore(sequence)}
          <div class="sequence" class:open>
            <button
              type="button"
              class="sequence-row"
              aria-expanded={open}
              aria-controls={open ? `${uid}-seq-${index}` : undefined}
              onclick={() => toggle(openSequences, key)}
            >
              <ChevronRightIcon class="chev" size={12} aria-hidden="true" />
              <span class="flow">
                {#each steps(sequence) as step, stepIndex (stepIndex)}
                  {#if stepIndex > 0}<span class="arrow" aria-hidden="true">→</span>{/if}
                  {#if "more" in step}
                    <span class="step more">{m.tool_sequences_more_calls(countArgs(step.more))}</span>
                  {:else}
                    <span class="step">
                      {@render dot(step.outcome)}{step.tool}<span class="kit-sr-only">, {outcomeLabel(step.outcome)}</span>{#if step.count > 1}<span class="times">×{step.count}</span>{/if}
                    </span>
                  {/if}
                {/each}
              </span>
              {#if flags.length > 0}
                <span class="facts">{flags.join(" · ")}</span>
              {/if}
              <span class="where">{messageRange(sequence)}</span>
              <Chip size="xs" tone={ENDING_TONES[sequence.ending]} title={endingExplanation(sequence.ending)} class="ending">
                {endingLabel(sequence.ending)}
              </Chip>
            </button>
            {#if open}
              <div class="calls" id="{uid}-seq-{index}">
                {#each sequence.calls as call, callIndex (`${call.ordinal}-${call.call_index}-${call.tool_use_id}`)}
                  {@const callKey = `${key}-${call.ordinal}-${call.call_index}`}
                  {@const callOpen = openCalls.has(callKey)}
                  {@const tag = callTag(call)}
                  {@const size = resultSummary(call)}
                  {@const duration = durationOf(call)}
                  {#if callIndex === gap}
                    <p class="omit">
                      <InfoIcon size={12} aria-hidden="true" />
                      {m.tool_sequences_calls_hidden_between({
                        ...countArgs(sequence.omitted_calls),
                        from: sequence.calls[callIndex - 1]!.ordinal,
                        to: call.ordinal,
                      })}
                    </p>
                  {/if}
                  <div class="call" class:open={callOpen} data-kit-tone={OUTCOME_TONES[call.outcome]}>
                    <div class="call-line">
                      <button
                        type="button"
                        class="call-row"
                        aria-expanded={callOpen}
                        aria-controls={callOpen ? `${uid}-call-${index}-${callIndex}` : undefined}
                        onclick={() => toggle(openCalls, callKey)}
                      >
                        <ChevronRightIcon class="chev" size={12} aria-hidden="true" />
                        <span class="tool">{@render dot(call.outcome)}<span class="name" title={call.tool_name}>{call.tool_name}</span><span class="kit-sr-only">, {m.tool_sequences_message({ ordinal: call.ordinal })}</span></span>
                        <span class="input" title={call.input_preview}>
                          {call.input_preview ? summarizeToolInputPreview(call.input_preview) : m.tool_sequences_no_input()}
                          {#if tag}<span class="tag" title={tag.title}>{tag.label}</span>{/if}
                        </span>
                        <span class="res"><b>{outcomeLabel(call.outcome)}</b>{#if size}{` · ${size}`}{/if}</span>
                        {#if duration === null}
                          <span class="dur" title={m.tool_sequences_not_measured()}>
                            <span aria-hidden="true">—</span><span class="kit-sr-only">{m.tool_sequences_not_measured()}</span>
                          </span>
                        {:else}
                          <span class="dur">{formatDuration(duration)}</span>
                        {/if}
                      </button>
                      {#if linked}
                        <a
                          class="jump"
                          href={jumpHref(call)}
                          aria-label={m.tool_sequences_jump_label({ ordinal: call.ordinal, tool: call.tool_name })}
                          onclick={(event) => jumpToCall(event, call)}
                        >{m.tool_sequences_message({ ordinal: call.ordinal })}<span aria-hidden="true"> ↗</span></a>
                      {:else}
                        <!-- The transcript on screen is a different revision, so the ordinal could name another message. -->
                        <span class="jump unlinked">{m.tool_sequences_message({ ordinal: call.ordinal })}</span>
                      {/if}
                    </div>
                    {#if callOpen}
                      <div class="detail" id="{uid}-call-{index}-{callIndex}">
                        <dl class="evidence">
                          <!-- The call row drops its duration column on narrow panels, so the details carry it there. -->
                          <div class="ev ev-duration">
                            <dt>{m.tool_sequences_duration()}</dt>
                            <dd>{duration === null ? m.tool_sequences_not_measured() : formatDuration(duration)}</dd>
                          </div>
                          <div class="ev">
                            <dt>{m.tool_sequences_input()}</dt>
                            <dd>
                              {#if call.input_preview}
                                <!-- svelte-ignore a11y_no_noninteractive_tabindex (scrollable preview needs keyboard access) -->
                                <pre tabindex="0" role="region" aria-label={m.tool_sequences_input()}>{call.input_preview}</pre>
                              {:else}
                                <p class="none">{m.tool_sequences_no_input()}</p>
                              {/if}
                              {#if call.input_preview && call.input_omitted_bytes > 0}
                                <p class="note">
                                  {previewNote(call.input_bytes, call.input_omitted_bytes)}
                                  <span class="cut">{m.tool_sequences_full_input_in_message({ ordinal: call.ordinal })}</span>
                                </p>
                              {/if}
                            </dd>
                          </div>
                          <div class="ev">
                            <dt>{m.tool_sequences_result()}</dt>
                            <dd>
                              {#if call.result_content_unknown && call.outcome === "unknown"}
                                <p class="none">{m.tool_sequences_result_unknown()}</p>
                              {/if}
                              {#if call.result_preview}
                                <!-- svelte-ignore a11y_no_noninteractive_tabindex (scrollable preview needs keyboard access) -->
                                <pre tabindex="0" role="region" aria-label={m.tool_sequences_result()}>{call.result_preview}</pre>
                              {:else if call.result_bytes !== null && call.result_bytes > 0}
                                <p class="none">{m.tool_sequences_result_unavailable(countArgs(call.result_bytes))}</p>
                              {:else if call.result_bytes === 0}
                                <p class="none">{m.tool_sequences_result_empty()}</p>
                              {:else if !(call.result_content_unknown && call.outcome === "unknown")}
                                <p class="none">{m.tool_sequences_result_size_unknown()}</p>
                              {/if}
                              {#if call.result_preview && call.result_bytes !== null && call.result_omitted_bytes !== null && call.result_omitted_bytes > 0}
                                <p class="note">
                                  {previewNote(call.result_bytes, call.result_omitted_bytes)}
                                  <span class="cut">{m.tool_sequences_full_result_in_message({ ordinal: call.ordinal })}</span>
                                </p>
                              {/if}
                            </dd>
                          </div>
                        </dl>
                        <p class="idline">{call.tool_use_id || m.tool_sequences_missing_identity()}</p>
                      </div>
                    {/if}
                  </div>
                {/each}
                {#if sequence.omitted_calls > 0 && gap < 0}
                  <p class="omit">
                    <InfoIcon size={12} aria-hidden="true" />
                    {m.tool_sequences_calls_not_shown(countArgs(sequence.omitted_calls))}
                  </p>
                {/if}
                <p class="explain">
                  <b>{endingLabel(sequence.ending)}.</b>
                  {endingExplanation(sequence.ending)}
                  {#if sequence.total_calls > 1}{m.tool_sequences_calls_in_row(countArgs(sequence.total_calls))}{/if}
                </p>
              </div>
            {/if}
          </div>
        {/each}
      </div>
      {#if data.omitted_sequences > 0 || data.omitted_calls > 0}
        <p class="omit">
          <InfoIcon size={12} aria-hidden="true" />
          <span>
            {#if data.omitted_sequences > 0}
              {m.tool_sequences_showing_sequences({
                ...countArgs(data.total_sequences),
                shownLabel: data.sequences.length.toLocaleString(getLocale()),
              })}
            {/if}
            {#if data.omitted_calls > 0}
              {m.tool_sequences_calls_not_shown(countArgs(data.omitted_calls))}
            {/if}
          </span>
        </p>
      {/if}
    {/if}
  {/if}
</section>

<style>
  .tool-sequences-panel {
    container-type: inline-size;
    max-height: clamp(10rem, 34vh, 24rem);
    min-width: 0;
    overflow: auto;
    padding: 10px var(--space-5) var(--space-5);
    background: var(--bg-inset);
    border-bottom: 1px solid var(--border-default);
    color: var(--text-primary);
    font-size: var(--font-size-sm);
  }

  .panel-head {
    display: flex;
    align-items: center;
    justify-content: space-between;
    flex-wrap: wrap;
    gap: var(--space-2) var(--space-6);
    margin-bottom: var(--space-4);
  }

  .panel-heading {
    display: flex;
    align-items: center;
    gap: var(--space-4);
    color: var(--text-muted);
  }

  h2 {
    margin: 0;
    color: var(--text-primary);
    font-size: var(--font-size-md);
    font-weight: 600;
  }

  .count {
    padding: 0 5px;
    border: 1px solid var(--border-default);
    border-radius: var(--radius-sm);
    background: var(--bg-surface);
    color: var(--text-secondary);
    font-family: var(--font-mono);
    font-size: var(--font-size-2xs);
  }

  .panel-sub {
    margin: 2px 0 0;
    color: var(--text-muted);
    font-size: var(--font-size-xs);
    font-variant-numeric: tabular-nums;
  }

  .legend {
    display: inline-flex;
    flex-wrap: wrap;
    gap: var(--space-4);
    margin: 0;
    padding: 0;
    list-style: none;
    color: var(--text-muted);
    font-size: var(--font-size-xs);
  }

  .legend li {
    display: inline-flex;
    align-items: center;
    gap: var(--space-2);
  }

  .dot {
    display: inline-block;
    flex-shrink: 0;
    width: 6px;
    height: 6px;
    border-radius: 50%;
    background: var(--kit-tone, var(--text-muted));
  }

  .dot.hollow {
    width: 7px;
    height: 7px;
    border: 1.5px solid var(--text-muted);
    background: transparent;
  }

  .box {
    min-width: 0;
    overflow: hidden;
    border: 1px solid var(--border-default);
    border-radius: var(--radius-md);
    background: var(--bg-surface);
  }

  .sequence + .sequence {
    border-top: 1px solid var(--border-muted);
  }

  button {
    font: inherit;
    color: inherit;
    text-align: left;
    background: none;
    border: 0;
    cursor: pointer;
  }

  .sequence-row {
    display: flex;
    align-items: center;
    gap: var(--space-4);
    width: 100%;
    min-width: 0;
    padding: 7px 10px;
    font-size: var(--font-size-sm);
  }

  .sequence-row:hover {
    background: var(--bg-surface-hover);
  }

  .sequence-row:focus-visible,
  .call-row:focus-visible,
  .jump:focus-visible,
  pre:focus-visible {
    outline: var(--focus-ring);
    outline-offset: -2px;
    border-radius: var(--radius-sm);
  }

  :global(.tool-sequences-panel .chev) {
    flex-shrink: 0;
    color: var(--text-muted);
    transition: transform 0.18s ease;
  }

  .open > .sequence-row :global(.chev),
  .call.open .call-row :global(.chev) {
    transform: rotate(90deg);
  }

  .flow {
    display: flex;
    align-items: center;
    flex-wrap: wrap;
    flex: 1;
    gap: var(--space-2);
    min-width: 0;
    font-family: var(--font-mono);
    font-size: var(--font-size-xs);
  }

  .step {
    display: inline-flex;
    align-items: center;
    gap: var(--space-2);
    min-width: 0;
    padding: 1px var(--space-3);
    border: 1px solid var(--border-muted);
    border-radius: var(--radius-sm);
    background: var(--bg-primary);
    color: var(--text-secondary);
    overflow-wrap: anywhere;
  }

  .step .times {
    color: var(--text-muted);
    font-family: var(--font-sans);
  }

  .step.more {
    border-style: dashed;
    color: var(--text-muted);
    font-family: var(--font-sans);
  }

  .arrow {
    color: var(--text-muted);
    font-size: var(--font-size-2xs);
  }

  .facts,
  .where {
    color: var(--text-muted);
    font-size: var(--font-size-xs);
    font-variant-numeric: tabular-nums;
  }

  .facts {
    min-width: 0;
    overflow-wrap: anywhere;
  }

  .where {
    white-space: nowrap;
  }

  .calls {
    display: grid;
    gap: var(--space-1);
    padding: 0 10px var(--space-4) 30px;
  }

  .call {
    border: 1px solid var(--border-muted);
    border-radius: var(--radius-sm);
    background: var(--bg-primary);
    transition: border-color 0.15s, background 0.15s;
  }

  .call:hover {
    border-color: var(--border-default);
    background: var(--bg-surface-hover);
  }

  .call.open {
    border-color: var(--border-default);
    background: var(--bg-surface);
  }

  .call-line {
    display: grid;
    grid-template-columns: minmax(0, 1fr) auto;
    align-items: center;
    gap: var(--space-4);
    padding-right: var(--space-4);
  }

  .call-row {
    display: grid;
    grid-template-columns: 12px 6.5rem minmax(0, 1fr) auto 3.5em;
    align-items: center;
    gap: var(--space-4);
    min-width: 0;
    /* Right padding keeps the inset focus ring off the last column. */
    padding: var(--space-2) var(--space-3) var(--space-2) var(--space-4);
    font-size: var(--font-size-xs);
  }

  .tool {
    display: flex;
    align-items: center;
    gap: var(--space-3);
    min-width: 0;
    color: color-mix(in srgb, var(--accent-amber) 72%, var(--text-primary));
    font-family: var(--font-mono);
    font-weight: 500;
  }

  .name {
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  .input {
    overflow: hidden;
    color: var(--text-secondary);
    font-family: var(--font-mono);
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  .tag {
    display: inline-block;
    margin-left: var(--space-3);
    padding: 0 5px;
    border-radius: 3px;
    background: var(--bg-inset);
    color: var(--text-muted);
    font-family: var(--font-sans);
    font-size: var(--font-size-2xs);
  }

  .res {
    color: var(--text-muted);
    white-space: nowrap;
    font-variant-numeric: tabular-nums;
  }

  .res b {
    color: var(--kit-tone-ink, var(--text-secondary));
    font-weight: 600;
  }

  .dur {
    min-width: 3.5em;
    color: var(--text-muted);
    font-family: var(--font-mono);
    font-size: var(--font-size-2xs);
    text-align: right;
  }

  .jump {
    color: var(--accent-blue);
    font-size: var(--font-size-xs);
    text-decoration: none;
    white-space: nowrap;
  }

  a.jump:hover {
    text-decoration: underline;
  }

  .jump.unlinked {
    color: var(--text-muted);
  }

  .detail,
  .evidence {
    display: grid;
    gap: var(--space-3);
    margin: 0;
  }

  .detail {
    padding: var(--space-1) var(--space-4) var(--space-4) 28px;
  }

  .ev {
    display: grid;
    grid-template-columns: 3.5rem minmax(0, 1fr);
    gap: var(--space-4);
    font-size: var(--font-size-xs);
  }

  .ev-duration {
    display: none;
  }

  .ev-duration dd {
    padding-top: var(--space-2);
  }

  dt {
    padding-top: var(--space-2);
    color: var(--text-muted);
  }

  dd {
    min-width: 0;
    margin: 0;
  }

  pre {
    max-height: 9.5em;
    margin: 0;
    padding: 5px var(--space-4);
    overflow: auto;
    border: 1px solid var(--border-muted);
    border-radius: var(--radius-sm);
    background: var(--tool-bg);
    color: var(--text-primary);
    font: 11px/1.45 var(--font-mono);
    white-space: pre-wrap;
    overflow-wrap: anywhere;
  }

  .note {
    margin: 3px 0 0;
    color: var(--text-muted);
  }

  .cut {
    color: color-mix(in srgb, var(--accent-amber) 72%, var(--text-primary));
  }

  .none {
    margin: 0;
    padding-top: var(--space-2);
    color: var(--text-muted);
    font-style: italic;
  }

  .idline {
    margin: 0;
    color: var(--text-muted);
    font-family: var(--font-mono);
    font-size: var(--font-size-2xs);
    overflow-wrap: anywhere;
  }

  .explain {
    margin: 0;
    padding: var(--space-2) var(--space-1) 0;
    color: var(--text-muted);
    font-size: var(--font-size-xs);
  }

  .explain b {
    color: var(--text-secondary);
    font-weight: 600;
  }

  .omit {
    display: flex;
    align-items: center;
    gap: var(--space-3);
    margin: var(--space-4) 0 0;
    color: var(--text-muted);
    font-size: var(--font-size-xs);
  }

  .calls .omit {
    margin: var(--space-1) 0;
  }

  .state {
    margin: 0;
    padding: 14px var(--space-5);
    border: 1px dashed var(--border-default);
    border-radius: var(--radius-md);
    background: var(--bg-surface);
    color: var(--text-muted);
    font-size: var(--font-size-sm);
    text-align: center;
  }

  .state-error {
    display: flex;
    align-items: center;
    justify-content: center;
    flex-wrap: wrap;
    gap: var(--space-4);
    border-style: solid;
    border-color: var(--kit-tone-border);
    background: var(--kit-tone-band-bg);
    color: var(--kit-tone-ink);
  }

  .skel {
    height: 30px;
    background: linear-gradient(90deg, var(--bg-surface) 0%, var(--bg-surface-hover) 50%, var(--bg-surface) 100%);
    background-size: 200% 100%;
    animation: skeleton-sweep 1.2s linear infinite;
  }

  .skel + .skel {
    border-top: 2px solid var(--bg-inset);
  }

  @keyframes skeleton-sweep {
    to {
      background-position: -200% 0;
    }
  }

  @media (prefers-reduced-motion: reduce) {
    .skel {
      animation: none;
    }

    :global(.tool-sequences-panel .chev) {
      transition: none;
    }
  }

  @container (max-width: 820px) {
    .call-row {
      grid-template-columns: 12px 5.5rem minmax(0, 1fr) auto;
    }

    .dur {
      display: none;
    }

    .ev-duration {
      display: grid;
    }
  }

  @container (max-width: 520px) {
    .legend {
      display: none;
    }

    .sequence-row {
      flex-wrap: wrap;
      row-gap: var(--space-2);
    }

    .flow {
      flex-basis: calc(100% - 22px);
    }

    .facts,
    .where {
      margin-left: 22px;
    }

    .facts + .where {
      margin-left: 0;
    }

    .sequence-row :global(.ending) {
      margin-left: auto;
    }

    .calls {
      padding-left: 10px;
    }

    .call-line {
      align-items: start;
    }

    /* fit-content keeps a long MCP tool name from squeezing out the input. */
    .call-row {
      grid-template-columns: 12px fit-content(40%) minmax(0, 1fr);
      row-gap: var(--space-1);
    }

    .res {
      grid-column: 2 / 4;
      white-space: normal;
    }

    .jump {
      padding-top: var(--space-2);
    }

    .detail {
      padding-left: var(--space-4);
    }

    .ev {
      grid-template-columns: minmax(0, 1fr);
      gap: var(--space-1);
    }
  }
</style>
