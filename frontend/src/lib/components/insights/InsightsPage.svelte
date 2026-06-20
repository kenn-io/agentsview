<script lang="ts">
  import { onMount, onDestroy, untrack } from "svelte";
  import { analytics } from "../../stores/analytics.svelte.js";
  import { insights } from "../../stores/insights.svelte.js";
  import { ui } from "../../stores/ui.svelte.js";
  import { getBasePath, router } from "../../stores/router.svelte.js";
  import { sessions } from "../../stores/sessions.svelte.js";
  import { sync } from "../../stores/sync.svelte.js";
  import { events } from "../../stores/events.svelte.js";
  import { copyToClipboard } from "../../utils/clipboard.js";
  import {
    yokedDates,
    panelDateState,
    panelStateToRange,
    rangeToInsightParams,
    rangeToPanelDate,
    type PanelDateState,
  } from "../../stores/yokedDates.svelte.js";
  import { renderMarkdown } from "../../utils/markdown.js";
  import { rollingRange } from "../../utils/dates.js";
  import { scoreToGrade } from "../../utils/grade.js";
  import { agentLabel } from "../../utils/agents.js";
  import { AnalyticsService } from "../../api/generated/index.js";
  import type {
    AgentName,
    AutomatedScope,
    CannedInsightKind,
    InsightGenerationFilters,
    InsightType,
    SignalCalibration,
    SignalSessionExample,
  } from "../../api/types.js";
  import CopyButton from "../shared/CopyButton.svelte";
  import OptionTypeahead from "../layout/OptionTypeahead.svelte";
  import ProjectTypeahead from "../layout/ProjectTypeahead.svelte";
  import RangePicker from "../shared/RangePicker.svelte";
  import {
    resolveRange,
    selectionFromWindow,
    type RangeSelection,
  } from "../shared/rangeSelection.js";
  import {
    buildQualityPatterns,
    buildQualitySummary,
    buildRuleBasedRecommendations,
    type QualityPatternSeverity,
    type QualityPatternView,
  } from "./qualityPatterns.js";

  const REFRESH_INTERVAL_MS = 5 * 60 * 1000;
  const INSIGHTS_WINDOW_PARAM = "window_days";
  const INSIGHTS_DATE_PARAM_KEYS = [
    INSIGHTS_WINDOW_PARAM,
    "date_from",
    "date_to",
  ] as const;
  type AnalyticsParams = Parameters<
    typeof AnalyticsService.getApiV1AnalyticsSignals
  >[0];

  let refreshTimer: ReturnType<typeof setInterval> | undefined;
  let unsubEvents: (() => void) | undefined;
  let copiedInsightLinkId: number | null = $state(null);
  let copiedInsightLinkTimer:
    | ReturnType<typeof setTimeout>
    | undefined;
  let selectedSignalId: string | null = $state(null);
  let signalExamples: SignalSessionExample[] = $state([]);
  let signalExamplesLoading = $state(false);
  let signalExamplesError: string | null = $state(null);
  let signalExamplesFilterKey: string | null = $state(null);
  let signalExamplesRequest = 0;

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
  const earliestSession = $derived(sync.stats?.earliest_session ?? null);
  const rangeSelection = $derived(
    selectionFromWindow({
      isPinned: analytics.isPinned,
      windowDays: analytics.windowDays,
      from: analytics.from,
      to: analytics.to,
      earliestSession,
    }),
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
  const generationAgentNames = [
    "claude",
    "codex",
    "copilot",
    "gemini",
    "kiro",
  ] satisfies AgentName[];
  const agentOptions = $derived.by(() => {
    const opts = [...sessions.agents]
      .sort((a, b) => b.session_count - a.session_count)
      .map((agent) => ({
        name: agent.name,
        label: `${agentLabel(agent.name)} (${agent.session_count})`,
        displayLabel: agentLabel(agent.name),
        count: agent.session_count,
      }));
    return [
      {
        name: "",
        label: "All Agents",
        displayLabel: "All Agents",
        count: 0,
      },
      ...opts,
    ];
  });
  const generationAgentOptions = generationAgentNames.map((name) => ({
    name,
    label: agentLabel(name),
    displayLabel: agentLabel(name),
  }));
  const templateOptions = [
    { name: "prompt_maturity_review", label: "Prompt Maturity" },
    { name: "context_setup_review", label: "Context Setup" },
    { name: "workflow_hygiene_review", label: "Workflow Hygiene" },
    { name: "tool_reliability_review", label: "Tool Reliability" },
    { name: "model_cost_review", label: "Model and Cost" },
    {
      name: "instruction_opportunity_review",
      label: "Instruction Opportunities",
    },
  ];
  const scopeOptions = [
    { name: "human", label: "No automated" },
    { name: "all", label: "Both" },
    { name: "automated", label: "Only automated" },
  ];

  function applyRange(sel: RangeSelection) {
    let state: PanelDateState | null = null;
    if (sel.mode === "relative" && sel.days > 0) {
      analytics.setRollingWindow(sel.days);
      state = panelDateState(analytics.from, analytics.to, {
        mode: "rolling",
        windowDays: sel.days,
      });
    } else {
      const range = resolveRange(sel, earliestSession);
      analytics.setDateRange(range.from, range.to);
      state = panelDateState(range.from, range.to, { mode: "fixed" });
    }
    updateYokeFromInsights(state);
  }

  function parseInsightWindowDays(raw: string | undefined): number | null {
    if (!raw) return null;
    const n = Number.parseInt(raw, 10);
    if (!Number.isInteger(n) || n <= 0 || String(n) !== raw) {
      return null;
    }
    return n;
  }

  function insightParamsToPanelDate(
    params: Record<string, string>,
  ): PanelDateState | null {
    const windowDays = parseInsightWindowDays(params[INSIGHTS_WINDOW_PARAM]);
    if (windowDays !== null) {
      const range = rollingRange(windowDays);
      return panelDateState(range.from, range.to, {
        mode: "rolling",
        windowDays,
      });
    }
    return panelDateState(
      params.date_from ?? "",
      params.date_to ?? "",
      { mode: "fixed" },
    );
  }

  function hasInsightDateParams(
    params: Record<string, string>,
  ): boolean {
    return !!params.date_from || !!params.date_to ||
      !!params[INSIGHTS_WINDOW_PARAM];
  }

  function applyInsightPanelDate(state: PanelDateState): boolean {
    const before = JSON.stringify({
      from: analytics.from,
      to: analytics.to,
      isPinned: analytics.isPinned,
      windowDays: analytics.windowDays,
    });
    if (state.mode === "rolling" && state.windowDays) {
      analytics.applyRollingWindow(state.windowDays);
    } else {
      analytics.applyDateRange(state.from, state.to);
    }
    const after = JSON.stringify({
      from: analytics.from,
      to: analytics.to,
      isPinned: analytics.isPinned,
      windowDays: analytics.windowDays,
    });
    return before !== after;
  }

  function currentInsightPanelDate(): PanelDateState | null {
    if (!analytics.isPinned) {
      return panelDateState(analytics.from, analytics.to, {
        mode: "rolling",
        windowDays: analytics.windowDays,
      });
    }
    return panelDateState(analytics.from, analytics.to, {
      mode: "fixed",
    });
  }

  function paramsWithInsightDate(
    state: PanelDateState | null = currentInsightPanelDate(),
    extra: Record<string, string> = {},
  ): Record<string, string> {
    const nextParams = { ...router.params };
    for (const key of INSIGHTS_DATE_PARAM_KEYS) {
      delete nextParams[key];
    }
    if (state) {
      const range = panelStateToRange(state, Date.now());
      if (range) {
        Object.assign(nextParams, rangeToInsightParams(range));
      }
    }
    return { ...nextParams, ...extra };
  }

  function writeInsightDateParams(state: PanelDateState): void {
    router.replaceParams(paramsWithInsightDate(state));
  }

  function updateYokeFromInsights(state: PanelDateState | null): void {
    if (!state) return;
    yokedDates.updateFromPanel(state);
    writeInsightDateParams(state);
  }

  function seedInsightsYoke(): void {
    const urlState = insightParamsToPanelDate(router.params);
    if (urlState) {
      applyInsightPanelDate(urlState);
      yokedDates.updateFromPanel(urlState);
      return;
    }
    if (hasInsightDateParams(router.params)) return;

    const seed = yokedDates.seedForPanel();
    const state = seed ? rangeToPanelDate(seed) : null;
    if (!state) return;
    applyInsightPanelDate(state);
    writeInsightDateParams(state);
  }

  function fetchInsightSignals() {
    analytics.fetchSignalsForInsights();
    const state = currentInsightPanelDate();
    if (state?.mode === "rolling") {
      updateYokeFromInsights(state);
    }
  }

  function handleProjectChange(value: string) {
    analytics.project = value;
    fetchInsightSignals();
  }

  function handleAgentChange(value: string) {
    analytics.agent = value;
    fetchInsightSignals();
  }

  function handleInsightAgentChange(value: string) {
    insights.setAgent(value as AgentName);
  }

  function handleCannedKindChange(value: string) {
    insights.setCannedKind(value as CannedInsightKind);
  }

  function handleAutomatedScopeChange(value: string) {
    const scope = value as AutomatedScope;
    analytics.automatedScope = scope;
    analytics.includeAutomated = scope !== "human";
    fetchInsightSignals();
  }

  function handlePromptChange(e: Event) {
    const textarea = e.target as HTMLTextAreaElement;
    insights.promptText = textarea.value;
  }

  function effectiveAutomatedScope(): AutomatedScope {
    if (!analytics.includeAutomated) return "human";
    if (analytics.automatedScope === "human") return "all";
    return analytics.automatedScope;
  }

  function currentInsightFilters(): InsightGenerationFilters {
    const filters: InsightGenerationFilters = {
      timezone: analytics.timezone,
      include_one_shot: analytics.includeOneShot,
      automated_scope: effectiveAutomatedScope(),
    };
    if (analytics.machine) filters.machine = analytics.machine;
    if (analytics.agent) filters.agent = analytics.agent;
    if (analytics.termination) {
      filters.termination = analytics.termination;
    }
    if (analytics.minUserMessages > 0) {
      filters.min_user_messages = analytics.minUserMessages;
    }
    if (analytics.recentlyActive) {
      filters.active_since = new Date(
        Date.now() - 24 * 60 * 60 * 1000,
      ).toISOString();
    }
    return filters;
  }

  function handleGenerateCanned() {
    if (generationUnavailable) return;
    const filters = currentInsightFilters();
    insights.setType("llm_canned");
    insights.setDateFrom(analytics.from);
    insights.setDateTo(analytics.to);
    insights.setProject(analytics.project);
    insights.setAutomatedScope(filters.automated_scope ?? "human");
    insights.setSessionFilters(filters);
    insights.generate();
  }

  function handleRefresh() {
    fetchInsightSignals();
    insights.load();
  }

  function signalEvidenceKey(
    signal: string,
    params: AnalyticsParams,
  ): string {
    const entries = Object.entries(params)
      .filter(([, value]) => value !== undefined && value !== "")
      .sort(([a], [b]) => a.localeCompare(b));
    return JSON.stringify({ signal, params: Object.fromEntries(entries) });
  }

  async function openSignalEvidence(signal: string) {
    const params = analytics.signalEvidenceParams();
    const requestKey = signalEvidenceKey(signal, params);
    const request = ++signalExamplesRequest;
    selectedSignalId = signal;
    signalExamplesFilterKey = requestKey;
    signalExamplesLoading = true;
    signalExamplesError = null;
    try {
      const response = await AnalyticsService.getApiV1AnalyticsSignalSessions({
        ...params,
        signal,
        limit: 8,
      });
      if (
        selectedSignalId === signal &&
        signalExamplesFilterKey === requestKey &&
        signalExamplesRequest === request
      ) {
        signalExamples = response.sessions ?? [];
      }
    } catch (err) {
      if (
        selectedSignalId === signal &&
        signalExamplesFilterKey === requestKey &&
        signalExamplesRequest === request
      ) {
        signalExamples = [];
        signalExamplesError =
          err instanceof Error ? err.message : "Could not load examples";
      }
    } finally {
      if (
        selectedSignalId === signal &&
        signalExamplesFilterKey === requestKey &&
        signalExamplesRequest === request
      ) {
        signalExamplesLoading = false;
      }
    }
  }

  function openEvidenceSession(
    example: SignalSessionExample,
    event: MouseEvent,
  ) {
    event.preventDefault();
    const params = evidenceSessionParams(example);
    if (example.message_ordinal != null) {
      ui.scrollToOrdinal(example.message_ordinal, example.session_id);
    }
    sessions.navigateToSession(example.session_id);
    router.navigateToSession(example.session_id, params);
  }

  function evidenceSessionParams(
    example: SignalSessionExample,
  ): Record<string, string> {
    return example.message_ordinal == null
      ? {}
      : { msg: String(example.message_ordinal) };
  }

  function insightLinkPath(id: number): string {
    const params = new URLSearchParams();
    const dateParams = paramsWithInsightDate();
    if (Object.hasOwn(router.params, "desktop")) {
      params.set("desktop", router.params.desktop ?? "");
    }
    for (const [key, value] of Object.entries(dateParams)) {
      if (key !== "desktop" && key !== "insight") {
        params.set(key, value);
      }
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
    router.replaceParams(
      paramsWithInsightDate(currentInsightPanelDate(), {
        insight: String(id),
      }),
    );
  }

  function selectGeneratedTask(clientId: string) {
    insights.selectTask(clientId);
    const params = paramsWithInsightDate();
    delete params.insight;
    router.replaceParams(params);
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

  function calibrationFor(signal: string): SignalCalibration | null {
    return signals?.calibration?.[signal] ?? null;
  }

  function calibrationLabel(signal: string): string {
    if (signal.startsWith("outcome_")) {
      return "Outcome cohort";
    }
    const calibration = calibrationFor(signal);
    if (!calibration) {
      return "Examples only";
    }
    if (calibration.affected_sessions === 0) {
      return "No affected sessions";
    }
    if (calibration.incomplete_lift == null) {
      return `${calibration.affected_incomplete_rate}% incomplete`;
    }
    return `${calibration.incomplete_lift.toFixed(1)}x incomplete`;
  }

  function selectedSignalLabel(): string {
    if (!selectedSignalId) return "Signal examples";
    for (const pattern of patterns) {
      const found = pattern.drivers.find(
        (driver) => driver.id === selectedSignalId,
      );
      if (found) return found.label;
    }
    return selectedSignalId;
  }

  function qualityBadge(example: SignalSessionExample): string {
    if (example.health_grade) return `Grade ${example.health_grade}`;
    if (example.health_score != null) return String(example.health_score);
    return "Unscored";
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
    seedInsightsYoke();
    sessions.loadProjects();
    sessions.loadAgents();
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
    const headerAutomatedScope = headerIncludeAutomated
      ? "all"
      : "human";

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
        headerIncludeAutomated ||
      untrack(() => analytics.automatedScope) !==
        headerAutomatedScope;

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
      analytics.automatedScope = headerAutomatedScope;
      untrack(() => fetchInsightSignals());
    }
  });

  onDestroy(() => {
    if (refreshTimer !== undefined) clearInterval(refreshTimer);
    clearTimeout(copiedInsightLinkTimer);
    unsubEvents?.();
  });

  $effect(() => {
    const signal = selectedSignalId;
    if (!signal) return;
    const params = analytics.signalEvidenceParams();
    const nextKey = signalEvidenceKey(signal, params);
    if (signalExamplesFilterKey === nextKey) return;
    signalExamples = [];
    signalExamplesError = null;
    untrack(() => void openSignalEvidence(signal));
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
    <RangePicker
      selection={rangeSelection}
      busy={loading}
      {earliestSession}
      onSelect={applyRange}
    />

    <div class="filter-group">
      <ProjectTypeahead
        projects={sessions.projects}
        value={analytics.project}
        onselect={handleProjectChange}
      />
      <OptionTypeahead
        options={agentOptions}
        value={analytics.agent}
        fallbackLabel={analytics.agent
          ? agentLabel(analytics.agent)
          : "All Agents"}
        placeholder="Filter agents..."
        title="Filter insights by agent"
        emptyLabel="No matching agents"
        onselect={handleAgentChange}
      />
      <label class="toolbar-scope">
        <span>Session scope</span>
        <OptionTypeahead
          options={scopeOptions}
          value={analytics.automatedScope}
          fallbackLabel="No automated"
          placeholder="Filter scopes..."
          title="Filter insights by session scope"
          emptyLabel="No matching scopes"
          onselect={handleAutomatedScopeChange}
        />
      </label>
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
                  <button
                    class="driver-row"
                    class:active={selectedSignalId === driver.id}
                    type="button"
                    onclick={() => openSignalEvidence(String(driver.id))}
                  >
                    <span>{driver.label}</span>
                    <strong>
                      {driver.total}{driver.unit ?? ""}
                    </strong>
                    <em>{driver.sessions} sessions</em>
                    <small>{calibrationLabel(String(driver.id))}</small>
                  </button>
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

        {#if selectedSignalId}
          <section class="evidence-panel" aria-live="polite">
            <div class="evidence-head">
              <div>
                <span class="examples-label">Session evidence</span>
                <h3>{selectedSignalLabel()}</h3>
              </div>
              <button
                class="text-btn"
                type="button"
                onclick={() => {
                  selectedSignalId = null;
                  signalExamples = [];
                  signalExamplesError = null;
                  signalExamplesFilterKey = null;
                }}
              >
                Close
              </button>
            </div>
            {#if signalExamplesLoading}
              <p class="evidence-state">Loading examples...</p>
            {:else if signalExamplesError}
              <p class="evidence-state error">{signalExamplesError}</p>
            {:else if signalExamples.length === 0}
              <p class="evidence-state">
                No sessions currently trigger this signal in the selected filters.
              </p>
            {:else}
              <div class="evidence-list">
                {#each signalExamples as example}
                  <a
                    class="evidence-row"
                    href={router.buildSessionHref(
                      example.session_id,
                      evidenceSessionParams(example),
                    )}
                    onclick={(event) =>
                      openEvidenceSession(example, event)}
                  >
                    <span class="evidence-main">
                      <strong>{example.project || "Unassigned project"}</strong>
                      <em>{example.excerpt || "No prompt excerpt available"}</em>
                    </span>
                    <span class="evidence-meta">
                      <span>{agentLabel(example.agent)}</span>
                      <span>{example.outcome || "unknown"}</span>
                      <span>{qualityBadge(example)}</span>
                      <span>{example.failure_signals} failures</span>
                    </span>
                  </a>
                {/each}
              </div>
            {/if}
          </section>
        {/if}
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
          <OptionTypeahead
            options={templateOptions}
            value={insights.cannedKind}
            fallbackLabel={cannedKindLabel(insights.cannedKind)}
            placeholder="Filter templates..."
            title="Select template"
            emptyLabel="No matching templates"
            onselect={handleCannedKindChange}
          />
        </label>

        <label class="generated-control">
          <span>Generator</span>
          <OptionTypeahead
            options={generationAgentOptions}
            value={insights.agent}
            fallbackLabel={agentLabel(insights.agent)}
            placeholder="Filter generators..."
            title="Select generator"
            emptyLabel="No matching generators"
            onselect={handleInsightAgentChange}
          />
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
              <div class="generated-detail-head">
                <span class="badge generated">
                  {insights.selectedTask.status === "error"
                    ? "Generation error"
                    : "Generating"}
                </span>
                {#if insights.selectedTask.status === "error"}
                  <div class="generated-actions">
                    <button
                      class="text-btn"
                      type="button"
                      onclick={() =>
                        insights.retryTask(
                          insights.selectedTask!.clientId,
                        )}
                    >
                      Retry
                    </button>
                    <button
                      class="icon-action danger"
                      type="button"
                      onclick={() =>
                        insights.dismissTask(
                          insights.selectedTask!.clientId,
                        )}
                      title="Dismiss failed generation"
                      aria-label="Dismiss failed generation"
                    >
                      <svg width="14" height="14" viewBox="0 0 16 16" fill="currentColor" aria-hidden="true">
                        <path d="M5.5 5.5A.5.5 0 016 6v6a.5.5 0 01-1 0V6a.5.5 0 01.5-.5zm2.5 0a.5.5 0 01.5.5v6a.5.5 0 01-1 0V6a.5.5 0 01.5-.5zm3 .5a.5.5 0 00-1 0v6a.5.5 0 001 0V6z"/>
                        <path fill-rule="evenodd" d="M14.5 3a1 1 0 01-1 1H13v9a2 2 0 01-2 2H5a2 2 0 01-2-2V4h-.5a1 1 0 01-1-1V2a1 1 0 011-1H5.5l1-1h3l1 1h2.5a1 1 0 011 1v1zM4.118 4L4 4.059V13a1 1 0 001 1h6a1 1 0 001-1V4.059L11.882 4H4.118zM2.5 3V2h11v1h-11z"/>
                      </svg>
                    </button>
                  </div>
                {/if}
              </div>
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
                    class="icon-action danger"
                    type="button"
                    onclick={() => {
                      if (insights.selectedItem) {
                        insights.deleteItem(insights.selectedItem.id);
                        const params = paramsWithInsightDate();
                        delete params.insight;
                        router.replaceParams(params);
                      }
                    }}
                    title="Delete generated insight"
                    aria-label="Delete generated insight"
                  >
                    <svg width="14" height="14" viewBox="0 0 16 16" fill="currentColor" aria-hidden="true">
                      <path d="M5.5 5.5A.5.5 0 016 6v6a.5.5 0 01-1 0V6a.5.5 0 01.5-.5zm2.5 0a.5.5 0 01.5.5v6a.5.5 0 01-1 0V6a.5.5 0 01.5-.5zm3 .5a.5.5 0 00-1 0v6a.5.5 0 001 0V6z"/>
                      <path fill-rule="evenodd" d="M14.5 3a1 1 0 01-1 1H13v9a2 2 0 01-2 2H5a2 2 0 01-2-2V4h-.5a1 1 0 01-1-1V2a1 1 0 011-1H5.5l1-1h3l1 1h2.5a1 1 0 011 1v1zM4.118 4L4 4.059V13a1 1 0 001 1h6a1 1 0 001-1V4.059L11.882 4H4.118zM2.5 3V2h11v1h-11z"/>
                    </svg>
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
    flex-wrap: wrap;
    gap: 12px;
    padding: 8px 16px;
    background: var(--bg-surface);
    border-bottom: 1px solid var(--border-muted);
    flex-shrink: 0;
    min-height: 45px;
  }

  .filter-group {
    display: flex;
    align-items: center;
    flex-wrap: wrap;
    gap: 8px;
    flex: 1 1 560px;
    min-width: 0;
    max-width: 720px;
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

  .toolbar-scope {
    display: flex;
    align-items: center;
    gap: 6px;
    flex: 0 0 220px;
    min-width: 220px;
  }

  .toolbar-scope span {
    color: var(--text-muted);
    font-size: 10px;
    font-weight: 700;
    letter-spacing: 0.04em;
    text-transform: uppercase;
    white-space: nowrap;
  }

  .filter-group :global(.typeahead),
  .toolbar-scope :global(.typeahead),
  .generated-control :global(.typeahead) {
    min-width: 0;
    max-width: none;
    width: 100%;
  }

  .filter-group > :global(.typeahead:first-child) {
    --typeahead-list-min-width: min(360px, calc(100vw - 32px));
    flex: 0 1 220px;
    min-width: 180px;
    max-width: 260px;
  }

  .filter-group > :global(.typeahead:nth-child(2)) {
    flex: 0 0 120px;
  }

  .toolbar-scope :global(.typeahead) {
    flex: 0 0 128px;
    width: 128px;
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
    grid-template-columns: minmax(0, 1fr) auto auto auto;
    gap: 10px;
    align-items: baseline;
    width: 100%;
    min-height: 24px;
    padding: 2px 4px;
    border-radius: var(--radius-sm);
    font-size: 12px;
    text-align: left;
  }

  .driver-row:hover,
  .driver-row.active {
    background: var(--bg-surface-hover);
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

  .driver-row small {
    color: var(--text-muted);
    font-size: 10px;
    font-variant-numeric: tabular-nums;
    white-space: nowrap;
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

  .evidence-panel {
    display: grid;
    gap: 10px;
    padding: 12px;
    background: var(--bg-surface);
    border: 1px solid var(--border-muted);
    border-radius: var(--radius-md);
  }

  .evidence-head {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: 12px;
  }

  .evidence-head h3 {
    margin-top: 2px;
    color: var(--text-primary);
    font-size: 14px;
  }

  .evidence-state {
    color: var(--text-secondary);
    font-size: 12px;
  }

  .evidence-state.error {
    color: var(--accent-red);
  }

  .evidence-list {
    display: grid;
    gap: 6px;
  }

  .evidence-row {
    display: grid;
    grid-template-columns: minmax(0, 1fr) auto;
    gap: 12px;
    align-items: center;
    padding: 9px 10px;
    border: 1px solid var(--border-muted);
    border-radius: var(--radius-sm);
    background: var(--bg-inset);
    text-decoration: none;
  }

  .evidence-row:hover {
    border-color: var(--border-default);
    background: var(--bg-surface-hover);
  }

  .evidence-main {
    display: grid;
    gap: 2px;
    min-width: 0;
  }

  .evidence-main strong {
    color: var(--text-primary);
    font-size: 12px;
  }

  .evidence-main em {
    color: var(--text-muted);
    font-size: 12px;
    font-style: normal;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  .evidence-meta {
    display: flex;
    flex-wrap: wrap;
    justify-content: flex-end;
    gap: 6px;
    color: var(--text-muted);
    font-size: 11px;
    font-variant-numeric: tabular-nums;
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
    grid-template-columns:
      minmax(180px, 220px) minmax(130px, 160px)
      minmax(240px, 1fr) auto;
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

  .generated-focus {
    width: 100%;
    border: 1px solid var(--border-muted);
    background: var(--bg-inset);
    border-radius: var(--radius-sm);
    color: var(--text-primary);
    font-size: 12px;
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

  .icon-action {
    display: inline-flex;
    align-items: center;
    justify-content: center;
    width: 26px;
    height: 26px;
    border: 1px solid var(--border-muted);
    border-radius: var(--radius-sm);
    background: var(--bg-inset);
    color: var(--text-muted);
    cursor: pointer;
    flex-shrink: 0;
    transition:
      background 0.15s,
      border-color 0.15s,
      color 0.15s,
      transform 0.08s;
  }

  .icon-action:hover {
    background: var(--bg-surface-hover);
    border-color: var(--border-default);
    color: var(--text-primary);
  }

  .icon-action.danger:hover {
    color: var(--accent-red);
  }

  .icon-action:active {
    transform: scale(0.94);
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

    .filter-group {
      flex: 0 1 auto;
      min-width: 0;
      width: 100%;
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

    .driver-row,
    .evidence-row {
      grid-template-columns: 1fr;
    }

    .evidence-meta {
      justify-content: flex-start;
    }
  }
</style>
