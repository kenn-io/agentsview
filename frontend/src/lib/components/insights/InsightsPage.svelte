<script lang="ts">
  import { onMount, onDestroy, untrack } from "svelte";
  import { analytics } from "../../stores/analytics.svelte.js";
  import { insights } from "../../stores/insights.svelte.js";
  import { getBasePath, router } from "../../stores/router.svelte.js";
  import { sessions } from "../../stores/sessions.svelte.js";
  import { sync } from "../../stores/sync.svelte.js";
  import { events } from "../../stores/events.svelte.js";
  import { copyToClipboard } from "../../utils/clipboard.js";
  import { renderMarkdown } from "../../utils/markdown.js";
  import { scoreToGrade } from "../../utils/grade.js";
  import type {
    AgentName,
    CannedInsightKind,
    InsightType,
  } from "../../api/types.js";
  import DateRangeSelector from "../shared/DateRangeSelector.svelte";
  import CopyButton from "../shared/CopyButton.svelte";
  import ProjectTypeahead from "../layout/ProjectTypeahead.svelte";
  import {
    buildQualityPatterns,
    buildQualitySummary,
    buildRuleBasedRecommendations,
    type QualityPatternSeverity,
    type QualityPatternView,
  } from "./qualityPatterns.js";

  const REFRESH_INTERVAL_MS = 5 * 60 * 1000;

  let refreshTimer: ReturnType<typeof setInterval> | undefined;
  let unsubEvents: (() => void) | undefined;
  let copiedInsightLinkId: number | null = $state(null);
  let copiedInsightLinkTimer:
    | ReturnType<typeof setTimeout>
    | undefined;

  const signals = $derived(analytics.signals);
  const summary = $derived(buildQualitySummary(signals));
  const patterns = $derived(buildQualityPatterns(signals));
  const recommendations = $derived(
    buildRuleBasedRecommendations(patterns),
  );
  const loading = $derived(analytics.loading.signals);
  const error = $derived(analytics.errors.signals);
  const readOnly = $derived(
    sync.serverVersion?.read_only === true,
  );
  const generationUnavailable = $derived(
    sync.serverVersion === null || readOnly,
  );
  const hasData = $derived(
    summary.totalSessions > 0 || summary.computedQualitySessions > 0,
  );
  const maxGradeCount = $derived(
    Math.max(
      1,
      ...summary.scoreDistribution.map((bucket) => bucket.count),
    ),
  );

  function fetchInsightSignals() {
    analytics.fetchSignalsForInsights();
  }

  function handleProjectChange(value: string) {
    analytics.project = value;
    fetchInsightSignals();
  }

  function handleAgentChange(e: Event) {
    const select = e.target as HTMLSelectElement;
    analytics.agent = select.value;
    fetchInsightSignals();
  }

  function handleInsightAgentChange(e: Event) {
    const select = e.target as HTMLSelectElement;
    insights.setAgent(select.value as AgentName);
  }

  function handleCannedKindChange(e: Event) {
    const select = e.target as HTMLSelectElement;
    insights.setCannedKind(select.value as CannedInsightKind);
  }

  function handlePromptChange(e: Event) {
    const textarea = e.target as HTMLTextAreaElement;
    insights.promptText = textarea.value;
  }

  function handleGenerateCanned() {
    if (generationUnavailable) return;
    insights.setType("llm_canned");
    insights.setDateFrom(analytics.from);
    insights.setDateTo(analytics.to);
    insights.setProject(analytics.project);
    insights.generate();
  }

  function handleRefresh() {
    fetchInsightSignals();
    insights.load();
  }

  function insightLinkPath(id: number): string {
    const params = new URLSearchParams();
    if (router.params.desktop) {
      params.set("desktop", router.params.desktop);
    }
    params.set("insight", String(id));
    return `${getBasePath()}/insights?${params.toString()}`;
  }

  function insightLinkUrl(id: number): string {
    return new URL(
      insightLinkPath(id),
      window.location.origin,
    ).toString();
  }

  async function handleCopyInsightLink(id: number) {
    const ok = await copyToClipboard(insightLinkUrl(id));
    if (!ok) return;
    copiedInsightLinkId = id;
    clearTimeout(copiedInsightLinkTimer);
    copiedInsightLinkTimer = setTimeout(() => {
      copiedInsightLinkId = null;
    }, 1500);
  }

  function selectGeneratedInsight(id: number) {
    insights.select(id);
    router.replaceParams({ insight: String(id) });
  }

  function selectGeneratedTask(clientId: string) {
    insights.selectTask(clientId);
    router.replaceParams({});
  }

  function selectedInsightFromRoute(): number | null {
    const raw = router.params.insight;
    if (!raw) return null;
    const id = Number.parseInt(raw, 10);
    if (!Number.isSafeInteger(id) || id <= 0) return null;
    return id;
  }

  function formatDateRange(from: string, to: string): string {
    if (from === to) return formatDate(from);
    return `${formatDate(from)} to ${formatDate(to)}`;
  }

  function formatDate(date: string): string {
    const d = new Date(date + "T00:00:00");
    return d.toLocaleDateString(undefined, {
      month: "short",
      day: "numeric",
    });
  }

  function formatTime(iso: string): string {
    const d = new Date(iso);
    return d.toLocaleTimeString(undefined, {
      hour: "2-digit",
      minute: "2-digit",
    });
  }

  function severityLabel(severity: QualityPatternSeverity): string {
    switch (severity) {
      case "critical":
        return "Critical";
      case "warning":
        return "Warning";
      case "watch":
        return "Watch";
      case "clear":
        return "Clear";
      case "unavailable":
        return "No data";
    }
  }

  function affectedLabel(pattern: QualityPatternView): string {
    if (pattern.totalSessions === 0) return "No computed sessions";
    return `${pattern.affectedSessions} of ${pattern.totalSessions} sessions`;
  }

  function pct(count: number, total: number): number {
    if (total <= 0) return 0;
    return Math.round((count / total) * 100);
  }

  function maxTrend(pattern: QualityPatternView): number {
    return Math.max(1, ...pattern.trend.map((p) => p.value));
  }

  function cannedKindLabel(
    kind: CannedInsightKind | "" | undefined,
  ): string {
    switch (kind) {
      case "prompt_maturity_review":
        return "Prompt Maturity";
      case "context_setup_review":
        return "Context Setup";
      case "workflow_hygiene_review":
        return "Workflow Hygiene";
      case "tool_reliability_review":
        return "Tool Reliability";
      case "model_cost_review":
        return "Model and Cost";
      case "instruction_opportunity_review":
        return "Instruction Opportunities";
      default:
        return "Generated Recommendation";
    }
  }

  function insightTypeLabel(
    type: InsightType,
    kind: CannedInsightKind | "" | undefined,
  ): string {
    if (type === "llm_canned") return cannedKindLabel(kind);
    if (type === "agent_analysis") return "Agent Analysis";
    return "Activity";
  }

  function cacheStatusLabel(status: string | undefined): string {
    if (status === "hit") return "cache hit";
    if (status === "fresh") return "fresh";
    return "";
  }

  onMount(() => {
    sessions.loadProjects();
    fetchInsightSignals();
    insights.load();
    refreshTimer = setInterval(
      () => fetchInsightSignals(),
      REFRESH_INTERVAL_MS,
    );
    unsubEvents = events.subscribeDebounced(() => {
      fetchInsightSignals();
    });
  });

  $effect(() => {
    const headerProject = sessions.filters.project;
    const headerMachine = sessions.filters.machine;
    const headerAgent = sessions.filters.agent;
    const headerTermination = sessions.filters.termination;
    const headerRecentlyActive = sessions.filters.recentlyActive;
    const headerMinUserMessages =
      sessions.filters.minUserMessages;
    const headerIncludeOneShot =
      sessions.filters.includeOneShot;
    const headerIncludeAutomated =
      sessions.filters.includeAutomated;

    const changed =
      untrack(() => analytics.project) !== headerProject ||
      untrack(() => analytics.machine) !== headerMachine ||
      untrack(() => analytics.agent) !== headerAgent ||
      untrack(() => analytics.termination) !== headerTermination ||
      untrack(() => analytics.recentlyActive) !==
        headerRecentlyActive ||
      untrack(() => analytics.minUserMessages) !==
        (headerMinUserMessages > 0 ? headerMinUserMessages : 0) ||
      untrack(() => analytics.includeOneShot) !==
        headerIncludeOneShot ||
      untrack(() => analytics.includeAutomated) !==
        headerIncludeAutomated;

    if (changed) {
      analytics.project = headerProject;
      analytics.machine = headerMachine;
      analytics.agent = headerAgent;
      analytics.termination = headerTermination;
      analytics.recentlyActive = headerRecentlyActive;
      analytics.minUserMessages =
        headerMinUserMessages > 0 ? headerMinUserMessages : 0;
      analytics.includeOneShot = headerIncludeOneShot;
      analytics.includeAutomated = headerIncludeAutomated;
      untrack(() => fetchInsightSignals());
    }
  });

  onDestroy(() => {
    if (refreshTimer !== undefined) clearInterval(refreshTimer);
    clearTimeout(copiedInsightLinkTimer);
    unsubEvents?.();
  });

  $effect(() => {
    if (router.route !== "insights") return;
    const id = selectedInsightFromRoute();
    if (id === null || insights.selectedId === id) return;
    if (!insights.items.some((item) => item.id === id)) return;
    insights.select(id);
  });
