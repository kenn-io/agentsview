<script lang="ts">
  import { m } from "../../i18n/index.js";
  import { onMount, onDestroy, untrack } from "svelte";
  import { analytics } from "../../stores/analytics.svelte.js";
  import { analyticsPageDates } from "../../stores/analyticsPageDates.js";
  import { ui } from "../../stores/ui.svelte.js";
  import { router } from "../../stores/router.svelte.js";
  import { sessions } from "../../stores/sessions.svelte.js";
  import { sync } from "../../stores/sync.svelte.js";
  import { events } from "../../stores/events.svelte.js";
  import {
    yokedDates,
    panelDateState,
    panelStateToRange,
    rangeToInsightParams,
    rangeToPanelDate,
    type PanelDateState,
  } from "../../stores/yokedDates.svelte.js";
  import { rollingRange } from "../../utils/dates.js";
  import { scoreToGrade } from "../../utils/grade.js";
  import { agentLabel } from "../../utils/agents.js";
  import { AnalyticsService } from "../../api/generated/index.js";
  import { callGenerated, isAbortError } from "../../api/runtime.js";
  import { LatestRead } from "../../utils/latest-read.js";
  import type {
    AutomatedScope,
    SignalCalibration,
    SignalSessionExample,
  } from "../../api/types.js";
  import { Card, Typeahead } from "@kenn-io/kit-ui";
  import ProjectTypeahead from "../layout/ProjectTypeahead.svelte";
  import RangePicker from "../shared/RangePicker.svelte";
  import RefreshControl from "../shared/RefreshControl.svelte";
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

  const QUALITY_WINDOW_PARAM = "window_days";
  const QUALITY_DATE_PARAM_KEYS = [
    QUALITY_WINDOW_PARAM,
    "date_from",
    "date_to",
  ] as const;
  type AnalyticsParams = Parameters<
    typeof AnalyticsService.getApiV1AnalyticsSignals
  >[0];

  let unsubEvents: (() => void) | undefined;
  let selectedSignalId: string | null = $state(null);
  let signalExamples: SignalSessionExample[] = $state([]);
  let signalExamplesLoading = $state(false);
  let signalExamplesError: string | null = $state(null);
  let signalExamplesFilterKey: string | null = $state(null);
  let signalExamplesRequest = 0;
  const signalEvidenceRead = new LatestRead();
  // A materialized page default is not date intent. Only picker input, a
  // dated URL, or a shared seed may serialize dates back into the URL.
  let qualityDateIntentEstablished = false;

  const signals = $derived(analytics.signals);
  const summary = $derived(buildQualitySummary(signals));
  const patterns = $derived(buildQualityPatterns(signals));
  const recommendations = $derived(
    buildRuleBasedRecommendations(patterns),
  );
  const loading = $derived(analytics.loading.signals);
  const querying = $derived(analytics.querying.signals);
  const error = $derived(analytics.errors.signals);
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
    updateYokeFromQuality(state);
  }

  function parseQualityWindowDays(raw: string | undefined): number | null {
    if (!raw) return null;
    const n = Number.parseInt(raw, 10);
    if (!Number.isInteger(n) || n <= 0 || String(n) !== raw) {
      return null;
    }
    return n;
  }

  function qualityParamsToPanelDate(
    params: Record<string, string>,
  ): PanelDateState | null {
    const windowDays = parseQualityWindowDays(params[QUALITY_WINDOW_PARAM]);
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

  function hasQualityDateParams(
    params: Record<string, string>,
  ): boolean {
    return !!params.date_from || !!params.date_to ||
      !!params[QUALITY_WINDOW_PARAM];
  }

  function applyQualityPanelDate(state: PanelDateState): boolean {
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

  function currentQualityPanelDate(): PanelDateState | null {
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

  function paramsWithQualityDate(
    state: PanelDateState | null = qualityDateIntentEstablished
      ? currentQualityPanelDate()
      : null,
    extra: Record<string, string> = {},
  ): Record<string, string> {
    const nextParams = { ...router.params };
    for (const key of QUALITY_DATE_PARAM_KEYS) {
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

  function writeQualityDateParams(state: PanelDateState): void {
    router.replaceParams(paramsWithQualityDate(state));
  }

  function updateYokeFromQuality(state: PanelDateState | null): void {
    if (!state) return;
    qualityDateIntentEstablished = true;
    yokedDates.updateFromPanel(state);
    writeQualityDateParams(state);
  }

  function seedQualityYoke(): void {
    const urlState = qualityParamsToPanelDate(router.params);
    if (urlState) {
      qualityDateIntentEstablished = true;
      applyQualityPanelDate(urlState);
      yokedDates.updateFromPanel(urlState);
      return;
    }
    if (hasQualityDateParams(router.params)) return;

    const seed = yokedDates.seedForPanel();
    const retained = seed
      ? null
      : analyticsPageDates.restoreWithIntent("quality");
    const state = seed
      ? rangeToPanelDate(seed)
      : retained?.state ?? null;
    if (!state) return;
    if (retained) {
      qualityDateIntentEstablished = retained.explicitDateIntent;
    }
    applyQualityPanelDate(state);
    if (retained?.explicitDateIntent) {
      yokedDates.updateFromPanel(state);
    }
    if (seed || retained?.explicitDateIntent) {
      qualityDateIntentEstablished = true;
      writeQualityDateParams(state);
    }
  }

  function fetchQualitySignals() {
    analytics.fetchSignalsForQuality();
    const state = currentQualityPanelDate();
    if (state?.mode === "rolling" && qualityDateIntentEstablished) {
      if (yokedDates.range !== null) {
        yokedDates.updateFromPanel(state);
      }
      writeQualityDateParams(state);
    }
  }

  function handleProjectChange(value: string) {
    analytics.project = value;
    fetchQualitySignals();
  }

  function handleAgentChange(value: string) {
    analytics.agent = value;
    fetchQualitySignals();
  }

  function handleAutomatedScopeChange(value: string) {
    const scope = value as AutomatedScope;
    analytics.automatedScope = scope;
    analytics.includeAutomated = scope !== "human";
    fetchQualitySignals();
  }

  function handleRefresh() {
    fetchQualitySignals();
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
    const requestSignal = signalEvidenceRead.begin();
    selectedSignalId = signal;
    signalExamplesFilterKey = requestKey;
    signalExamplesLoading = true;
    signalExamplesError = null;
    try {
      const response = await callGenerated(
        () => AnalyticsService.getApiV1AnalyticsSignalSessions({
          ...params,
          signal,
          limit: 8,
        }),
        requestSignal,
      );
      if (
        signalEvidenceRead.isCurrent(requestSignal) &&
        selectedSignalId === signal &&
        signalExamplesFilterKey === requestKey &&
        signalExamplesRequest === request
      ) {
        signalExamples = response.sessions ?? [];
      }
    } catch (err) {
      if (
        isAbortError(err) ||
        !signalEvidenceRead.isCurrent(requestSignal)
      ) return;
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
        signalEvidenceRead.finish(requestSignal) &&
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
    // Route-first: App's deep-link effect owns selection and
    // hydration once the URL commits (#1190).
    router.navigateToSession(example.session_id, params);
    if (example.message_ordinal != null) {
      ui.scrollToOrdinal(example.message_ordinal, example.session_id);
    }
  }

  function openEvidenceListSession(event: MouseEvent) {
    if (
      !(event.target instanceof Element) ||
      !(event.currentTarget instanceof HTMLElement)
    ) return;
    const link = event.target.closest<HTMLAnchorElement>("a.evidence-row");
    if (!link) return;
    const links = Array.from(
      event.currentTarget.querySelectorAll<HTMLAnchorElement>(
        "a.evidence-row",
      ),
    );
    const example = signalExamples[links.indexOf(link)];
    if (!example) return;
    openEvidenceSession(example, event);
  }

  function delegateEvidenceClicks(node: HTMLElement) {
    const handleClick = (event: MouseEvent) => {
      openEvidenceListSession(event);
    };
    node.addEventListener("click", handleClick);
    return {
      destroy() {
        node.removeEventListener("click", handleClick);
      },
    };
  }

  function evidenceSessionParams(
    example: SignalSessionExample,
  ): Record<string, string> {
    return example.message_ordinal == null
      ? {}
      : { msg: String(example.message_ordinal) };
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

  onMount(() => {
    seedQualityYoke();
    sessions.loadProjects();
    sessions.loadAgents();
    fetchQualitySignals();
    unsubEvents = events.subscribeDebounced(() => {
      fetchQualitySignals();
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
      untrack(() => fetchQualitySignals());
    }
  });

  onDestroy(() => {
    analytics.cancelInFlightReads();
    signalEvidenceRead.cancel();
    const state = currentQualityPanelDate();
    if (state) {
      analyticsPageDates.retain(
        "quality",
        state,
        qualityDateIntentEstablished,
      );
    }
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

</script>

<div class="quality-page">
  <header class="toolbar">
    <RangePicker
      selection={rangeSelection}
      busy={querying}
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

    <RefreshControl
      lastUpdatedAt={analytics.qualityLastUpdatedAt}
      busy={querying}
      onRefresh={handleRefresh}
      label={m.quality_page_refresh()}
      title={m.quality_page_refresh()}
    />
  </header>

  <main class="content" class:querying aria-busy={querying}>
    {#if querying}
      <div class="query-progress" aria-hidden="true"></div>
    {/if}

    <section class="section-block" aria-labelledby="actions-title">
      <div class="section-heading compact">
        <div>
          <div class="eyebrow">
            <span class="badge rule">{m.insights_page_rule_based()}</span>
            <span>{m.insights_page_next_actions()}</span>
          </div>
          <h2 id="actions-title">{m.insights_page_deterministic_recommendations()}</h2>
        </div>
        <p class="quality-help">
          {m.quality_page_help_intro()}
          <a
            href="https://www.agentsview.io/quality/"
            target="_blank"
            rel="noopener noreferrer"
            class="quality-help-link"
          >
            {m.quality_page_help_docs()}
          </a>
        </p>
      </div>

      {#if recommendations.length === 0}
        <Card level="default" padding="none" class="state-panel compact-state">
          <strong>{m.insights_page_no_rule_actions()}</strong>
          <span>
            {m.insights_page_patterns_clear()}
          </span>
        </Card>
      {:else}
        <div class="recommendation-list">
          {#each recommendations as rec}
            <Card level="default" padding="none" class="recommendation">
              <article class="recommendation-content">
                <span class="badge rule">{m.insights_page_rule_based()}</span>
                <strong>{rec.label}</strong>
                <p>{rec.rationale}</p>
              </article>
            </Card>
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
            <Card level="default" padding="none" class="skeleton-pattern"></Card>
          {/each}
        </div>
      {:else if error && !signals}
        <Card level="default" padding="none" class="state-panel error">
          <div class="state-panel-alert" role="alert">
            <strong>{m.insights_page_could_not_load()}</strong>
            <span>{error}</span>
            <button onclick={fetchQualitySignals}>
              {m.insights_page_retry()}
            </button>
          </div>
        </Card>
      {:else if !hasData}
        <Card level="default" padding="none" class="state-panel">
          <strong>{m.insights_page_no_scored_data()}</strong>
          <span>
            {m.insights_page_no_scored_data_hint()}
          </span>
        </Card>
      {:else}
        {#if error}
          <Card level="default" padding="none" class="inline-warning">
            <div role="status">
              {m.insights_page_cached_warning({ error })}
            </div>
          </Card>
        {/if}

        <div class="summary-grid">
          <Card level="default" padding="none" class="summary-card">
            <article class="summary-card-content">
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
          </Card>
          <Card level="default" padding="none" class="summary-card">
            <article class="summary-card-content">
              <span class="label">{m.insights_page_scored_sessions()}</span>
              <strong>{summary.scoredSessions}</strong>
              <span>{m.insights_page_unscored_count({ count: summary.unscoredSessions })}</span>
            </article>
          </Card>
          <Card level="default" padding="none" class="summary-card">
            <article class="summary-card-content">
              <span class="label">{m.insights_page_low_quality()}</span>
              <strong>{summary.lowQualitySessions}</strong>
              <span>{m.insights_page_df_graded()}</span>
            </article>
          </Card>
          <Card level="default" padding="none" class="summary-card">
            <article class="summary-card-content">
              <span class="label">{m.insights_page_prompt_signals()}</span>
              <strong>{summary.computedQualitySessions}</strong>
              <span>{m.insights_page_sessions_computed()}</span>
            </article>
          </Card>
        </div>

        <Card
          level="default"
          padding="none"
          class="distribution-row"
          ariaLabel={m.insights_page_score_distribution()}
        >
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
        </Card>

        <div class="pattern-grid">
          {#each patterns as pattern}
            <Card
              level="default"
              padding="none"
              class={`pattern-card severity-${pattern.severity}`}
            >
              <article
                class="pattern-card-content"
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
            </Card>
          {/each}
        </div>

        {#if selectedSignalId}
          <Card level="default" padding="none" class="evidence-panel">
            <section class="evidence-panel-live" aria-live="polite">
              <div class="evidence-head">
              <div>
                <span class="examples-label">{m.insights_page_session_evidence()}</span>
                <h3>{selectedSignalLabel()}</h3>
              </div>
              <button
                class="text-btn"
                type="button"
                onclick={() => {
                  signalEvidenceRead.cancel();
                  selectedSignalId = null;
                  signalExamples = [];
                  signalExamplesLoading = false;
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
                <div class="evidence-list" use:delegateEvidenceClicks>
                  {#each signalExamples as example}
                    <Card
                      level="inset"
                      padding="none"
                      class="evidence-row"
                      href={router.buildSessionHref(
                      example.session_id,
                      evidenceSessionParams(example),
                    )}
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
                    </Card>
                  {/each}
                </div>
              {/if}
            </section>
          </Card>
        {/if}
      {/if}
    </section>

  </main>
</div>

<style>
  .quality-page {
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
  .toolbar-scope :global(.kit-typeahead) {
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

  .content {
    flex: 1;
    min-height: 0;
    overflow-y: auto;
    padding: 18px;
    display: flex;
    flex-direction: column;
    gap: var(--space-6);
    position: relative;
    transition: opacity 0.12s;
  }

  .content.querying {
    opacity: 0.88;
  }

  .query-progress {
    position: absolute;
    top: 0;
    left: 0;
    right: 0;
    z-index: 4;
    height: 2px;
    overflow: hidden;
    background: color-mix(
      in srgb,
      var(--accent-blue) 16%,
      transparent
    );
  }

  .query-progress::before {
    content: "";
    display: block;
    width: 38%;
    height: 100%;
    background: var(--accent-blue);
    border-radius: 999px;
    animation: query-progress 1s ease-in-out infinite;
  }

  .toolbar :global(.kit-refresh-control) {
    margin-left: auto;
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

  .section-heading p.quality-help {
    max-width: 58ch;
    margin: 0;
    display: flex;
    flex-wrap: wrap;
    justify-content: flex-end;
    gap: 4px;
    text-align: right;
    line-height: 1.35;
  }

  .quality-help-link {
    color: var(--accent-blue);
  }

  .quality-help-link:hover {
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


  .summary-grid {
    display: grid;
    grid-template-columns: repeat(4, minmax(0, 1fr));
    gap: var(--space-5);
  }

  .summary-grid :global(.summary-card) {
    min-height: 92px;
    padding: 12px;
    display: flex;
    flex-direction: column;
    gap: 0;
    justify-content: space-between;
  }

  .summary-grid :global(.summary-card > .kit-card__body) {
    display: contents;
  }

  .summary-card-content {
    display: flex;
    flex: 1;
    flex-direction: column;
    gap: 0;
    justify-content: space-between;
    min-width: 0;
  }

  .summary-grid :global(.summary-card .label) {
    color: var(--text-muted);
    font-size: 11px;
    font-weight: 600;
    text-transform: uppercase;
    letter-spacing: 0.04em;
  }

  .summary-grid :global(.summary-card strong) {
    font-size: 28px;
    line-height: 1;
    color: var(--text-primary);
    font-variant-numeric: tabular-nums;
  }

  .summary-grid :global(.summary-card span:last-child) {
    color: var(--text-secondary);
    font-size: 12px;
  }

  .content :global(.distribution-row) {
    display: grid;
    grid-template-columns: repeat(5, minmax(0, 1fr));
    gap: 8px;
    padding: 10px;
  }

  .content :global(.distribution-row > .kit-card__body) {
    display: contents;
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

  .pattern-grid :global(.pattern-card) {
    min-height: 310px;
    padding: 14px;
    display: flex;
    flex-direction: column;
    gap: 12px;
  }

  .pattern-grid :global(.pattern-card > .kit-card__body) {
    display: contents;
  }

  .pattern-card-content {
    display: flex;
    flex: 1;
    flex-direction: column;
    gap: 12px;
    min-width: 0;
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

  .pattern-grid :global(.severity-critical .severity) {
    color: var(--accent-red);
    background: color-mix(
      in srgb,
      var(--accent-red) 9%,
      transparent
    );
  }

  .pattern-grid :global(.severity-warning .severity),
  .pattern-grid :global(.severity-watch .severity) {
    color: var(--accent-amber);
    background: color-mix(
      in srgb,
      var(--accent-amber) 11%,
      transparent
    );
  }

  .pattern-grid :global(.severity-clear .severity) {
    color: var(--accent-green);
    background: color-mix(
      in srgb,
      var(--accent-green) 10%,
      transparent
    );
  }

  .pattern-grid :global(.severity-unavailable .severity) {
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

  .content :global(.evidence-panel) {
    display: grid;
    gap: var(--space-5);
    padding: 12px;
  }

  .content :global(.evidence-panel > .kit-card__body) {
    display: contents;
  }

  .evidence-panel-live {
    display: grid;
    gap: var(--space-5);
    min-width: 0;
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

  .evidence-list :global(.evidence-row) {
    display: grid;
    grid-template-columns: minmax(0, 1fr) auto;
    gap: 12px;
    align-items: center;
    padding: 9px 10px;
    text-decoration: none;
  }

  .evidence-list :global(.evidence-row > .kit-card__body) {
    display: contents;
  }

  .evidence-list :global(.evidence-row:hover) {
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

  .recommendation-list :global(.recommendation) {
    padding: 12px;
    display: grid;
    gap: var(--space-3);
  }

  .recommendation-list :global(.recommendation > .kit-card__body) {
    display: contents;
  }

  .recommendation-content {
    display: grid;
    gap: var(--space-3);
    min-width: 0;
  }

  .recommendation-list :global(.recommendation strong) {
    color: var(--text-primary);
    font-size: 13px;
  }

  .recommendation-list :global(.recommendation p) {
    color: var(--text-secondary);
    font-size: 12px;
    line-height: 1.45;
  }

  .text-btn {
    color: var(--text-muted);
    font-size: 12px;
  }

  .text-btn:hover {
    color: var(--text-primary);
  }

  .content :global(.state-panel) {
    padding: 18px;
    display: grid;
    gap: 6px;
    color: var(--text-secondary);
  }

  .content :global(.state-panel > .kit-card__body) {
    display: contents;
  }

  .state-panel-alert {
    display: grid;
    gap: 6px;
    min-width: 0;
  }

  .content :global(.state-panel strong) {
    color: var(--text-primary);
  }

  .content :global(.state-panel button) {
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

  .content :global(.state-panel.error) {
    border-color: color-mix(
      in srgb,
      var(--accent-red) 35%,
      var(--border-muted)
    );
  }

  .content :global(.compact-state) {
    padding: 14px;
  }

  .content :global(.inline-warning) {
    padding: 9px 10px;
    background: color-mix(
      in srgb,
      var(--accent-amber) 10%,
      var(--bg-surface)
    );
    border-color: color-mix(
      in srgb,
      var(--accent-amber) 24%,
      var(--border-muted)
    );
    color: var(--text-secondary);
    font-size: 12px;
  }

  .skeleton-card,
  .pattern-grid :global(.skeleton-pattern) {
    background: linear-gradient(
      90deg,
      var(--bg-surface) 0%,
      var(--bg-surface-hover) 50%,
      var(--bg-surface) 100%
    );
    background-size: 200% 100%;
    animation: shimmer 1.4s ease-in-out infinite;
  }

  .skeleton-card {
    border-radius: var(--radius-md);
    border: 1px solid var(--border-muted);
    height: 92px;
  }

  .pattern-grid :global(.skeleton-pattern) {
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

  @keyframes query-progress {
    0% {
      transform: translateX(-105%);
    }
    100% {
      transform: translateX(265%);
    }
  }

  @media (max-width: 900px) {
    .toolbar,
    .section-heading {
      align-items: stretch;
      flex-direction: column;
    }

    .toolbar :global(.kit-refresh-control) {
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

    .section-heading p.quality-help {
      justify-content: flex-start;
      text-align: left;
    }

    .summary-grid,
    .pattern-grid,
    .recommendation-list {
      grid-template-columns: 1fr;
    }

    .content :global(.distribution-row) {
      grid-template-columns: 1fr;
    }

    .driver-row,
    .evidence-list :global(.evidence-row) {
      grid-template-columns: 1fr;
    }

    .evidence-meta {
      justify-content: flex-start;
    }
  }
</style>
