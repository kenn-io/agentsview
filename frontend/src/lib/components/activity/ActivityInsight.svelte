<script lang="ts">
  import { EmptyState, Spinner, Typeahead, type TypeaheadOption } from "@kenn-io/kit-ui";
  import { m } from "../../i18n/index.js";
  import { InsightsService } from "../../api/generated/index";
  import { configureGeneratedClient } from "../../api/runtime.js";
  import {
    generateInsight,
    type GenerateInsightHandle,
  } from "../../api/client.js";
  import { sync } from "../../stores/sync.svelte.js";
  import { insights } from "../../stores/insights.svelte.js";
  import { router, getBasePath } from "../../stores/router.svelte.js";
  import { renderMarkdown } from "../../utils/markdown.js";
  import { highlightCodeFences } from "../../utils/highlight-fences.js";
  import type { Insight, InsightsResponse, AgentName } from "../../api/types.js";
  import { LightbulbIcon, PlusIcon } from "../../icons.js";

  let {
    dateFrom,
    dateTo,
    timezone = "",
  }: { dateFrom: string; dateTo: string; timezone?: string } = $props();

  let insight: Insight | null = $state(null);
  let loading = $state(false);
  let generating = $state(false);
  let phase = $state("");
  let error: string | null = $state(null);

  // Guards stale fetch responses when the range changes mid-flight.
  let fetchVersion = 0;
  // Bumped on every generation start and on abort, so a generation that
  // settles after the range changed (or after a newer one started) is
  // ignored instead of clobbering the current handle or panel state.
  let genVersion = 0;
  // The in-flight generation, so we can abort it on range change/unmount.
  let handle: GenerateInsightHandle | null = null;

  /**
   * Open the standalone Insights page prefilled for this panel's range.
   * Modified or middle clicks fall through to the browser so the href opens in
   * a new tab/window; a plain left click is intercepted for SPA navigation.
   */
  function openInsightsPage(e: MouseEvent) {
    if (e.metaKey || e.ctrlKey || e.shiftKey || e.altKey || e.button !== 0) {
      return;
    }
    e.preventDefault();
    insights.setType("daily_activity");
    insights.setDateFrom(dateFrom);
    insights.setDateTo(dateTo);
    insights.setProject("");
    router.navigate("insights");
  }

  const insightGenerationAvailable = $derived(
    sync.serverVersion?.insight_generation_available === true ||
      sync.serverVersion?.read_only !== true,
  );
  const generationUnavailable = $derived(
    sync.serverVersion === null || !insightGenerationAvailable,
  );
  const unavailableTitle = $derived(
    sync.serverVersion !== null && !insightGenerationAvailable
      ? m.activity_insight_unavailable_read_only()
      : sync.serverVersion === null
        ? m.activity_insight_waiting_server()
        : m.activity_insight_generate_insight(),
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
    // Invalidate the aborted generation so its late settle is a no-op.
    genVersion++;
  }

  $effect(() => {
    // Read both bounds synchronously so the effect re-runs when either
    // changes, and capture them so the closures below stay stable.
    const from = dateFrom;
    const to = dateTo;
    const v = ++fetchVersion;
    abortGeneration();
    error = null;
    generating = false;
    loading = true;

    configureGeneratedClient();
    InsightsService.getApiV1Insights({
      type: "daily_activity",
      dateFrom: from,
      dateTo: to,
    })
      .then((res) => {
        if (v !== fetchVersion) return;
        // The list endpoint treats date_from/date_to as range BOUNDS, so a
        // multi-day range also returns narrower insights nested inside it
        // (e.g. a single day) and project-scoped ones. This panel shows the
        // global insight for the exact range, so match both bounds and drop
        // project-scoped rows before taking the newest.
        const list = (res as unknown as InsightsResponse).insights.filter(
          (i) => !i.project && i.date_from === from && i.date_to === to,
        );
        insight = list[0] ?? null;
        loading = false;
      })
      .catch(() => {
        if (v !== fetchVersion) return;
        insight = null;
        loading = false;
      });

    return abortGeneration;
  });

  // The agent choice is shared with the standalone Insights page via the
  // insights store, so picking one here and there stays in sync.
  function onAgentChange(value: string) {
    insights.setAgent(value as AgentName);
  }

  function handleGenerate() {
    if (generationUnavailable || generating) return;
    generating = true;
    phase = "starting";
    error = null;

    const v = ++genVersion;
    const current = generateInsight(
      {
        type: "daily_activity",
        date_from: dateFrom,
        date_to: dateTo,
        timezone,
        agent: insights.agent,
      },
      (p) => {
        if (v !== genVersion) return;
        phase = p;
      },
    );
    handle = current;

    current.done
      .then((result) => {
        if (v !== genVersion) return;
        handle = null;
        insight = result;
        generating = false;
      })
      .catch((e) => {
        if (v !== genVersion) return;
        handle = null;
        if (e instanceof DOMException && e.name === "AbortError") {
          return;
        }
        error = e instanceof Error ? e.message : m.activity_insight_generation_failed();
        generating = false;
      });
  }
