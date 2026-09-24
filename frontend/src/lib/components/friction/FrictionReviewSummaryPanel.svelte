<script lang="ts">
  import { EmptyState, Spinner, Typeahead, type TypeaheadOption } from "@kenn-io/kit-ui";
  import { m } from "../../i18n/index.js";
  import { InsightsService, type DbInsight } from "../../api/generated/index";
  import { isAbortError } from "../../api/runtime.js";
  import { generateInsight, type GenerateInsightHandle } from "../../api/client.js";
  import { sync } from "../../stores/sync.svelte.js";
  import { insights } from "../../stores/insights.svelte.js";
  import { ui } from "../../stores/ui.svelte.js";
  import { renderMarkdown } from "../../utils/markdown.js";
  import type { AgentName } from "../../api/types.js";
  import { LatestRead } from "../../utils/latest-read.js";
  import { frictionReviewRequest, pickFrictionReviewInsight } from "./frictionReviewSummary.js";

  let { date }: { date: string } = $props();

  let summary: DbInsight | null = $state(null);
  let loading = $state(false);
  let generating = $state(false);
  let phase = $state("");
  let error: string | null = $state(null);

  let fetchVersion = 0;
  let genVersion = 0;
  let handle: GenerateInsightHandle | null = null;
  const listRead = new LatestRead();

  const generationAvailable = $derived(
    sync.serverVersion?.insight_generation_available ?? (sync.serverVersion?.read_only !== true),
  );
  const generationDisabled = $derived(sync.serverVersion === null || !generationAvailable);
  const disabledTitle = $derived(
    generationDisabled ? m.friction_review_summary_unavailable() : m.friction_review_summary_generate(),
  );
  const agentOptions: TypeaheadOption[] = [
    { name: "claude", label: "Claude", displayLabel: "Claude" },
    { name: "codex", label: "Codex", displayLabel: "Codex" },
    { name: "copilot", label: "Copilot", displayLabel: "Copilot" },
    { name: "gemini", label: "Gemini", displayLabel: "Gemini" },
    { name: "kiro", label: "Kiro", displayLabel: "Kiro" },
  ];

  function abortGeneration() {
    handle?.abort();
    handle = null;
    genVersion++;
  }

  $effect(() => {
    const requestedDate = date;
    const version = ++fetchVersion;
    const signal = listRead.begin();
    abortGeneration();
    error = null;
    generating = false;
    loading = true;

    InsightsService.getApiV1Insights(
      { type: "llm_canned", date_from: requestedDate, date_to: requestedDate },
      { signal },
    )
      .then((response) => {
        if (version !== fetchVersion || !listRead.isCurrent(signal)) return;
        summary = pickFrictionReviewInsight(response.insights, requestedDate);
        loading = false;
      })
      .catch((cause) => {
        if (isAbortError(cause) || version !== fetchVersion || !listRead.isCurrent(signal)) return;
        summary = null;
        loading = false;
        error = cause instanceof Error ? cause.message : m.friction_load_error();
      })
      .finally(() => listRead.finish(signal));

    return () => {
      listRead.cancel();
      abortGeneration();
    };
  });

  function start(forceRefresh: boolean) {
    if (generationDisabled || generating) return;
    generating = true;
    phase = "starting";
    error = null;

    const version = ++genVersion;
    const current = generateInsight(frictionReviewRequest(date, insights.agent, forceRefresh), (next) => {
      if (version === genVersion) phase = next;
    });
    handle = current;
    current.done
      .then((result) => {
        if (version !== genVersion) return;
        handle = null;
        summary = result;
        generating = false;
      })
      .catch((cause) => {
        if (version !== genVersion) return;
        handle = null;
        if (isAbortError(cause)) return;
        error = cause instanceof Error ? cause.message : m.activity_insight_generation_failed();
        generating = false;
      });
  }
</script>

