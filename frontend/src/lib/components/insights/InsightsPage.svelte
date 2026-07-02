<script lang="ts">
  import { m } from "../../i18n/index.js";
  import { onMount, onDestroy, untrack } from "svelte";
  import { analytics } from "../../stores/analytics.svelte.js";
  import { insights } from "../../stores/insights.svelte.js";
  import { ui } from "../../stores/ui.svelte.js";
  import { getBasePath, router } from "../../stores/router.svelte.js";
  import { sessions } from "../../stores/sessions.svelte.js";
  import { sync } from "../../stores/sync.svelte.js";
  import { events } from "../../stores/events.svelte.js";
  import { downloadInsightExport } from "../../api/client.js";
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
  import { CopyButton, IconButton, Typeahead } from "@kenn-io/kit-ui";
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
  const insightGenerationAvailable = $derived(
    sync.serverVersion?.insight_generation_available === true ||
      sync.serverVersion?.read_only !== true,
  );
  const generationUnavailable = $derived(
    sync.serverVersion === null || !insightGenerationAvailable,
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
        label: m.insights_page_all_agents(),
        displayLabel: m.insights_page_all_agents(),
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
  const templateOptions = $derived([
    { name: "prompt_maturity_review", label: m.insights_page_template_prompt_maturity() },
    { name: "context_setup_review", label: m.insights_page_template_context_setup() },
    { name: "workflow_hygiene_review", label: m.insights_page_template_workflow_hygiene() },
    { name: "tool_reliability_review", label: m.insights_page_template_tool_reliability() },
    { name: "model_cost_review", label: m.insights_page_template_model_cost() },
    {
      name: "instruction_opportunity_review",
      label: m.insights_page_template_instruction_opportunities(),
    },
  ]);
  const scopeOptions = $derived([
    { name: "human", label: m.insights_page_scope_no_automated() },
    { name: "all", label: m.insights_page_scope_both() },
    { name: "automated", label: m.insights_page_scope_only_automated() },
  ]);

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
          err instanceof Error ? err.message : m.insights_page_could_not_load_examples();
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
    return m.insights_page_date_range({ from: formatDate(from), to: formatDate(to) });
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
        return m.insights_page_severity_critical();
      case "warning":
        return m.insights_page_severity_warning();
      case "watch":
        return m.insights_page_severity_watch();
      case "clear":
        return m.insights_page_severity_clear();
      case "unavailable":
        return m.insights_page_severity_unavailable();
    }
  }

  function affectedLabel(pattern: QualityPatternView): string {
    if (pattern.totalSessions === 0) return m.insights_page_no_computed_sessions();
    return m.insights_page_affected_sessions({ affected: pattern.affectedSessions, total: pattern.totalSessions });
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
      return m.insights_page_calibration_outcome_cohort();
    }
    const calibration = calibrationFor(signal);
    if (!calibration) {
      return m.insights_page_calibration_examples_only();
    }
    if (calibration.affected_sessions === 0) {
      return m.insights_page_calibration_no_affected();
    }
    if (calibration.incomplete_lift == null) {
      return m.insights_page_calibration_incomplete_rate({ rate: calibration.affected_incomplete_rate });
    }
    return m.insights_page_calibration_incomplete_lift({ lift: calibration.incomplete_lift.toFixed(1) });
  }

  function selectedSignalLabel(): string {
    if (!selectedSignalId) return m.insights_page_signal_examples();
    for (const pattern of patterns) {
      const found = pattern.drivers.find(
        (driver) => driver.id === selectedSignalId,
      );
      if (found) return found.label;
    }
    return selectedSignalId;
  }

  function qualityBadge(example: SignalSessionExample): string {
    if (example.health_grade) return m.insights_page_grade_badge({ grade: example.health_grade });
    if (example.health_score != null) return String(example.health_score);
    return m.insights_page_unscored();
  }

  function cannedKindLabel(
    kind: CannedInsightKind | "" | undefined,
  ): string {
    switch (kind) {
      case "prompt_maturity_review":
        return m.insights_page_template_prompt_maturity();
      case "context_setup_review":
        return m.insights_page_template_context_setup();
      case "workflow_hygiene_review":
        return m.insights_page_template_workflow_hygiene();
      case "tool_reliability_review":
        return m.insights_page_template_tool_reliability();
      case "model_cost_review":
        return m.insights_page_template_model_cost();
      case "instruction_opportunity_review":
        return m.insights_page_template_instruction_opportunities();
      default:
        return m.insights_page_generated_recommendation();
    }
  }

  function insightTypeLabel(
    type: InsightType,
    kind: CannedInsightKind | "" | undefined,
  ): string {
    if (type === "llm_canned") return cannedKindLabel(kind);
    if (type === "agent_analysis") return m.insights_page_agent_analysis();
    return m.insights_page_activity();
  }

  function cacheStatusLabel(status: string | undefined): string {
    if (status === "hit") return m.insights_page_cache_hit();
    if (status === "fresh") return m.insights_page_fresh();
    return "";
  }

  async function handleInsightExport() {
    if (!insights.selectedItem) return;
    try {
      await downloadInsightExport(insights.selectedItem.id);
    } catch (error) {
      console.error("Insight export failed:", error);
    }
  }

  function openInsightPublish(secret: boolean) {
    if (!insights.selectedItem) return;
    ui.publishSecret = secret;
    ui.setPublishTarget({
      kind: "insight",
      id: insights.selectedItem.id,
    });
    ui.activeModal = "publish";
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
      <Typeahead
        options={agentOptions}
        value={analytics.agent}
        fallbackLabel={analytics.agent
          ? agentLabel(analytics.agent)
          : m.insights_page_all_agents()}
        placeholder={m.insights_page_filter_agents()}
        title={m.insights_page_filter_by_agent()}
        emptyLabel={m.insights_page_no_matching_agents()}
        onselect={handleAgentChange}
      />
      <label class="toolbar-scope">
        <span>{m.insights_page_session_scope()}</span>
        <Typeahead
          options={scopeOptions}
          value={analytics.automatedScope}
          fallbackLabel={m.insights_page_scope_no_automated()}
          placeholder={m.insights_page_filter_scopes()}
          title={m.insights_page_filter_by_scope()}
          emptyLabel={m.insights_page_no_matching_scopes()}
          onselect={handleAutomatedScopeChange}
        />
      </label>
    </div>

    <IconButton
      class="toolbar-refresh"
      onclick={handleRefresh}
      title={m.insights_page_refresh()}
      ariaLabel={m.insights_page_refresh()}
    >
      <svg width="14" height="14" viewBox="0 0 16 16" fill="currentColor">
        <path d="M8 3a5 5 0 00-4.546 2.914.5.5 0 01-.908-.418A6 6 0 0114 8a.5.5 0 01-1 0 5 5 0 00-5-5zm4.546 7.086a.5.5 0 01.908.418A6 6 0 012 8a.5.5 0 011 0 5 5 0 005 5 5 5 0 004.546-2.914z"/>
      </svg>
    </IconButton>
  </header>

  <main class="content">
    <section class="section-block" aria-labelledby="actions-title">
      <div class="section-heading compact">
        <div>
          <div class="eyebrow">
            <span class="badge rule">{m.insights_page_rule_based()}</span>
            <span>{m.insights_page_next_actions()}</span>
          </div>
          <h2 id="actions-title">{m.insights_page_deterministic_recommendations()}</h2>
        </div>
        <p class="insights-help">
          {m.insights_page_insights_help_intro()}
          <a
            href="https://www.agentsview.io/insights/"
            target="_blank"
            rel="noopener noreferrer"
            class="insights-help-link"
          >
            {m.insights_page_insights_help_docs()}
          </a>
        </p>
      </div>

      {#if recommendations.length === 0}
        <div class="state-panel compact-state">
          <strong>{m.insights_page_no_rule_actions()}</strong>
          <span>
            {m.insights_page_patterns_clear()}
          </span>
        </div>
      {:else}
        <div class="recommendation-list">
          {#each recommendations as rec}
            <article class="recommendation">
              <span class="badge rule">{m.insights_page_rule_based()}</span>
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
            <span class="badge rule">{m.insights_page_rule_based()}</span>
            <span>{m.insights_page_scored_facts()}</span>
          </div>
          <h2 id="facts-title">{m.insights_page_quality_patterns()}</h2>
        </div>
        <p>
          {m.insights_page_deterministic_counts({ range: formatDateRange(analytics.from, analytics.to) })}
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
          <strong>{m.insights_page_could_not_load()}</strong>
          <span>{error}</span>
          <button onclick={fetchInsightSignals}>
            {m.insights_page_retry()}
          </button>
        </div>
      {:else if !hasData}
        <div class="state-panel">
          <strong>{m.insights_page_no_scored_data()}</strong>
          <span>
            {m.insights_page_no_scored_data_hint()}
          </span>
        </div>
      {:else}
        {#if error}
          <div class="inline-warning" role="status">
            {m.insights_page_cached_warning({ error })}
          </div>
        {/if}

        <div class="summary-grid">
          <article class="summary-card">
            <span class="label">{m.insights_page_average_score()}</span>
            <strong>
              {summary.avgHealthScore == null
                ? "--"
                : Math.round(summary.avgHealthScore)}
            </strong>
            <span>
              {summary.avgHealthScore == null
                ? m.insights_page_no_scored_sessions()
                : m.insights_page_grade_badge({ grade: scoreToGrade(summary.avgHealthScore) })}
            </span>
          </article>
          <article class="summary-card">
            <span class="label">{m.insights_page_scored_sessions()}</span>
            <strong>{summary.scoredSessions}</strong>
            <span>{m.insights_page_unscored_count({ count: summary.unscoredSessions })}</span>
          </article>
          <article class="summary-card">
            <span class="label">{m.insights_page_low_quality()}</span>
            <strong>{summary.lowQualitySessions}</strong>
            <span>{m.insights_page_df_graded()}</span>
          </article>
          <article class="summary-card">
            <span class="label">{m.insights_page_prompt_signals()}</span>
            <strong>{summary.computedQualitySessions}</strong>
            <span>{m.insights_page_sessions_computed()}</span>
          </article>
        </div>

        <div class="distribution-row" aria-label={m.insights_page_score_distribution()}>
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
                  {m.insights_page_pct_affected({ pct: pct(pattern.affectedSessions, pattern.totalSessions) })}
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
                    <em>{m.insights_page_driver_sessions({
                      count: driver.sessions,
                      countLabel: driver.sessions.toLocaleString(),
                    })}</em>
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
                <span class="examples-label">{m.insights_page_session_evidence()}</span>
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
                {m.insights_page_close()}
              </button>
            </div>
            {#if signalExamplesLoading}
              <p class="evidence-state">{m.insights_page_loading_examples()}</p>
            {:else if signalExamplesError}
              <p class="evidence-state error">{signalExamplesError}</p>
            {:else if signalExamples.length === 0}
              <p class="evidence-state">
                {m.insights_page_no_triggering_sessions()}
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
                      <strong>{example.project || m.insights_page_unassigned_project()}</strong>
                      <em>{example.excerpt || m.insights_page_no_excerpt()}</em>
                    </span>
                    <span class="evidence-meta">
                      <span>{agentLabel(example.agent)}</span>
                      <span>{example.outcome || m.insights_page_unknown()}</span>
                      <span>{qualityBadge(example)}</span>
                      <span>{m.insights_page_failures({ count: example.failure_signals })}</span>
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
            <span class="badge generated">{m.insights_page_generated()}</span>
            <span>{m.insights_page_separate_from_facts()}</span>
          </div>
          <h2 id="generated-title">{m.insights_page_generated_archive()}</h2>
        </div>
        <p>
          {m.insights_page_generated_archive_hint()}
        </p>
      </div>

      <div class="generated-controls">
        <label class="generated-control">
          <span>{m.insights_page_template_label()}</span>
          <Typeahead
            options={templateOptions}
            value={insights.cannedKind}
            fallbackLabel={cannedKindLabel(insights.cannedKind)}
            placeholder={m.insights_page_filter_templates()}
            title={m.insights_page_select_template()}
            emptyLabel={m.insights_page_no_matching_templates()}
            onselect={handleCannedKindChange}
          />
        </label>

        <label class="generated-control">
          <span>{m.insights_page_generator_label()}</span>
          <Typeahead
            options={generationAgentOptions}
            value={insights.agent}
            fallbackLabel={agentLabel(insights.agent)}
            placeholder={m.insights_page_filter_generators()}
            title={m.insights_page_select_generator()}
            emptyLabel={m.insights_page_no_matching_generators()}
            onselect={handleInsightAgentChange}
          />
        </label>

        <label class="generated-control focus-control">
          <span>{m.insights_page_optional_focus()}</span>
          <textarea
            class="generated-focus"
            value={insights.promptText}
            maxlength="1200"
            rows="2"
            placeholder={m.insights_page_focus_placeholder()}
            oninput={handlePromptChange}
          ></textarea>
        </label>

        <button
          class="generate-action"
          disabled={generationUnavailable}
          title={sync.serverVersion !== null && !insightGenerationAvailable
            ? m.insights_page_generate_disabled()
            : m.insights_page_generate_title()}
          onclick={handleGenerateCanned}
        >
          {m.insights_page_generate()}
        </button>
      </div>

      {#if insights.loading}
        <div class="state-panel compact-state">{m.insights_page_loading_archive()}</div>
      {:else if insights.items.length === 0 && insights.tasks.length === 0}
        <div class="state-panel compact-state">
          <strong>{m.insights_page_no_generated_saved()}</strong>
          <span>
            {m.insights_page_no_generated_hint()}
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
                <span>{task.status === "error" ? m.insights_page_error() : m.insights_page_running()}</span>
                <strong>{task.project || m.insights_page_global()}</strong>
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
                <strong>{item.project || m.insights_page_global()}</strong>
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
                    ? m.insights_page_generation_error()
                    : m.insights_page_generating()}
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
                      {m.insights_page_retry()}
                    </button>
                    <button
                      class="icon-action danger"
                      type="button"
                      onclick={() =>
                        insights.dismissTask(
                          insights.selectedTask!.clientId,
                        )}
                      title={m.insights_page_dismiss_failed()}
                      aria-label={m.insights_page_dismiss_failed()}
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
                        {m.insights_page_template_version({ version: insights.selectedItem.template_version })}
                      </span>
                    {/if}
                    {#if insights.selectedItem.aggregate_hash}
                      <span class="detail-chip muted">
                        {m.insights_page_aggregate_hash({ hash: insights.selectedItem.aggregate_hash.slice(0, 12) })}
                      </span>
                    {/if}
                  {/if}
                </div>
                <div class="generated-actions">
                  <button
                    class="header-action"
                    type="button"
                    onclick={handleInsightExport}
                  >
                    Export
                  </button>
                  <button
                    class="header-action"
                    type="button"
                    onclick={() => openInsightPublish(false)}
                  >
                    Publish
                  </button>
                  <button
                    class="header-action subtle"
                    type="button"
                    onclick={() => openInsightPublish(true)}
                  >
                    Secret
                  </button>
                  <CopyButton
                    class="insight-link-copy"
                    copied={copiedInsightLinkId === insights.selectedItem.id}
                    ariaLabel={m.insights_page_copy_link()}
                    copiedAriaLabel={m.insights_page_copied_link()}
                    title={m.insights_page_copy_link_title()}
                    copiedTitle={m.insights_page_copied_link_short()}
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
                    title={m.insights_page_delete_insight()}
                    aria-label={m.insights_page_delete_insight()}
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
              <p>{m.insights_page_select_to_read()}</p>
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

  .filter-group :global(.kit-typeahead),
  .toolbar-scope :global(.kit-typeahead),
  .generated-control :global(.kit-typeahead) {
    min-width: 0;
    max-width: none;
    width: 100%;
  }

  /* The kit-ui Typeahead list pins to the trigger width, so size the
     trigger itself (the old --typeahead-list-min-width knob is retired). */
  .filter-group > :global(.kit-typeahead:first-child) {
    flex: 0 1 220px;
    min-width: 180px;
    max-width: 260px;
  }

  .filter-group > :global(.kit-typeahead:nth-child(2)) {
    flex: 0 0 120px;
  }

  .toolbar-scope :global(.kit-typeahead) {
    flex: 0 0 128px;
    width: 128px;
  }

  :global(.toolbar-refresh.kit-icon-button) {
    margin-left: auto;
  }

  .content {
    flex: 1;
    min-height: 0;
    overflow-y: auto;
    padding: 18px;
    display: flex;
    flex-direction: column;
    gap: var(--space-6);
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

  .section-heading p.insights-help {
    max-width: 58ch;
    margin: 0;
    display: flex;
    flex-wrap: wrap;
    justify-content: flex-end;
    gap: 4px;
    text-align: right;
    line-height: 1.35;
  }

  .insights-help-link {
    color: var(--accent-blue);
  }

  .insights-help-link:hover {
    color: color-mix(in srgb, var(--accent-blue) 70%, var(--text-primary));
    text-underline-offset: 2px;
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
    gap: var(--space-5);
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
    gap: var(--space-4);
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
    gap: var(--space-4);
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
    gap: var(--space-1);
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
    gap: var(--space-4);
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
    gap: var(--space-5);
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
    gap: var(--space-5);
  }

  .recommendation {
    padding: 12px;
    display: grid;
    gap: var(--space-3);
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

  /* The generated-archive grids have hard minimum column widths (controls:
     180+130+240px; layout: a 240px list rail), so they collapse on available
     CONTENT width via a container query rather than the viewport-width media
     gate below — with the sidebar open, a ~950px viewport leaves far less
     room than a viewport breakpoint assumes. */
  .generated-block {
    container-type: inline-size;
  }

  @container (max-width: 760px) {
    .generated-controls,
    .generated-layout {
      grid-template-columns: 1fr;
    }
  }

  .generated-controls {
    display: grid;
    grid-template-columns:
      minmax(180px, 220px) minmax(130px, 160px)
      minmax(240px, 1fr) auto;
    gap: var(--space-5);
    align-items: end;
    padding: 12px;
    background: var(--bg-surface);
    border: 1px solid var(--border-muted);
    border-radius: var(--radius-md);
  }

  .generated-control {
    display: grid;
    gap: var(--space-2);
    min-width: 0;
  }

  .generated-control span {
    color: var(--text-muted);
    font-size: 10px;
    font-weight: 700;
    letter-spacing: 0.04em;
    text-transform: uppercase;
  }

  .badge-blue {
    background: var(--accent-blue);
  }

  .badge-purple {
    background: var(--accent-purple);
  }

  .badge-red {
    background: var(--accent-red);
  }

  .header-date {
    font-size: 15px;
    font-weight: 600;
    color: var(--text-primary);
    letter-spacing: -0.01em;
  }

  .delete-btn {
    width: 28px;
    height: 28px;
    display: flex;
    align-items: center;
    justify-content: center;
    border-radius: var(--radius-sm);
    color: var(--text-primary);
    font-size: 12px;
  }

  .generated-focus {
    width: 100%;
    min-height: 30px;
    max-height: 76px;
    border: 1px solid var(--border-muted);
    background: var(--bg-inset);
    padding: 7px 8px;
    resize: vertical;
    line-height: 1.35;
  }

  .generate-action {
    height: 30px;
    padding: 0 12px;
    background: var(--accent-purple);
    color: var(--accent-purple-foreground);
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

  .header-actions {
    display: flex;
    align-items: center;
    gap: 8px;
  }

  .header-action {
    height: 28px;
    padding: 0 10px;
    border-radius: var(--radius-sm);
    font-size: 11px;
    font-weight: 600;
    color: var(--text-secondary);
    background: var(--bg-inset);
    border: 1px solid var(--border-muted);
    transition: background 0.12s, color 0.12s,
      border-color 0.12s;
  }

  .header-action:hover {
    background: var(--bg-surface-hover);
    color: var(--text-primary);
    border-color: var(--border-default);
  }

  .header-action.subtle {
    color: var(--text-muted);
  }

  .delete-btn:hover {
    background: color-mix(
      in srgb,
      var(--accent-red) 10%,
      transparent
    );
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

  .generated-actions :global(.insight-link-copy.kit-copy-btn) {
    border: 1px solid var(--border-muted);
    background: var(--bg-inset);
  }

  .generated-actions :global(.insight-link-copy.kit-copy-btn:hover) {
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
    color: var(--accent-blue-foreground);
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

  @media (max-width: 900px) {
    .toolbar,
    .section-heading {
      align-items: stretch;
      flex-direction: column;
    }

    :global(.toolbar-refresh.kit-icon-button) {
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

    .section-heading p.insights-help {
      justify-content: flex-start;
      text-align: left;
    }

    .summary-grid,
    .pattern-grid,
    .recommendation-list {
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
