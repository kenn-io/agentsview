<script lang="ts">
  import { Chip, type ChipTone } from "@kenn-io/kit-ui";
  import {
    SessionsService,
    type DbInsight,
    type DbSessionTiming,
    type ServiceSessionDetail,
    type SessionToolSequencesResponse,
  } from "../../api/generated/index";
  import { isAbortError } from "../../api/runtime.js";
  import {
    parseToolEffectivenessReport,
    type ToolEffectivenessAssessment,
    type ToolEffectivenessCitedCall,
    type ToolEffectivenessConclusion,
    type ToolEffectivenessOmission,
  } from "../../api/types/insights.js";
  import { getLocale, m } from "../../i18n/index.js";
  import {
    CheckIcon,
    ChevronRightIcon,
    CircleQuestionMarkIcon,
    InfoIcon,
    MessageSquareIcon,
    TriangleAlertIcon,
    XIcon,
  } from "../../icons.js";
  import { router } from "../../stores/router.svelte.js";
  import { ui } from "../../stores/ui.svelte.js";
  import { LatestRead } from "../../utils/latest-read.js";
  import { loadAssetImages, renderMarkdown } from "../../utils/markdown.js";
  import { normalizeMessagePreview } from "../../utils/messages.js";
  import { summarizeToolInputPreview } from "../../utils/tool-summary.js";
  import ToolCallRow from "../content/ToolCallRow.svelte";
  import ToolSequencesPanel from "../content/ToolSequencesPanel.svelte";

  interface Props {
    insight: DbInsight;
  }

  type Citation =
    | { kind: "call"; key: string; ordinal: number; callIndex: number; detail?: ToolEffectivenessCitedCall }
    | { kind: "message"; key: string; ordinal: number };

  const ASSESSMENTS: ToolEffectivenessAssessment[] = ["helped", "did_not_help", "unknown"];
  const ASSESSMENT_TONES: Record<ToolEffectivenessAssessment, ChipTone> = {
    helped: "success",
    did_not_help: "danger",
    unknown: "neutral",
  };
  const ASSESSMENT_ICONS = {
    helped: CheckIcon,
    did_not_help: XIcon,
    unknown: CircleQuestionMarkIcon,
  } as const;

  let { insight }: Props = $props();
  const uid = $props.id();

  const report = $derived(parseToolEffectivenessReport(insight));
  const sessionId = $derived(report?.session_id ?? "");

  let facts = $state<SessionToolSequencesResponse | null>(null);
  let session = $state<ServiceSessionDetail | null>(null);
  let timing = $state<DbSessionTiming | null>(null);
  let factsLoading = $state(false);
  let factsFailed = $state(false);
  let factsAttempt = $state(0);
  // Per conclusion: whether its evidence is open and which citation is highlighted.
  let openConclusions = $state<Record<number, boolean>>({});
  let activeCitation = $state<Record<number, string | null>>({});
  const factsRead = new LatestRead();
  const sessionRead = new LatestRead();
  const timingRead = new LatestRead();

  $effect(() => {
    const id = sessionId;
    // A new report for the same session starts fresh too.
    void insight.id;
    void factsAttempt;
    facts = null;
    session = null;
    timing = null;
    factsFailed = false;
    openConclusions = {};
    activeCitation = {};
    if (!id) {
      factsRead.cancel();
      sessionRead.cancel();
      timingRead.cancel();
      factsLoading = false;
      return;
    }
    factsLoading = true;
    const signal = factsRead.begin();
    // The revision is read after the sequences, so a change between the two reads still shows as stale.
    const readSession = () => {
      const sessionSignal = sessionRead.begin();
      SessionsService.getApiV1SessionsById({ id }, { signal: sessionSignal })
        .then((detail) => {
          if (sessionRead.finish(sessionSignal)) session = detail;
        })
        .catch(() => {
          // Without the session only the call-count check and report fields apply.
          sessionRead.finish(sessionSignal);
        });
      // Durations come from the session's timing, as the session page's panel shows them.
      const timingSignal = timingRead.begin();
      SessionsService.getApiV1SessionsByIdTiming({ id }, { signal: timingSignal })
        .then((detail) => {
          if (timingRead.finish(timingSignal)) timing = detail;
        })
        .catch(() => {
          // Without timing every call reads as not measured.
          timingRead.finish(timingSignal);
        });
    };
    SessionsService.getApiV1SessionsByIdToolSequences({ id }, { signal })
      .then((response) => {
        if (factsRead.isCurrent(signal)) facts = response;
      })
      .catch((error) => {
        if (isAbortError(error) || !factsRead.isCurrent(signal)) return;
        factsFailed = true;
      })
      .finally(() => {
        if (!factsRead.finish(signal)) return;
        factsLoading = false;
        readSession();
      });
    return () => {
      factsRead.cancel();
      sessionRead.cancel();
      timingRead.cancel();
    };
  });

  const callDetails = $derived.by(() => {
    const byKey = new Map<string, ToolEffectivenessCitedCall>();
    const byOrdinal = new Map<number, ToolEffectivenessCitedCall[]>();
    for (const call of report?.cited_calls ?? []) {
      byKey.set(`${call.ordinal}:${call.call_index}`, call);
      byOrdinal.set(call.ordinal, [...(byOrdinal.get(call.ordinal) ?? []), call]);
    }
    return { byKey, byOrdinal };
  });

  function callCitation(ordinal: number, callIndex: number): Citation {
    const key = `${ordinal}:${callIndex}`;
    return { kind: "call", key, ordinal, callIndex, detail: callDetails.byKey.get(key) };
  }

  /** Orders a conclusion's citations by its ordinals, folding in the calls it names. */
  function citationsOf(conclusion: ToolEffectivenessConclusion): Citation[] {
    const named = new Map<number, number[]>();
    for (const call of conclusion.calls) {
      named.set(call.ordinal, [...(named.get(call.ordinal) ?? []), call.call_index]);
    }
    const out: Citation[] = [];
    const placed = new Set<number>();
    for (const ordinal of conclusion.ordinals) {
      if (placed.has(ordinal)) continue;
      placed.add(ordinal);
      const calls = named.get(ordinal);
      const only = callDetails.byOrdinal.get(ordinal);
      if (calls) out.push(...calls.map((index) => callCitation(ordinal, index)));
      else if (only?.length === 1 && only[0]!.message_calls === 1) out.push(callCitation(ordinal, only[0]!.call_index));
      else out.push({ kind: "message", key: `m${ordinal}`, ordinal });
    }
    for (const [ordinal, calls] of named) {
      if (!placed.has(ordinal)) out.push(...calls.map((index) => callCitation(ordinal, index)));
    }
    return out;
  }

  const conclusions = $derived(
    (report?.conclusions ?? []).map((conclusion) => ({ conclusion, citations: citationsOf(conclusion) })),
  );

  const counts = $derived.by(() => {
    const out: Record<ToolEffectivenessAssessment, number> = { helped: 0, did_not_help: 0, unknown: 0 };
    for (const { conclusion } of conclusions) out[conclusion.assessment]++;
    return out;
  });

  const citedCallCount = $derived(
    new Set(conclusions.flatMap(({ citations }) => citations.filter((c) => c.kind === "call").map((c) => c.key))).size,
  );

  const sessionTitle = $derived(
    session
      ? session.display_name || normalizeMessagePreview(session.first_message) || session.project
      : "",
  );

  const staleText = $derived.by(() => {
    if (!report) return null;
    if (facts && facts.total_tool_calls !== report.call_count) {
      return m.tool_effectiveness_session_changed({
        ...countArgs(report.call_count),
        currentLabel: formatCount(facts.total_tool_calls),
      });
    }
    const current = session?.transcript_revision;
    if (report.transcript_revision && session && (current ?? "") !== report.transcript_revision) {
      return m.tool_effectiveness_session_revised();
    }
    // The status decides whether a trailing sequence reads as open or abandoned.
    if (report.termination_status !== undefined && session && (session.termination_status ?? "") !== report.termination_status) {
      return m.tool_effectiveness_session_revised();
    }
    return null;
  });

  function formatCount(value: number): string {
    return value.toLocaleString(getLocale());
  }

  function countArgs(count: number) {
    return { count, countLabel: formatCount(count) };
  }

  function assessmentLabel(assessment: ToolEffectivenessAssessment): string {
    switch (assessment) {
      case "helped": return m.tool_effectiveness_assessment_helped();
      case "did_not_help": return m.tool_effectiveness_assessment_did_not_help();
      case "unknown": return m.tool_effectiveness_assessment_unknown();
    }
  }

  function assessmentCount(assessment: ToolEffectivenessAssessment, count: number): string {
    const args = countArgs(count);
    switch (assessment) {
      case "helped": return m.tool_effectiveness_count_helped(args);
      case "did_not_help": return m.tool_effectiveness_count_did_not_help(args);
      case "unknown": return m.tool_effectiveness_count_unknown(args);
    }
  }

  /** What the model saw of a cited call's result when the budget cut it. */
  function modelSaw(citation: Citation): string | undefined {
    if (citation.kind !== "call") return undefined;
    const cut = report?.omissions.find((o) =>
      o.reason === "budget" && o.field === "result" &&
      o.ordinal === citation.ordinal && o.call_index === citation.callIndex);
    if (cut?.kept_bytes === undefined) return undefined;
    return m.tool_effectiveness_model_saw({ kept: m.tool_sequences_byte_count(countArgs(cut.kept_bytes)) });
  }

  function citationTitle(citation: Citation): string {
    if (citation.kind === "message") return m.tool_sequences_message({ ordinal: citation.ordinal });
    if (!citation.detail) return callFallback(citation);
    return `${citation.detail.tool_name} ${citation.detail.input_preview} · ${citationPlace(citation)}`;
  }

  /** Names the call inside its message when the message holds several. */
  function citationPlace(citation: Citation & { kind: "call" }): string {
    return sharesMessage(citation) ? callFallback(citation) : m.tool_sequences_message({ ordinal: citation.ordinal });
  }

  function sharesMessage(citation: Citation & { kind: "call" }): boolean {
    return (citation.detail?.message_calls ?? 1) > 1;
  }

  function inputLabel(preview: string): string {
    return preview ? summarizeToolInputPreview(preview) : m.tool_sequences_no_input();
  }

  function callFallback(citation: Citation & { kind: "call" }): string {
    return m.tool_sequences_message_call({ ordinal: citation.ordinal, callIndex: citation.callIndex });
  }

  function omissionText(omission: ToolEffectivenessOmission): string {
    const where = {
      ordinal: String(omission.ordinal ?? ""),
      callIndex: String(omission.call_index ?? ""),
      tool: omission.tool_name ?? "",
    };
    switch (omission.reason) {
      case "budget":
        return m.tool_effectiveness_omission_budget({
          ...where,
          field: omission.field === "input" ? m.tool_sequences_input() : m.tool_sequences_result(),
          keptLabel: formatCount(omission.kept_bytes ?? 0),
          ...countArgs(omission.original_bytes ?? 0),
        });
      case "unretained":
        return omission.original_bytes === undefined
          ? m.tool_effectiveness_omission_unretained(where)
          : m.tool_effectiveness_omission_unretained_bytes({
            ...where,
            ...countArgs(omission.original_bytes),
          });
      case "previews": {
        const count = omission.count ?? 0;
        return m.tool_effectiveness_omission_previews({ count, countLabel: formatCount(count) });
      }
    }
  }

  function selectCitation(index: number, key: string) {
    if (activeCitation[index] === key && openConclusions[index]) {
      activeCitation[index] = null;
      openConclusions[index] = false;
      return;
    }
    activeCitation[index] = key;
    openConclusions[index] = true;
  }

  function toggleConclusion(index: number) {
    activeCitation[index] = null;
    openConclusions[index] = !openConclusions[index];
  }

  function openSession(event: MouseEvent) {
    if (event.button !== 0 || event.metaKey || event.ctrlKey || event.shiftKey || event.altKey) return;
    event.preventDefault();
    router.navigateToSession(sessionId);
  }