<section class="friction-review-summary" aria-labelledby="friction-review-summary-title">
  <header class="panel-header">
    <span class="panel-title" id="friction-review-summary-title">
      {m.friction_review_summary_title()}
      {#if !loading && summary?.model}<span class="summary-model">{summary.model}</span>{/if}
    </span>
  </header>
  <p class="notice">{m.friction_review_summary_notice()}</p>

  {#snippet controls(label: string, forceRefresh: boolean)}
    <div class="gen-row">
      <div class="agent-typeahead">
        <Typeahead
          options={agentOptions}
          value={insights.agent}
          disabled={generationDisabled}
          title={m.activity_insight_agent_cli_title()}
          fallbackLabel={insights.agent}
          placeholder={m.activity_insight_insight_agent()}
          emptyLabel={m.activity_insight_no_matching_agents()}
          onselect={(value: string) => insights.setAgent(value as AgentName)}
        />
      </div>
      <button
        class="generate-btn"
        onclick={() => start(forceRefresh)}
        disabled={generationDisabled}
        title={disabledTitle}
      >
        {label}
      </button>
    </div>
  {/snippet}

  {#if loading}
    <div class="state muted">{m.activity_insight_loading()}</div>
  {:else if generating}
    <div class="state">
      <span aria-hidden="true"><Spinner size={12} /></span>
      <span>{m.friction_review_summary_generating({ phase })}</span>
    </div>
  {:else if error}
    <div class="state error">
      <span>{error}</span>
      {@render controls(m.activity_insight_retry(), false)}
    </div>
  {:else if summary}
    <article class="markdown-body">
      {@html renderMarkdown(summary.content, {
        renderUnknownXmlBlocksAsPreformatted: ui.renderUnknownXmlBlocksAsPreformatted,
      })}
    </article>
    {@render controls(m.friction_review_summary_regenerate(), true)}
  {:else}
    <EmptyState title={m.friction_review_summary_empty()}>
      {@render controls(m.friction_review_summary_generate(), false)}
    </EmptyState>
  {/if}
</section>

<style>
  .friction-review-summary {
    display: flex;
    flex-direction: column;
    gap: 8px;
    border-left: 2px dashed var(--border-muted);
    padding-left: 12px;
  }
  .panel-title {
    font-size: 11px;
    font-weight: 600;
    color: var(--text-muted);
    text-transform: uppercase;
    letter-spacing: 0.05em;
  }
  .summary-model {
    font-family: var(--font-mono);
    font-size: 10px;
    font-weight: 400;
    text-transform: none;
    letter-spacing: 0;
    opacity: 0.7;
    margin-left: 6px;
  }
  .notice {
    margin: 0;
    font-size: 12px;
    color: var(--text-muted);
  }
  .state {
    display: flex;
    align-items: center;
    gap: var(--space-4);
    font-size: 12px;
    color: var(--text-muted);
  }
  .state.error { color: var(--accent-red); }
  .gen-row {
    display: flex;
    align-items: center;
    gap: 8px;
  }
  .agent-typeahead {
    --typeahead-min-width: 96px;
    --typeahead-max-width: 112px;
    --typeahead-control-height: 28px;
    --typeahead-control-padding: 0 6px;
  }
  .generate-btn {
    display: inline-flex;
    align-items: center;
    gap: var(--space-2);
    height: 28px;
    padding: 0 12px;
    border-radius: var(--radius-sm);
    font-size: 11px;
    font-weight: 600;
    background: var(--accent-blue);
    color: var(--accent-blue-foreground);
    letter-spacing: 0.01em;
    transition: opacity 0.12s, transform 0.1s, box-shadow 0.12s;
    box-shadow: 0 1px 2px color-mix(in srgb, var(--accent-blue) 20%, transparent);
  }
  .generate-btn:hover:not(:disabled) {
    opacity: 0.92;
    box-shadow: 0 2px 6px color-mix(in srgb, var(--accent-blue) 30%, transparent);
  }
  .generate-btn:active:not(:disabled) {
    transform: var(--press-transform);
    box-shadow: none;
  }
  .generate-btn:disabled {
    opacity: var(--opacity-disabled);
    box-shadow: none;
    cursor: default;
  }
  .markdown-body {
    font-size: 14px;
    line-height: 1.7;
    color: var(--text-primary);
    max-width: 720px;
  }
  .markdown-body :global(p) { margin: 0 0 10px; }
  .markdown-body :global(ul), .markdown-body :global(ol) {
    margin: 0 0 10px;
    padding-left: 20px;
  }
  .markdown-body :global(a) { color: var(--accent-blue); }
</style>