</script>

<section class="activity-insight">
  <header class="panel-header">
    <span class="panel-title">
      <LightbulbIcon size="13" strokeWidth="1.8" aria-hidden="true" />
      <span>{m.activity_insight_title()}</span>
      {#if !loading && insight?.model}
        <span class="insight-model">{insight.model}</span>
      {/if}
    </span>
    <a
      class="insights-link"
      href={getBasePath() + "/insights"}
      onclick={openInsightsPage}
    >
      {m.activity_insight_open_insights_page()}
    </a>
  </header>

  {#snippet agentPicker()}
    <div class="agent-typeahead">
      <Typeahead
        options={agentOptions}
        value={insights.agent}
        disabled={generationUnavailable}
        title={m.activity_insight_agent_cli_title()}
        fallbackLabel={insights.agent}
        placeholder={m.activity_insight_insight_agent()}
        emptyLabel={m.activity_insight_no_matching_agents()}
        onselect={onAgentChange}
      />
    </div>
  {/snippet}

  {#if loading}
    <div class="state muted">{m.activity_insight_loading()}</div>
  {:else if generating}
    <div class="state generating">
      <span aria-hidden="true"><Spinner size={12} /></span>
      <span>{m.activity_insight_generating_phase({ phase })}</span>
    </div>
  {:else if error}
    <div class="state error">
      <span>{error}</span>
      <div class="gen-row">
        {@render agentPicker()}
        <button
          class="generate-btn"
          onclick={handleGenerate}
          disabled={generationUnavailable}
          title={unavailableTitle}
        >
          {m.activity_insight_retry()}
        </button>
      </div>
    </div>
  {:else if insight}
    <article
      class="markdown-body"
      use:highlightCodeFences={{ content: insight.content }}
    >
      {@html renderMarkdown(insight.content)}
    </article>
  {:else}
    <EmptyState title={m.activity_insight_empty_text()}>
      {@render agentPicker()}
      <button
        class="generate-btn"
        onclick={handleGenerate}
        disabled={generationUnavailable}
        title={unavailableTitle}
      >
        <PlusIcon size="12" strokeWidth="2.2" aria-hidden="true" />
        {m.activity_insight_generate()}
      </button>
    </EmptyState>
  {/if}
</section>

<style>
  .activity-insight {
    display: flex;
    flex-direction: column;
    gap: 12px;
  }

  .panel-header {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: 12px;
  }

  .panel-title {
    display: flex;
    align-items: center;
    gap: 6px;
    font-size: 11px;
    font-weight: 600;
    color: var(--text-muted);
    text-transform: uppercase;
    letter-spacing: 0.05em;
  }

  .insight-model {
    font-family: var(--font-mono);
    font-size: 10px;
    font-weight: 400;
    opacity: 0.7;
    text-transform: none;
    letter-spacing: 0;
  }

  .insights-link {
    font-size: 11px;
    font-weight: 600;
    color: var(--accent-blue);
    text-decoration: none;
    letter-spacing: 0.01em;
  }

  .insights-link:hover {
    text-decoration: underline;
  }

  .state {
    display: flex;
    align-items: center;
    gap: 10px;
    font-size: 12px;
    color: var(--text-muted);
  }

  .state.error {
    color: var(--accent-red);
  }

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
    gap: 5px;
    height: 28px;
    padding: 0 12px;
    border-radius: var(--radius-sm);
    font-size: 11px;
    font-weight: 600;
    background: var(--accent-blue);
    color: var(--accent-blue-foreground);
    letter-spacing: 0.01em;
    transition: opacity 0.12s, transform 0.1s, box-shadow 0.12s;
    box-shadow: 0 1px 2px rgba(37, 99, 235, 0.2);
  }

  .generate-btn:hover:not(:disabled) {
    opacity: 0.92;
    box-shadow: 0 2px 6px rgba(37, 99, 235, 0.3);
  }

  .generate-btn:active:not(:disabled) {
    transform: scale(0.98);
    box-shadow: none;
  }

  .generate-btn:disabled {
    opacity: 0.45;
    box-shadow: none;
    cursor: default;
  }

  /* ── Markdown Content ── */
  .markdown-body {
    font-size: 14px;
    line-height: 1.7;
    color: var(--text-primary);
    max-width: 720px;
  }

  .markdown-body :global(h1) {
    font-size: 20px;
    font-weight: 700;
    margin: 0 0 14px;
    padding-bottom: 8px;
    border-bottom: 1px solid var(--border-muted);
    letter-spacing: -0.02em;
  }

  .markdown-body :global(h2) {
    font-size: 16px;
    font-weight: 600;
    margin: 28px 0 10px;
    letter-spacing: -0.015em;
  }

  .markdown-body :global(h3) {
    font-size: 14px;
    font-weight: 600;
    margin: 20px 0 6px;
    letter-spacing: -0.01em;
  }

  .markdown-body :global(p) {
    margin: 0 0 10px;
  }

  .markdown-body :global(ul),
  .markdown-body :global(ol) {
    margin: 0 0 10px;
    padding-left: 20px;
  }

  .markdown-body :global(li) {
    margin: 3px 0;
  }

  .markdown-body :global(li + li) {
    margin-top: 4px;
  }

  .markdown-body :global(code) {
    font-family: var(--font-mono);
    font-size: 12px;
    padding: 2px 5px;
    background: var(--bg-inset);
    border-radius: var(--radius-sm);
  }

  .markdown-body :global(pre) {
    background: var(--bg-inset);
    padding: 10px 14px;
    border-radius: var(--radius-md);
    overflow-x: auto;
    margin: 0 0 10px;
    border: 1px solid var(--border-muted);
  }

  .markdown-body :global(pre code) {
    padding: 0;
    background: transparent;
    border: none;
  }

  .markdown-body :global(blockquote) {
    margin: 0 0 10px;
    padding: 6px 14px;
    border-left: 3px solid var(--accent-blue);
    color: var(--text-secondary);
    background: color-mix(
      in srgb,
      var(--accent-blue) 4%,
      transparent
    );
    border-radius: 0 var(--radius-sm) var(--radius-sm) 0;
  }

  .markdown-body :global(strong) {
    font-weight: 600;
    color: var(--text-primary);
  }

  .markdown-body :global(a) {
    color: var(--accent-blue);
    text-decoration: none;
  }

  .markdown-body :global(a:hover) {
    text-decoration: underline;
  }

  .markdown-body :global(hr) {
    border: none;
    border-top: 1px solid var(--border-muted);
    margin: 20px 0;
  }

  .markdown-body :global(table) {
    width: 100%;
    border-collapse: collapse;
    margin: 0 0 10px;
    font-size: 12px;
  }

  .markdown-body :global(th),
  .markdown-body :global(td) {
    padding: 6px 10px;
    border: 1px solid var(--border-muted);
    text-align: left;
  }

  .markdown-body :global(th) {
    background: var(--bg-inset);
    font-weight: 600;
  }
</style>