</script>

{#if report}
  <div class="tool-effectiveness-report">
    <div class="report-head">
      {#if session}
        <div class="report-title">
          <h3>{sessionTitle}</h3>
          <p>
            <a href={router.buildSessionHref(sessionId)} onclick={openSession}>{m.tool_effectiveness_open_session()}</a>
            · {session.agent_label || session.agent} · {session.project}
            · {m.tool_effectiveness_message_count(countArgs(session.message_count))}
          </p>
        </div>
      {/if}

      {#if staleText}
        <p class="stale" data-kit-tone="warning" data-testid="tool-effectiveness-session-changed">
          <TriangleAlertIcon size={14} aria-hidden="true" />
          <span>{staleText}</span>
        </p>
      {/if}

      <div class="summary" data-testid="tool-effectiveness-summary">
        <div class="summary-row">
          <span class="summary-label">{m.tool_effectiveness_conclusion_count(countArgs(conclusions.length))}</span>
          {#each ASSESSMENTS as assessment (assessment)}
            {@render verdict(assessment, assessmentCount(assessment, counts[assessment]))}
          {/each}
          <span class="summary-note">
            {m.tool_effectiveness_calls_cited({ ...countArgs(report.call_count), citedLabel: formatCount(citedCallCount) })}
          </span>
        </div>
        <div class="bar" aria-hidden="true">
          {#each ASSESSMENTS as assessment (assessment)}
            {#if counts[assessment] > 0}
              <span data-kit-tone={ASSESSMENT_TONES[assessment] === "neutral" ? undefined : ASSESSMENT_TONES[assessment]} style:flex={counts[assessment]}></span>
            {/if}
          {/each}
        </div>
      </div>
    </div>

    <section class="report-section" aria-labelledby="tool-effectiveness-assessment-title">
      <div class="section-head">
        <h2 id="tool-effectiveness-assessment-title">{m.tool_effectiveness_model_assessment()}</h2>
        <span class="hint">{m.tool_effectiveness_select_citation()}</span>
      </div>
      <ol class="conclusions">
        {#each conclusions as { conclusion, citations }, index (index)}
          {@const open = openConclusions[index] === true}
          <li class="conclusion" class:open>
            {@render verdict(conclusion.assessment, assessmentLabel(conclusion.assessment))}
            <div class="conclusion-body">
              <p>{conclusion.text}</p>
              <div class="citations">
                {#each citations as citation (citation.key)}
                  <button
                    type="button"
                    class="citation"
                    class:message={citation.kind === "message"}
                    class:active={open && activeCitation[index] === citation.key}
                    aria-pressed={open && activeCitation[index] === citation.key}
                    aria-controls={open ? `${uid}-evidence-${index}` : undefined}
                    title={citationTitle(citation)}
                    onclick={() => selectCitation(index, citation.key)}
                  >
                    {#if citation.kind === "message"}
                      <MessageSquareIcon size={12} aria-hidden="true" />{m.tool_sequences_message({ ordinal: citation.ordinal })}
                    {:else if citation.detail}
                      <span class="citation-tool">{citation.detail.tool_name}</span>
                      <span class="citation-input">{inputLabel(citation.detail.input_preview)}</span>
                      <span class="citation-at"><span aria-hidden="true">{"· "}</span>{citationPlace(citation)}</span>
                    {:else}
                      {callFallback(citation)}
                    {/if}
                    <span class="citation-arrow" aria-hidden="true">↗</span>
                  </button>
                {/each}
                <button
                  type="button"
                  class="citation-toggle"
                  aria-expanded={open}
                  aria-controls={open ? `${uid}-evidence-${index}` : undefined}
                  onclick={() => toggleConclusion(index)}
                >
                  <ChevronRightIcon class="chev" size={12} aria-hidden="true" />
                  {m.tool_effectiveness_citation_count(countArgs(citations.length))}
                </button>
              </div>
              {#if open}
                <div class="evidence" id="{uid}-evidence-{index}">
                  {#each citations as citation (citation.key)}
                    {@const highlighted = activeCitation[index] === citation.key}
                    {#if citation.kind === "message"}
                      <ToolCallRow
                        {sessionId}
                        ordinal={citation.ordinal}
                        message
                        tool={m.tool_effectiveness_message_label()}
                        input={m.tool_effectiveness_message_no_call()}
                        {highlighted}
                      />
                    {:else if citation.detail}
                      <ToolCallRow
                        {sessionId}
                        ordinal={citation.ordinal}
                        tool={citation.detail.tool_name}
                        input={inputLabel(citation.detail.input_preview)}
                        inputTitle={citation.detail.input_preview}
                        callIndex={sharesMessage(citation) ? citation.callIndex : undefined}
                        outcome={citation.detail.outcome}
                        resultBytes={citation.detail.result_bytes}
                        cut={modelSaw(citation)}
                        {highlighted}
                      />
                    {:else}
                      <ToolCallRow
                        {sessionId}
                        ordinal={citation.ordinal}
                        tool=""
                        input={callFallback(citation)}
                        cut={modelSaw(citation)}
                        {highlighted}
                      />
                    {/if}
                  {/each}
                </div>
              {/if}
            </div>
          </li>
        {/each}
      </ol>
    </section>

    <ToolSequencesPanel
      data={facts}
      {sessionId}
      loading={factsLoading}
      failed={factsFailed}
      onretry={() => factsAttempt++}
      {timing}
      embedded
    />

    <div class="omissions" data-testid="tool-effectiveness-omissions">
      <InfoIcon size={13} aria-hidden="true" />
      {#if report.omissions.length === 0}
        <span>{m.tool_effectiveness_no_omissions()}</span>
      {:else}
        <b>{m.tool_effectiveness_left_out()}</b>
        {#each report.omissions as omission, index (index)}
          <span class="omission">{omissionText(omission)}</span>
        {/each}
      {/if}
    </div>
  </div>
{:else}
  <p class="report-note" data-testid="tool-effectiveness-unavailable">
    {m.tool_effectiveness_details_unavailable()}
  </p>
  <div class="markdown-body" use:loadAssetImages={insight.content}>
    {@html renderMarkdown(insight.content, {
      renderUnknownXmlBlocksAsPreformatted: ui.renderUnknownXmlBlocksAsPreformatted,
    })}
  </div>
{/if}

{#snippet verdict(assessment: ToolEffectivenessAssessment, label: string)}
  {@const Icon = ASSESSMENT_ICONS[assessment]}
  <Chip size="sm" tone={ASSESSMENT_TONES[assessment]} uppercase={false} class="verdict">
    <Icon size={11} strokeWidth={3} aria-hidden="true" />
    {label}
  </Chip>
{/snippet}

<style>
  /* The report sits in a detail pane, so its layout follows the pane width. */
  .tool-effectiveness-report {
    container-type: inline-size;
    display: flex;
    flex-direction: column;
    gap: var(--space-6);
    min-width: 0;
    font-size: var(--font-size-md);
  }

  .report-head {
    display: flex;
    flex-direction: column;
    gap: var(--space-5);
    padding-bottom: var(--space-6);
    border-bottom: 1px solid var(--border-muted);
  }

  .report-title h3 {
    margin: 0;
    color: var(--text-primary);
    font-size: var(--font-size-lg);
    font-weight: 600;
  }

  .report-title p {
    margin: 2px 0 0;
    color: var(--text-muted);
    font-size: var(--font-size-sm);
  }

  a {
    color: var(--accent-blue);
    text-decoration: none;
  }

  a:hover {
    text-decoration: underline;
  }

  .stale {
    display: flex;
    align-items: center;
    gap: var(--space-4);
    margin: 0;
    padding: var(--space-3) 10px;
    border: 1px solid var(--kit-tone-border);
    border-radius: var(--radius-md);
    background: var(--kit-tone-band-bg);
    color: var(--kit-tone-ink);
    font-size: var(--font-size-sm);
  }

  .summary {
    display: flex;
    flex-direction: column;
    gap: var(--space-4);
  }

  .summary-row {
    display: flex;
    align-items: center;
    flex-wrap: wrap;
    gap: var(--space-3);
  }

  .summary-label {
    margin-right: 2px;
    color: var(--text-secondary);
    font-size: var(--font-size-sm);
  }

  .summary-note {
    margin-left: auto;
    color: var(--text-muted);
    font-size: var(--font-size-sm);
    font-variant-numeric: tabular-nums;
  }

  .bar {
    display: flex;
    gap: var(--space-1);
    height: 4px;
    overflow: hidden;
    border-radius: 999px;
  }

  .bar span {
    background: color-mix(in srgb, var(--kit-tone, var(--text-muted)) 70%, var(--bg-surface));
  }

  .report-section {
    display: flex;
    flex-direction: column;
    gap: var(--space-4);
    min-width: 0;
  }

  .section-head {
    display: flex;
    align-items: baseline;
    justify-content: space-between;
    flex-wrap: wrap;
    gap: var(--space-5);
  }

  .section-head h2 {
    margin: 0;
    color: var(--text-primary);
    font-size: var(--font-size-md);
    font-weight: 600;
  }

  .hint {
    color: var(--text-muted);
    font-size: var(--font-size-sm);
  }

  .conclusions {
    margin: 0;
    padding: 0;
    list-style: none;
    border: 1px solid var(--border-default);
    border-radius: var(--radius-md);
  }

  .conclusion {
    display: grid;
    grid-template-columns: 104px minmax(0, 1fr);
    gap: var(--space-5);
    padding: var(--space-5);
  }

  .conclusion + .conclusion {
    border-top: 1px solid var(--border-muted);
  }

  .conclusion > :global(.verdict) {
    align-self: start;
    justify-self: start;
    margin-top: 1px;
  }

  .conclusion-body {
    min-width: 0;
  }

  .conclusion-body p {
    max-width: 76ch;
    margin: 0;
    color: var(--text-primary);
    line-height: 1.6;
    overflow-wrap: anywhere;
  }

  .citations {
    display: flex;
    align-items: center;
    flex-wrap: wrap;
    gap: var(--space-3);
    margin-top: var(--space-4);
  }

  .citation {
    display: inline-flex;
    align-items: center;
    gap: var(--space-3);
    max-width: 100%;
    min-width: 0;
    padding: var(--space-2) var(--space-4);
    border: 1px solid var(--border-muted);
    border-radius: var(--radius-sm);
    background: var(--tool-bg);
    color: var(--text-secondary);
    font: inherit;
    font-size: var(--font-size-xs);
    line-height: 14px;
    cursor: pointer;
    transition: background 0.1s, border-color 0.1s;
  }

  .citation:hover {
    border-color: var(--border-default);
    background: var(--bg-surface-hover);
  }

  .citation.active {
    border-color: color-mix(in srgb, var(--accent-amber) 55%, var(--border-default));
  }

  .citation.message {
    background: var(--bg-inset);
  }

  .citation-tool {
    min-width: 0;
    max-width: 40%;
    overflow: hidden;
    color: color-mix(in srgb, var(--accent-amber) 72%, var(--text-primary));
    font-family: var(--font-mono);
    font-weight: 500;
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  .citation-input {
    flex: 1 1 auto;
    min-width: 6ch;
    max-width: 220px;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  .citation-at {
    color: var(--text-muted);
    white-space: nowrap;
  }

  .citation-toggle {
    display: inline-flex;
    align-items: center;
    gap: var(--space-2);
    padding: 2px var(--space-2);
    border: 0;
    border-radius: var(--radius-sm);
    background: none;
    color: var(--text-muted);
    font: inherit;
    font-size: var(--font-size-xs);
    cursor: pointer;
  }

  .citation-toggle:hover {
    color: var(--text-primary);
  }

  .citation-toggle :global(.chev) {
    color: var(--text-muted);
    transition: transform 0.18s ease;
  }

  .open .citation-toggle :global(.chev) {
    transform: rotate(90deg);
  }

  @media (prefers-reduced-motion: reduce) {
    .citation-toggle :global(.chev) {
      transition: none;
    }
  }

  .evidence {
    display: grid;
    grid-template-columns: minmax(0, 1fr);
    gap: var(--space-1);
    padding-top: var(--space-4);
  }

  .omissions {
    display: flex;
    align-items: center;
    flex-wrap: wrap;
    gap: var(--space-2) var(--space-4);
    padding: var(--space-4) var(--space-5);
    border: 1px solid var(--border-muted);
    border-radius: var(--radius-md);
    background: var(--bg-primary);
    color: var(--text-muted);
    font-size: var(--font-size-sm);
  }

  .omissions b {
    color: var(--text-secondary);
    font-weight: 600;
  }

  .report-note {
    margin: 0;
    color: var(--text-muted);
    font-size: 0.8rem;
  }

  @container (max-width: 600px) {
    .conclusion {
      grid-template-columns: minmax(0, 1fr);
      gap: var(--space-3);
    }

    .summary-note {
      width: 100%;
      margin-left: 0;
    }
  }
</style>