</script>

<div class="insights-page">
  <header class="toolbar">
    <DateRangeSelector
      from={analytics.from}
      to={analytics.to}
      onChange={(from, to) => analytics.setDateRange(from, to)}
      onPreset={(days) => analytics.setRollingWindow(days)}
    />

    <div class="filter-group">
      <ProjectTypeahead
        projects={sessions.projects}
        value={analytics.project}
        onselect={handleProjectChange}
      />
      <select
        class="agent-select"
        value={analytics.agent}
        onchange={handleAgentChange}
        aria-label="Filter insights by agent"
      >
        <option value="">All agents</option>
        <option value="claude">Claude</option>
        <option value="codex">Codex</option>
        <option value="copilot">Copilot</option>
        <option value="gemini">Gemini</option>
        <option value="kiro">Kiro</option>
      </select>
    </div>

    <button
      class="icon-btn"
      onclick={handleRefresh}
      title="Refresh insights"
      aria-label="Refresh insights"
    >
      <svg width="14" height="14" viewBox="0 0 16 16" fill="currentColor">
        <path d="M8 3a5 5 0 00-4.546 2.914.5.5 0 01-.908-.418A6 6 0 0114 8a.5.5 0 01-1 0 5 5 0 00-5-5zm4.546 7.086a.5.5 0 01.908.418A6 6 0 012 8a.5.5 0 011 0 5 5 0 005 5 5 5 0 004.546-2.914z"/>
      </svg>
    </button>
  </header>

  <main class="content">
    <section class="section-block" aria-labelledby="facts-title">
      <div class="section-heading">
        <div>
          <div class="eyebrow">
            <span class="badge rule">Rule-based</span>
            <span>Scored facts</span>
          </div>
          <h2 id="facts-title">Quality Patterns</h2>
        </div>
        <p>
          Deterministic counts from persisted session signals for
          {formatDateRange(analytics.from, analytics.to)}.
        </p>
      </div>

      {#if loading && !signals}
        <div class="summary-grid" aria-live="polite">
          {#each Array(4) as _}
            <div class="skeleton-card"></div>
          {/each}
        </div>
        <div class="pattern-grid">
          {#each Array(4) as _}
            <div class="skeleton-pattern"></div>
          {/each}
        </div>
      {:else if error && !signals}
        <div class="state-panel error" role="alert">
          <strong>Could not load deterministic insights.</strong>
          <span>{error}</span>
          <button onclick={fetchInsightSignals}>
            Retry
          </button>
        </div>
      {:else if !hasData}
        <div class="state-panel">
          <strong>No scored quality data for this range.</strong>
          <span>
            Sync or backfill sessions with Phase 3 quality signals to
            populate prompt, context, workflow, and tool patterns.
          </span>
        </div>
      {:else}
        {#if error}
          <div class="inline-warning" role="status">
            Showing cached deterministic data. Latest refresh failed:
            {error}
          </div>
        {/if}

        <div class="summary-grid">
          <article class="summary-card">
            <span class="label">Average score</span>
            <strong>
              {summary.avgHealthScore == null
                ? "--"
                : Math.round(summary.avgHealthScore)}
            </strong>
            <span>
              {summary.avgHealthScore == null
                ? "No scored sessions"
                : `Grade ${scoreToGrade(summary.avgHealthScore)}`}
            </span>
          </article>
          <article class="summary-card">
            <span class="label">Scored sessions</span>
            <strong>{summary.scoredSessions}</strong>
            <span>{summary.unscoredSessions} unscored</span>
          </article>
          <article class="summary-card">
            <span class="label">Low quality</span>
            <strong>{summary.lowQualitySessions}</strong>
            <span>D/F graded sessions</span>
          </article>
          <article class="summary-card">
            <span class="label">Prompt signals</span>
            <strong>{summary.computedQualitySessions}</strong>
            <span>sessions computed</span>
          </article>
        </div>

        <div class="distribution-row" aria-label="Score distribution">
          {#each summary.scoreDistribution as bucket}
            <div class="grade-bar">
              <span>{bucket.grade}</span>
              <div class="bar-track">
                <div
                  class="bar-fill"
                  style:width={`${(bucket.count / maxGradeCount) * 100}%`}
                ></div>
              </div>
              <strong>{bucket.count}</strong>
            </div>
          {/each}
        </div>

        <div class="pattern-grid">
          {#each patterns as pattern}
            <article
              class={`pattern-card severity-${pattern.severity}`}
              aria-labelledby={`${pattern.id}-title`}
            >
              <div class="pattern-head">
                <div>
                  <h3 id={`${pattern.id}-title`}>
                    {pattern.title}
                  </h3>
                  <p>{pattern.summary}</p>
                </div>
                <span class="severity">
                  {severityLabel(pattern.severity)}
                </span>
              </div>

              <div class="affected">
                <strong>{affectedLabel(pattern)}</strong>
                <span>
                  {pct(pattern.affectedSessions, pattern.totalSessions)}%
                  affected
                </span>
              </div>

              <div class="driver-list">
                {#each pattern.drivers as driver}
                  <div class="driver-row">
                    <span>{driver.label}</span>
                    <strong>
                      {driver.total}{driver.unit ?? ""}
                    </strong>
                    <em>{driver.sessions} sessions</em>
                  </div>
                {/each}
              </div>

              <div
                class="sparkline"
                aria-label={`${pattern.title}: ${pattern.trendLabel}`}
              >
                <span class="trend-caption">{pattern.trendLabel}</span>
                {#each pattern.trend.slice(-16) as point}
                  <span
                    title={`${formatDate(point.date)}: ${point.value} ${point.label}`}
                    style:height={`${Math.max(8, (point.value / maxTrend(pattern)) * 32)}px`}
                  ></span>
                {/each}
              </div>
              <p class="severity-note">{pattern.severityDescription}</p>

              {#if pattern.examples.length > 0}
                <div class="examples">
                  <span class="examples-label">{pattern.examplesLabel}</span>
                  {#each pattern.examples as example}
                    <div class="example-row">
                      <span>{example.label}</span>
                      <em>{example.detail}</em>
                    </div>
                  {/each}
                </div>
              {/if}
            </article>
          {/each}
        </div>
      {/if}
    </section>

    <section class="section-block" aria-labelledby="actions-title">
      <div class="section-heading compact">
        <div>
          <div class="eyebrow">
            <span class="badge rule">Rule-based</span>
            <span>Next actions</span>
          </div>
          <h2 id="actions-title">Deterministic Recommendations</h2>
        </div>
      </div>

      {#if recommendations.length === 0}
        <div class="state-panel compact-state">
          <strong>No rule-based actions are firing.</strong>
          <span>
            Patterns are clear or unavailable for the current filters.
          </span>
        </div>
      {:else}
        <div class="recommendation-list">
          {#each recommendations as rec}
            <article class="recommendation">
              <span class="badge rule">Rule-based</span>
              <strong>{rec.label}</strong>
              <p>{rec.rationale}</p>
            </article>
          {/each}
        </div>
      {/if}
    </section>

    <section
      class="section-block generated-block"
      aria-labelledby="generated-title"
    >
      <div class="section-heading">
        <div>
          <div class="eyebrow">
            <span class="badge generated">Generated</span>
            <span>Separate from scored facts</span>
          </div>
          <h2 id="generated-title">Generated Insights Archive</h2>
        </div>
        <p>
          Saved generated text is shown separately and does not affect
          deterministic quality scores. Creation remains out of scope
          for this deterministic phase.
        </p>
      </div>

      <div class="generated-controls">
        <label class="generated-control">
          <span>Template</span>
          <select
            class="generated-select"
            value={insights.cannedKind}
            onchange={handleCannedKindChange}
          >
            <option value="prompt_maturity_review">Prompt Maturity</option>
            <option value="context_setup_review">Context Setup</option>
            <option value="workflow_hygiene_review">Workflow Hygiene</option>
            <option value="tool_reliability_review">Tool Reliability</option>
            <option value="model_cost_review">Model and Cost</option>
            <option value="instruction_opportunity_review">
              Instruction Opportunities
            </option>
          </select>
        </label>

        <label class="generated-control">
          <span>Model agent</span>
          <select
            class="generated-select"
            value={insights.agent}
            onchange={handleInsightAgentChange}
          >
            <option value="claude">Claude</option>
            <option value="codex">Codex</option>
            <option value="copilot">Copilot</option>
            <option value="gemini">Gemini</option>
            <option value="kiro">Kiro</option>
          </select>
        </label>

        <label class="generated-control focus-control">
          <span>Optional focus</span>
          <textarea
            class="generated-focus"
            value={insights.promptText}
            maxlength="1200"
            rows="2"
            placeholder="Narrow the recommendation without changing scored facts"
            oninput={handlePromptChange}
          ></textarea>
        </label>

        <button
          class="generate-action"
          disabled={generationUnavailable}
          title={readOnly
            ? "Generation is disabled in read-only mode"
            : "Generate quality recommendation"}
          onclick={handleGenerateCanned}
        >
          Generate
        </button>
      </div>

      {#if insights.loading}
        <div class="state-panel compact-state">Loading archive...</div>
      {:else if insights.items.length === 0 && insights.tasks.length === 0}
        <div class="state-panel compact-state">
          <strong>No generated insights saved.</strong>
          <span>
            The deterministic dashboard above works without LLM
            configuration. Generated insight creation is reserved for
            the generated-insights phase.
          </span>
        </div>
      {:else}
        <div class="generated-layout">
          <div class="generated-list">
            {#each insights.tasks as task (task.clientId)}
              <button
                class:active={insights.selectedTaskId === task.clientId}
                class:error-task={task.status === "error"}
                onclick={() => selectGeneratedTask(task.clientId)}
              >
                <span>{task.status === "error" ? "Error" : "Running"}</span>
                <strong>{task.project || "global"}</strong>
                <em>
                  {task.kind ? cannedKindLabel(task.kind) : task.phase}
                </em>
              </button>
            {/each}
            {#each insights.items as item (item.id)}
              <button
                class:active={insights.selectedId === item.id}
                onclick={() => selectGeneratedInsight(item.id)}
              >
                <span>
                  {insightTypeLabel(item.type, item.kind)}
                </span>
                <strong>{item.project || "global"}</strong>
                <em>
                  {formatDateRange(item.date_from, item.date_to)}
                  · {formatTime(item.created_at)}
                </em>
              </button>
            {/each}
          </div>

          <article class="generated-detail">
            {#if insights.selectedTask}
              <span class="badge generated">
                {insights.selectedTask.status === "error"
                  ? "Generation error"
                  : "Generating"}
              </span>
              {#if insights.selectedTask.error}
                <p>{insights.selectedTask.error}</p>
              {:else}
                <p>{insights.selectedTask.phase}</p>
              {/if}
            {:else if insights.selectedItem}
              <div class="generated-detail-head">
                <div class="generated-meta">
                  <span class="badge generated">
                    {insightTypeLabel(
                      insights.selectedItem.type,
                      insights.selectedItem.kind,
                    )}
                  </span>
                  {#if insights.selectedItem.type === "llm_canned"}
                    {#if cacheStatusLabel(insights.selectedItem.cache_status)}
                      <span class="detail-chip muted">
                        {cacheStatusLabel(insights.selectedItem.cache_status)}
                      </span>
                    {/if}
                    {#if insights.selectedItem.template_version}
                      <span class="detail-chip muted">
                        template {insights.selectedItem.template_version}
                      </span>
                    {/if}
                    {#if insights.selectedItem.aggregate_hash}
                      <span class="detail-chip muted">
                        aggregate {insights.selectedItem.aggregate_hash.slice(0, 12)}
                      </span>
                    {/if}
                  {/if}
                </div>
                <div class="generated-actions">
                  <CopyButton
                    class="insight-link-copy"
                    copied={copiedInsightLinkId === insights.selectedItem.id}
                    ariaLabel="Copy generated insight link"
                    copiedAriaLabel="Copied generated insight link"
                    title="Copy link to generated insight"
                    copiedTitle="Copied link"
                    onclick={() =>
                      handleCopyInsightLink(insights.selectedItem!.id)}
                  />
                  <button
                    class="text-btn danger"
                    onclick={() => {
                      if (insights.selectedItem) {
                        insights.deleteItem(insights.selectedItem.id);
                        router.replaceParams({});
                      }
                    }}
                  >
                    Delete
                  </button>
                </div>
              </div>
              <div class="markdown-body">
                {@html renderMarkdown(insights.selectedItem.content)}
              </div>
            {:else}
              <p>Select a generated insight to read it.</p>
            {/if}
          </article>
        </div>
      {/if}
    </section>
  </main>
</div>

<style>
  .insights-page {
    flex: 1;
    display: flex;
    flex-direction: column;
    min-height: 0;
    background: var(--bg-primary);
  }

  .toolbar {
    display: flex;
    align-items: center;
    gap: 12px;
    padding: 8px 16px;
    background: var(--bg-surface);
    border-bottom: 1px solid var(--border-muted);
    flex-shrink: 0;
  }

  .filter-group {
    display: flex;
    align-items: center;
    gap: 8px;
    min-width: 280px;
  }

  .agent-select {
    height: 28px;
    min-width: 112px;
    border: 1px solid var(--border-muted);
    background: var(--bg-inset);
    border-radius: var(--radius-sm);
    color: var(--text-secondary);
    padding: 0 6px;
    font-size: 12px;
  }

  .icon-btn {
    width: 28px;
    height: 28px;
    display: inline-flex;
    align-items: center;
    justify-content: center;
    border-radius: var(--radius-sm);
    color: var(--text-muted);
    margin-left: auto;
  }

  .icon-btn:hover {
    background: var(--bg-surface-hover);
    color: var(--text-primary);
  }

  .content {
    flex: 1;
    min-height: 0;
    overflow-y: auto;
    padding: 18px;
    display: flex;
    flex-direction: column;
    gap: 18px;
  }

  .section-block {
    display: flex;
    flex-direction: column;
    gap: 12px;
  }

  .section-heading {
    display: flex;
    align-items: flex-end;
    justify-content: space-between;
    gap: 16px;
  }

  .section-heading.compact {
    align-items: center;
  }

  .section-heading h2 {
    margin-top: 2px;
    font-size: 18px;
    line-height: 1.2;
    color: var(--text-primary);
  }

  .section-heading p {
    max-width: 56ch;
    color: var(--text-muted);
    font-size: 12px;
    line-height: 1.4;
    text-align: right;
  }

  .eyebrow {
    display: flex;
    align-items: center;
    gap: 8px;
    color: var(--text-muted);
    font-size: 11px;
    font-weight: 600;
    text-transform: uppercase;
    letter-spacing: 0.04em;
  }

  .badge {
    display: inline-flex;
    align-items: center;
    height: 18px;
    padding: 0 6px;
    border-radius: 3px;
    border: 1px solid var(--border-muted);
    font-size: 10px;
    font-weight: 700;
    letter-spacing: 0.03em;
    text-transform: uppercase;
  }

  .badge.rule {
    color: var(--accent-blue);
    background: color-mix(
      in srgb,
      var(--accent-blue) 9%,
      var(--bg-surface)
    );
    border-color: color-mix(
      in srgb,
      var(--accent-blue) 22%,
      var(--border-muted)
    );
  }

  .badge.generated {
    color: var(--accent-purple);
    background: color-mix(
      in srgb,
      var(--accent-purple) 9%,
      var(--bg-surface)
    );
    border-color: color-mix(
      in srgb,
      var(--accent-purple) 22%,
      var(--border-muted)
    );
  }

  .summary-grid {
    display: grid;
    grid-template-columns: repeat(4, minmax(0, 1fr));
    gap: 10px;
  }

  .summary-card,
  .pattern-card,
  .recommendation,
  .generated-detail,
  .state-panel {
    background: var(--bg-surface);
    border: 1px solid var(--border-muted);
    border-radius: var(--radius-md);
  }

  .summary-card {
    min-height: 92px;
    padding: 12px;
    display: flex;
    flex-direction: column;
    justify-content: space-between;
  }

  .summary-card .label {
    color: var(--text-muted);
    font-size: 11px;
    font-weight: 600;
    text-transform: uppercase;
    letter-spacing: 0.04em;
  }

  .summary-card strong {
    font-size: 28px;
    line-height: 1;
    color: var(--text-primary);
    font-variant-numeric: tabular-nums;
  }

  .summary-card span:last-child {
    color: var(--text-secondary);
    font-size: 12px;
  }

  .distribution-row {
    display: grid;
    grid-template-columns: repeat(5, minmax(0, 1fr));
    gap: 8px;
    padding: 10px;
    background: var(--bg-surface);
    border: 1px solid var(--border-muted);
    border-radius: var(--radius-md);
  }

  .grade-bar {
    display: grid;
    grid-template-columns: 18px 1fr minmax(22px, auto);
    gap: 8px;
    align-items: center;
    color: var(--text-secondary);
    font-size: 12px;
  }

  .grade-bar strong {
    text-align: right;
    color: var(--text-primary);
    font-variant-numeric: tabular-nums;
  }

  .bar-track {
    height: 8px;
    border-radius: 4px;
    background: var(--bg-inset);
    overflow: hidden;
  }

  .bar-fill {
    height: 100%;
    min-width: 2px;
    background: var(--accent-blue);
  }

  .pattern-grid {
    display: grid;
    grid-template-columns: repeat(2, minmax(0, 1fr));
    gap: 12px;
  }

  .pattern-card {
    min-height: 310px;
    padding: 14px;
    display: flex;
    flex-direction: column;
    gap: 12px;
  }

  .pattern-head {
    display: flex;
    gap: 12px;
    justify-content: space-between;
    align-items: flex-start;
  }

  .pattern-head h3 {
    font-size: 14px;
    margin-bottom: 3px;
  }

  .pattern-head p {
    color: var(--text-muted);
    font-size: 12px;
    line-height: 1.4;
  }

  .severity {
    flex-shrink: 0;
    border-radius: 999px;
    padding: 2px 8px;
    font-size: 11px;
    font-weight: 700;
    border: 1px solid var(--border-muted);
  }

  .severity-critical .severity {
    color: var(--accent-red);
    background: color-mix(
      in srgb,
      var(--accent-red) 9%,
      transparent
    );
  }

  .severity-warning .severity,
  .severity-watch .severity {
    color: var(--accent-amber);
    background: color-mix(
      in srgb,
      var(--accent-amber) 11%,
      transparent
    );
  }

  .severity-clear .severity {
    color: var(--accent-green);
    background: color-mix(
      in srgb,
      var(--accent-green) 10%,
      transparent
    );
  }

  .severity-unavailable .severity {
    color: var(--text-muted);
    background: var(--bg-inset);
  }

  .affected {
    display: flex;
    align-items: baseline;
    justify-content: space-between;
    gap: 10px;
    padding: 10px;
    border-radius: var(--radius-sm);
    background: var(--bg-inset);
  }

  .affected strong {
    color: var(--text-primary);
    font-size: 13px;
  }

  .affected span {
    color: var(--text-muted);
    font-size: 12px;
  }

  .driver-list {
    display: flex;
    flex-direction: column;
    gap: 6px;
  }

  .driver-row {
    display: grid;
    grid-template-columns: 1fr auto auto;
    gap: 10px;
    align-items: baseline;
    font-size: 12px;
  }

  .driver-row span {
    color: var(--text-secondary);
    min-width: 0;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  .driver-row strong {
    color: var(--text-primary);
    font-variant-numeric: tabular-nums;
  }

  .driver-row em {
    color: var(--text-muted);
    font-style: normal;
    font-variant-numeric: tabular-nums;
  }

  .sparkline {
    height: 42px;
    display: flex;
    align-items: end;
    gap: 3px;
    padding: 6px 0 2px;
    border-top: 1px solid var(--border-muted);
    position: relative;
  }

  .sparkline span:not(.trend-caption) {
    width: 100%;
    min-width: 3px;
    max-width: 16px;
    background: color-mix(
      in srgb,
      var(--accent-blue) 48%,
      var(--border-muted)
    );
    border-radius: 2px 2px 0 0;
  }

  .trend-caption {
    align-self: start;
    width: auto;
    min-width: 118px;
    max-width: none;
    height: auto !important;
    margin-right: 8px;
    color: var(--text-muted);
    font-size: 10px;
    font-weight: 700;
    text-transform: uppercase;
    letter-spacing: 0.04em;
    background: transparent;
  }

  .severity-note {
    margin-top: -4px;
    color: var(--text-muted);
    font-size: 11px;
    line-height: 1.35;
  }

  .examples {
    display: flex;
    flex-direction: column;
    gap: 6px;
    margin-top: auto;
  }

  .examples-label {
    color: var(--text-muted);
    font-size: 10px;
    font-weight: 700;
    text-transform: uppercase;
    letter-spacing: 0.04em;
  }

  .example-row {
    display: grid;
    grid-template-columns: minmax(90px, 0.35fr) 1fr;
    gap: 10px;
    font-size: 12px;
  }

  .example-row span {
    color: var(--text-primary);
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  .example-row em {
    color: var(--text-muted);
    font-style: normal;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  .recommendation-list {
    display: grid;
    grid-template-columns: repeat(2, minmax(0, 1fr));
    gap: 10px;
  }

  .recommendation {
    padding: 12px;
    display: grid;
    gap: 7px;
  }

  .recommendation strong {
    color: var(--text-primary);
    font-size: 13px;
  }

  .recommendation p {
    color: var(--text-secondary);
    font-size: 12px;
    line-height: 1.45;
  }

  .generated-block {
    border-top: 1px solid var(--border-muted);
    padding-top: 18px;
  }

  .generated-controls {
    display: grid;
    grid-template-columns: minmax(180px, 220px) minmax(130px, 160px) minmax(240px, 1fr) auto;
    gap: 10px;
    align-items: end;
    padding: 12px;
    background: var(--bg-surface);
    border: 1px solid var(--border-muted);
    border-radius: var(--radius-md);
  }

  .generated-control {
    display: grid;
    gap: 5px;
    min-width: 0;
  }

  .generated-control span {
    color: var(--text-muted);
    font-size: 10px;
    font-weight: 700;
    letter-spacing: 0.04em;
    text-transform: uppercase;
  }

  .generated-select,
  .generated-focus {
    width: 100%;
    border: 1px solid var(--border-muted);
    background: var(--bg-inset);
    border-radius: var(--radius-sm);
    color: var(--text-primary);
    font-size: 12px;
  }

  .generated-select {
    height: 30px;
    padding: 0 8px;
  }

  .generated-focus {
    min-height: 30px;
    max-height: 76px;
    padding: 7px 8px;
    resize: vertical;
    line-height: 1.35;
  }

  .generate-action {
    height: 30px;
    padding: 0 12px;
    background: var(--accent-purple);
    color: white;
    border-radius: var(--radius-sm);
    font-size: 12px;
    font-weight: 700;
  }

  .generate-action:disabled {
    cursor: not-allowed;
    opacity: 0.5;
  }

  .generated-layout {
    display: grid;
    grid-template-columns: minmax(240px, 320px) 1fr;
    gap: 12px;
    align-items: start;
  }

  .generated-list {
    display: flex;
    flex-direction: column;
    gap: 6px;
  }

  .generated-list button {
    min-height: 54px;
    padding: 9px 10px;
    display: grid;
    gap: 2px;
    text-align: left;
    background: var(--bg-surface);
    border: 1px solid var(--border-muted);
    border-radius: var(--radius-md);
  }

  .generated-list button:hover,
  .generated-list button.active {
    background: var(--bg-surface-hover);
    border-color: var(--border-default);
  }

  .generated-list button span {
    color: var(--accent-purple);
    font-size: 10px;
    font-weight: 700;
    text-transform: uppercase;
  }

  .generated-list button strong {
    color: var(--text-primary);
    font-size: 12px;
  }

  .generated-list button em {
    color: var(--text-muted);
    font-size: 11px;
    font-style: normal;
  }

  .generated-list button.error-task span {
    color: var(--accent-red);
  }

  .generated-detail {
    min-height: 220px;
    padding: 14px;
  }

  .generated-detail-head {
    display: flex;
    justify-content: space-between;
    align-items: center;
    gap: 12px;
    margin-bottom: 12px;
  }

  .generated-meta {
    display: flex;
    align-items: center;
    flex-wrap: wrap;
    gap: 6px;
    min-width: 0;
  }

  .generated-actions {
    display: flex;
    align-items: center;
    gap: 6px;
    flex-shrink: 0;
  }

  .generated-actions :global(.insight-link-copy.copy-btn) {
    opacity: 1;
    border: 1px solid var(--border-muted);
    background: var(--bg-inset);
  }

  .generated-actions :global(.insight-link-copy.copy-btn:hover) {
    border-color: var(--border-default);
  }

  .detail-chip {
    display: inline-flex;
    align-items: center;
    min-height: 18px;
    padding: 2px 6px;
    border: 1px solid var(--border-muted);
    border-radius: 3px;
    color: var(--text-secondary);
    background: var(--bg-inset);
    font-size: 10px;
    font-weight: 700;
    letter-spacing: 0.02em;
    text-transform: uppercase;
  }

  .detail-chip.muted {
    color: var(--text-muted);
  }

  .text-btn {
    color: var(--text-muted);
    font-size: 12px;
  }

  .text-btn:hover {
    color: var(--text-primary);
  }

  .text-btn.danger:hover {
    color: var(--accent-red);
  }

  .markdown-body {
    color: var(--text-primary);
    line-height: 1.65;
    max-width: 76ch;
  }

  .markdown-body :global(h1),
  .markdown-body :global(h2),
  .markdown-body :global(h3) {
    margin: 14px 0 6px;
    font-size: 15px;
  }

  .markdown-body :global(p),
  .markdown-body :global(ul),
  .markdown-body :global(ol) {
    margin: 8px 0;
  }

  .markdown-body :global(ul),
  .markdown-body :global(ol) {
    padding-left: 18px;
  }

  .state-panel {
    padding: 18px;
    display: grid;
    gap: 6px;
    color: var(--text-secondary);
  }

  .state-panel strong {
    color: var(--text-primary);
  }

  .state-panel button {
    justify-self: start;
    margin-top: 6px;
    height: 26px;
    padding: 0 10px;
    background: var(--accent-blue);
    color: white;
    border-radius: var(--radius-sm);
    font-size: 12px;
    font-weight: 700;
  }

  .state-panel.error {
    border-color: color-mix(
      in srgb,
      var(--accent-red) 35%,
      var(--border-muted)
    );
  }

  .compact-state {
    padding: 14px;
  }

  .inline-warning {
    padding: 9px 10px;
    background: color-mix(
      in srgb,
      var(--accent-amber) 10%,
      var(--bg-surface)
    );
    border: 1px solid color-mix(
      in srgb,
      var(--accent-amber) 24%,
      var(--border-muted)
    );
    border-radius: var(--radius-md);
    color: var(--text-secondary);
    font-size: 12px;
  }

  .skeleton-card,
  .skeleton-pattern {
    border-radius: var(--radius-md);
    background: linear-gradient(
      90deg,
      var(--bg-surface) 0%,
      var(--bg-surface-hover) 50%,
      var(--bg-surface) 100%
    );
    background-size: 200% 100%;
    animation: shimmer 1.4s ease-in-out infinite;
    border: 1px solid var(--border-muted);
  }

  .skeleton-card {
    height: 92px;
  }

  .skeleton-pattern {
    height: 310px;
  }

  @keyframes shimmer {
    0% {
      background-position: 200% 0;
    }
    100% {
      background-position: -200% 0;
    }
  }

  @media (max-width: 980px) {
    .toolbar,
    .section-heading {
      align-items: stretch;
      flex-direction: column;
    }

    .icon-btn {
      margin-left: 0;
    }

    .toolbar :global(.date-range-picker) {
      align-items: stretch;
      flex-direction: column;
      gap: 6px;
      min-width: 0;
    }

    .toolbar :global(.presets) {
      flex-wrap: wrap;
    }

    .toolbar :global(.date-inputs) {
      display: grid;
      grid-template-columns: minmax(0, 1fr) auto minmax(0, 1fr);
    }

    .toolbar :global(.date-input) {
      min-width: 0;
      width: 100%;
    }

    .filter-group {
      min-width: 0;
    }

    .section-heading p {
      text-align: left;
    }

    .summary-grid,
    .pattern-grid,
    .recommendation-list,
    .generated-controls,
    .generated-layout {
      grid-template-columns: 1fr;
    }

    .distribution-row {
      grid-template-columns: 1fr;
    }
  }
</style>
