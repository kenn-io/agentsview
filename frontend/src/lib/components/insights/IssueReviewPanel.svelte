<script lang="ts">
  import { onDestroy, onMount } from "svelte";
  import { Button, Card, Spinner, TextInput, Typeahead, type TypeaheadOption } from "@kenn-io/kit-ui";
  import { AnalyticsService, IssueReviewFindingStateInputBody } from "../../api/generated/index.js";
  import { callGenerated, isAbortError } from "../../api/runtime.js";
  import type {
    IssueFacet,
    IssueReviewEvidence,
    IssueReviewFinding,
    IssueReviewResponse,
  } from "../../api/types.js";
  import { getLocale, m } from "../../i18n/index.js";
  import { analytics } from "../../stores/analytics.svelte.js";
  import { events } from "../../stores/events.svelte.js";
  import { router } from "../../stores/router.svelte.js";
  import { ui } from "../../stores/ui.svelte.js";
  import { LatestRead } from "../../utils/latest-read.js";

  const REFRESH_INTERVAL_MS = 60 * 60 * 1000;
  const FILTER_STORAGE_KEY = "agentsview.issue-review.filters.v1";
  const SAVED_VIEWS_STORAGE_KEY = "agentsview.issue-review.saved-views.v1";
  const MAX_SAVED_VIEWS = 50;
  const MAX_VIEW_NAME_LENGTH = 80;
  const read = new LatestRead();
  let response = $state<IssueReviewResponse | null>(null);
  let loading = $state(false);
  let refreshing = $state(false);
  let error: string | null = $state(null);
  let mounted = false;
  let scopeKey = "";
  let timer: ReturnType<typeof setInterval> | undefined;
  let unsubscribe: (() => void) | undefined;

  let sessionId = $state("");
  let folder = $state("");
  let category = $state("");
  let tool = $state("");
  let source = $state("");
  let outcome = $state("");
  let severity = $state("");
  let confidence = $state("");
  let status = $state("");
  let reviewState = $state("");
  let recommendationType = $state("");
  let minOccurrences = $state("2");
  let minSessions = $state("2");
  let minProjects = $state("0");
  let minWastedMs = $state("0");
  let sort = $state("impact");
  let savedViews = $state<SavedView[]>([]);
  let selectedView = $state("");
  let viewName = $state("");
  let updatingFinding = $state("");
  let reviewError: string | null = $state(null);
  let suppressionSelections = $state<Record<string, string>>({});

  const findings: IssueReviewFinding[] = $derived(response ? response.findings : []);
  const facets: IssueReviewResponse["facets"] | undefined = $derived(response ? response.facets : undefined);
  const chatOptions = $derived(facetOptions(facets?.session, m.issue_review_all_chats()));
  const folderOptions = $derived(facetOptions(facets?.folder, m.issue_review_all_folders()));
  const categoryOptions = $derived(facetOptions(facets?.category, m.issue_review_all_categories(), reasonLabel));
  const toolOptions = $derived(facetOptions(facets?.tool, m.issue_review_all_tools()));
  const sourceOptions = $derived(facetOptions(facets?.source, m.issue_review_all_sources(), sourceLabel));
  const outcomeOptions = $derived(facetOptions(facets?.outcome, m.issue_review_all_outcomes()));
  const severityOptions = $derived(facetOptions(facets?.severity, m.issue_review_all_severities(), severityLabel));
  const confidenceOptions = $derived(facetOptions(facets?.confidence, m.issue_review_all_confidences(), confidenceLabel));
  const statusOptions = $derived(facetOptions(facets?.status, m.issue_review_all_statuses(), statusLabel));
  const reviewStateOptions = $derived(facetOptions(facets?.review_state, m.issue_review_all_review_states(), reviewStateLabel));
  const actionOptions = $derived(facetOptions(facets?.recommendation_type, m.issue_review_all_actions(), actionLabel));
  const savedViewOptions = $derived([
    { name: "", label: m.issue_review_no_saved_view() },
    ...savedViews.map((view) => ({ name: view.name, label: view.name })),
  ]);
  const occurrenceOptions: TypeaheadOption[] = [1, 2, 3, 5, 10].map((count) => ({
    name: String(count),
    label: m.issue_review_min_occurrences_value({ count }),
  }));
  const minSessionOptions: TypeaheadOption[] = [1, 2, 3, 5, 10].map((count) => ({
    name: String(count),
    label: m.issue_review_min_chats_value({ count }),
  }));
  const projectOptions: TypeaheadOption[] = [0, 2, 3, 5].map((count) => ({
    name: String(count),
    label: count === 0 ? m.issue_review_any_projects() : m.issue_review_min_projects_value({ count }),
  }));
  const wastedOptions: TypeaheadOption[] = [
    { name: "0", label: m.issue_review_any_wasted_time() },
    { name: "60000", label: m.issue_review_min_wasted_value({ duration: formatDuration(60_000) }) },
    { name: "300000", label: m.issue_review_min_wasted_value({ duration: formatDuration(300_000) }) },
    { name: "1800000", label: m.issue_review_min_wasted_value({ duration: formatDuration(1_800_000) }) },
  ];
  const sortOptions: TypeaheadOption[] = ["impact", "frequency", "recent", "waste", "duration"].map((value) => ({
    name: value,
    label: sortLabel(value),
  }));
  const suppressionOptions: TypeaheadOption[] = [
    { name: "1", label: m.issue_review_suppress_one_day() },
    { name: "7", label: m.issue_review_suppress_seven_days() },
    { name: "30", label: m.issue_review_suppress_thirty_days() },
    { name: "permanent", label: m.issue_review_suppress_permanently() },
  ];

  type FilterState = ReturnType<typeof filterState>;
  interface SavedView { name: string; filters: FilterState }

  function facetOptions(
    values: IssueFacet[] | undefined,
    allLabel: string,
    label: (value: string) => string = (value) => value,
  ): TypeaheadOption[] {
    return [
      { name: "", label: allLabel },
      ...(values ?? []).map((facet) => ({
        name: facet.value,
        label: `${facet.label || label(facet.value)} (${formatCount(facet.count)})`,
      })),
    ];
  }

  function globalScopeKey(): string {
    return JSON.stringify([
      analytics.from,
      analytics.to,
      analytics.project,
      analytics.machine,
      analytics.agent,
      analytics.termination,
      analytics.minUserMessages,
      analytics.includeOneShot,
      analytics.automatedScope,
      analytics.recentlyActive,
    ]);
  }

  export async function refresh(force = false, offset = 0): Promise<void> {
    const signal = read.begin();
    const firstLoad = response === null;
    if (firstLoad) loading = true;
    else refreshing = true;
    if (firstLoad) error = null;
    try {
      const data = await callGenerated(
        () => AnalyticsService.getApiV1AnalyticsIssueReview({
          ...analytics.signalEvidenceParams(),
          sessionId: sessionId || undefined,
          folder: folder || undefined,
          category: category || undefined,
          tool: tool || undefined,
          source: source || undefined,
          outcome: outcome || undefined,
          severity: (severity || undefined) as "high" | "medium" | "low" | undefined,
          confidence: (confidence || undefined) as "high" | "medium" | "low" | undefined,
          status: (status || undefined) as "open" | "recovered" | "recurring" | "observed" | undefined,
          reviewState: (reviewState || undefined) as "active" | "acknowledged" | "suppressed" | undefined,
          recommendationType: (recommendationType || undefined) as "skill" | "script" | "rule" | "tool_fix" | undefined,
          minOccurrences: Number(minOccurrences),
          minSessions: Number(minSessions),
          minProjects: Number(minProjects),
          minWastedMs: Number(minWastedMs),
          sort: sort as "impact" | "frequency" | "recent" | "waste" | "duration",
          refresh: force,
          offset,
          limit: 100,
        }),
        signal,
      );
      if (read.isCurrent(signal)) {
        const page = data as unknown as IssueReviewResponse;
        response = offset > 0 && response
          ? { ...page, findings: [...response.findings, ...page.findings] }
          : page;
        error = null;
      }
    } catch (cause) {
      if (isAbortError(cause) || !read.isCurrent(signal)) return;
      error = cause instanceof Error ? cause.message : m.issue_review_load_failed();
    } finally {
      if (read.finish(signal)) {
        loading = false;
        refreshing = false;
      }
    }
  }

  function selectFilter(setter: (value: string) => void, value: string) {
    setter(value);
    selectedView = "";
    persistFilters();
    void refresh();
  }

  function filterState() {
    return { sessionId, folder, category, tool, source, outcome, severity, confidence, status, reviewState, recommendationType, minOccurrences, minSessions, minProjects, minWastedMs, sort };
  }

  function persistFilters() {
    localStorage.setItem(FILTER_STORAGE_KEY, JSON.stringify(filterState()));
  }

  function parseFilterState(value: unknown): FilterState | null {
    if (!value || typeof value !== "object") return null;
    const saved = { ...(value as Record<string, unknown>) };
    if (saved.reviewState === undefined) saved.reviewState = "";
    return (Object.keys(filterState()) as Array<keyof FilterState>)
      .every((key) => typeof saved[key] === "string")
      ? saved as FilterState
      : null;
  }

  function applyFilterState(saved: FilterState) {
    for (const key of Object.keys(filterState()) as Array<keyof FilterState>) {
      if (key === "sessionId") sessionId = saved[key];
      else if (key === "folder") folder = saved[key];
      else if (key === "category") category = saved[key];
      else if (key === "tool") tool = saved[key];
      else if (key === "source") source = saved[key];
      else if (key === "outcome") outcome = saved[key];
      else if (key === "severity") severity = saved[key];
      else if (key === "confidence") confidence = saved[key];
      else if (key === "status") status = saved[key];
      else if (key === "reviewState") reviewState = saved[key];
      else if (key === "recommendationType") recommendationType = saved[key];
      else if (key === "minOccurrences") minOccurrences = saved[key];
      else if (key === "minSessions") minSessions = saved[key];
      else if (key === "minProjects") minProjects = saved[key];
      else if (key === "minWastedMs") minWastedMs = saved[key];
      else if (key === "sort") sort = saved[key];
    }
  }

  function restoreFilters() {
    try {
      const saved = JSON.parse(localStorage.getItem(FILTER_STORAGE_KEY) ?? "null");
      const parsed = parseFilterState(saved);
      if (parsed) applyFilterState(parsed);
      else if (saved !== null) localStorage.removeItem(FILTER_STORAGE_KEY);
    } catch {
      localStorage.removeItem(FILTER_STORAGE_KEY);
    }
  }

  function restoreSavedViews() {
    try {
      const saved = JSON.parse(localStorage.getItem(SAVED_VIEWS_STORAGE_KEY) ?? "[]");
      if (!Array.isArray(saved)) throw new Error("invalid saved views");
      const names = new Set<string>();
      savedViews = saved.flatMap((item): SavedView[] => {
        if (!item || typeof item !== "object" || typeof item.name !== "string") return [];
        const filters = parseFilterState(item.filters);
        if (!filters) return [];
        const name = item.name.trim().slice(0, MAX_VIEW_NAME_LENGTH);
        const key = name.toLowerCase();
        if (!name || names.has(key) || names.size >= MAX_SAVED_VIEWS) return [];
        names.add(key);
        return [{ name, filters }];
      });
      if (JSON.stringify(savedViews) !== JSON.stringify(saved)) persistSavedViews();
    } catch {
      savedViews = [];
      localStorage.removeItem(SAVED_VIEWS_STORAGE_KEY);
    }
  }

  function persistSavedViews() {
    localStorage.setItem(SAVED_VIEWS_STORAGE_KEY, JSON.stringify(savedViews));
  }

  function saveView() {
    const name = viewName.trim().slice(0, MAX_VIEW_NAME_LENGTH);
    if (!name) return;
    const key = name.toLowerCase();
    const next = { name, filters: filterState() };
    const index = savedViews.findIndex((view) => view.name.toLowerCase() === key);
    savedViews = index === -1
      ? [...savedViews.slice(-(MAX_SAVED_VIEWS - 1)), next]
      : savedViews.map((view, itemIndex) => itemIndex === index ? next : view);
    selectedView = name;
    viewName = name;
    persistSavedViews();
  }

  function selectSavedView(name: string) {
    selectedView = name;
    if (!name) {
      viewName = "";
      return;
    }
    const view = savedViews.find((item) => item.name === name);
    if (!view) return;
    viewName = view.name;
    applyFilterState(view.filters);
    persistFilters();
    void refresh();
  }

  function deleteSavedView() {
    if (!selectedView) return;
    savedViews = savedViews.filter((view) => view.name !== selectedView);
    selectedView = viewName = "";
    persistSavedViews();
  }

  function clearFilters() {
    sessionId = folder = category = tool = source = outcome = severity = confidence = status = reviewState = recommendationType = "";
    minOccurrences = minSessions = "2";
    minProjects = minWastedMs = "0";
    sort = "impact";
    selectedView = viewName = "";
    persistFilters();
    void refresh();
  }

  function openEvidence(evidence: IssueReviewEvidence, event: MouseEvent) {
    event.preventDefault();
    const params: Record<string, string> = {};
    if (evidence.message_ordinal != null) params.msg = String(evidence.message_ordinal);
    router.navigateToSession(evidence.session_id, params);
    if (evidence.message_ordinal != null) {
      ui.scrollToOrdinal(evidence.message_ordinal, evidence.session_id);
    }
  }

  function evidenceHref(evidence: IssueReviewEvidence): string {
    return router.buildSessionHref(
      evidence.session_id,
      evidence.message_ordinal == null
        ? undefined
        : { msg: String(evidence.message_ordinal) },
    );
  }

  function reasonLabel(reason: string): string {
    const labels: Record<string, () => string> = {
      missing_file: m.issue_review_reason_missing_file,
      missing_dependency: m.issue_review_reason_missing_dependency,
      permission_auth: m.issue_review_reason_permission_auth,
      rate_limit: m.issue_review_reason_rate_limit,
      network: m.issue_review_reason_network,
      timeout: m.issue_review_reason_timeout,
      windows_shell: m.issue_review_reason_windows_shell,
      line_endings: m.issue_review_reason_line_endings,
      git_github_ci: m.issue_review_reason_git_github_ci,
      github_issue_reference: m.issue_review_reason_github_issue_reference,
      build_test: m.issue_review_reason_build_test,
      failed_edit: m.issue_review_reason_failed_edit,
      tool_crash: m.issue_review_reason_tool_crash,
      generic_tool_failure: m.issue_review_reason_generic_tool_failure,
      command_failure: m.issue_review_reason_command_failure,
      retry_after_failure: m.issue_review_reason_retry_after_failure,
      repeated_polling: m.issue_review_reason_repeated_polling,
      repeated_read: m.issue_review_reason_repeated_read,
      shell_syntax: m.issue_review_reason_shell_syntax,
      slow_tool: m.issue_review_reason_slow_tool,
      repeated_workflow: m.issue_review_reason_repeated_workflow,
      repeated_question: m.issue_review_reason_repeated_question,
      user_correction: m.issue_review_reason_user_correction,
      reported_blocker: m.issue_review_reason_reported_blocker,
      response_retry: m.issue_review_reason_response_retry,
      tool_router_error: m.issue_review_reason_tool_router_error,
      hook_failure: m.issue_review_reason_hook_failure,
      app_session_error: m.issue_review_reason_app_session_error,
      shell_snapshot_failure: m.issue_review_reason_shell_snapshot_failure,
    };
    return labels[reason]?.() ?? reason;
  }

  function severityLabel(value: string): string {
    if (value === "high") return m.issue_review_severity_high();
    if (value === "medium") return m.issue_review_severity_medium();
    return m.issue_review_severity_low();
  }

  function confidenceLabel(value: string): string {
    if (value === "high") return m.issue_review_confidence_high();
    if (value === "medium") return m.issue_review_confidence_medium();
    return m.issue_review_confidence_low();
  }

  function statusLabel(value: string): string {
    if (value === "open") return m.issue_review_status_open();
    if (value === "recovered") return m.issue_review_status_recovered();
    if (value === "recurring") return m.issue_review_status_recurring();
    return m.issue_review_status_observed();
  }

  function reviewStateLabel(value: string): string {
    if (value === "acknowledged") return m.issue_review_state_acknowledged();
    if (value === "suppressed") return m.issue_review_state_suppressed();
    return m.issue_review_state_active();
  }

  function findingReviewState(finding: IssueReviewFinding): string {
    return finding.review_state || "active";
  }

  function suppressionSelection(findingID: string): string {
    return suppressionSelections[findingID] ?? "7";
  }

  function selectSuppression(findingID: string, value: string) {
    suppressionSelections = { ...suppressionSelections, [findingID]: value };
  }

  async function setReviewState(
    finding: IssueReviewFinding,
    next: "acknowledged" | "suppressed",
  ) {
    updatingFinding = finding.id;
    reviewError = null;
    const selection = suppressionSelection(finding.id);
    try {
      await callGenerated(() => AnalyticsService.putApiV1AnalyticsIssueReviewFindingsIdState({
        id: finding.id,
        requestBody: {
          review_state: next === "acknowledged"
            ? IssueReviewFindingStateInputBody.review_state.ACKNOWLEDGED
            : IssueReviewFindingStateInputBody.review_state.SUPPRESSED,
          finding_last_seen: finding.last_seen,
          suppression_days: next === "suppressed" && selection !== "permanent"
            ? Number(selection)
            : undefined,
        },
      }));
      await refresh();
    } catch (cause) {
      reviewError = cause instanceof Error ? cause.message : m.issue_review_load_failed();
    } finally {
      updatingFinding = "";
    }
  }

  async function reopenFinding(finding: IssueReviewFinding) {
    updatingFinding = finding.id;
    reviewError = null;
    try {
      await callGenerated(() => AnalyticsService.deleteApiV1AnalyticsIssueReviewFindingsIdState({ id: finding.id }));
      await refresh();
    } catch (cause) {
      reviewError = cause instanceof Error ? cause.message : m.issue_review_load_failed();
    } finally {
      updatingFinding = "";
    }
  }

  function actionLabel(value: string): string {
    if (value === "skill") return m.issue_review_action_skill();
    if (value === "script") return m.issue_review_action_script();
    if (value === "rule") return m.issue_review_action_rule();
    return m.issue_review_action_tool_fix();
  }

  function sourceLabel(value: string): string {
    const labels: Record<string, () => string> = {
      tool_result: m.issue_review_source_tool_result,
      tool_execution: m.issue_review_source_tool_execution,
      tool_call: m.issue_review_source_tool_call,
      user_message: m.issue_review_source_user_message,
      assistant_commentary: m.issue_review_source_assistant_commentary,
      codex_log: m.issue_review_source_codex_log,
      event_msg: m.issue_review_source_event,
      message: m.issue_review_source_message,
    };
    return labels[value]?.() ?? value.replaceAll("_", " ");
  }

  function sortLabel(value: string): string {
    if (value === "frequency") return m.issue_review_sort_frequency();
    if (value === "recent") return m.issue_review_sort_recent();
    if (value === "waste") return m.issue_review_sort_waste();
    if (value === "duration") return m.issue_review_sort_duration();
    return m.issue_review_sort_impact();
  }

  function githubHref(reference: string): string {
    const match = /^([^/]+)\/([^#]+)#(\d+)$/.exec(reference);
    return match ? `https://github.com/${match[1]}/${match[2]}/issues/${match[3]}` : "https://github.com/issues";
  }

  function formatDuration(ms: number): string {
    const format = new Intl.NumberFormat(getLocale(), { minimumFractionDigits: 1, maximumFractionDigits: 1 });
    if (ms >= 60_000) return m.issue_review_duration_minutes({ value: format.format(ms / 60_000) });
    return m.issue_review_duration_seconds({ value: format.format(ms / 1000) });
  }

  function formatCount(value: number): string {
    return new Intl.NumberFormat(getLocale()).format(value);
  }

  onMount(() => {
    mounted = true;
    restoreFilters();
    restoreSavedViews();
    scopeKey = globalScopeKey();
    void refresh();
    timer = setInterval(() => void refresh(), REFRESH_INTERVAL_MS);
    unsubscribe = events.subscribeDebounced(() => void refresh());
  });

  $effect(() => {
    const next = globalScopeKey();
    if (!mounted || next === scopeKey) return;
    scopeKey = next;
    void refresh();
  });

  onDestroy(() => {
    read.cancel();
    if (timer) clearInterval(timer);
    unsubscribe?.();
  });
</script>

<section class="issue-review" aria-labelledby="issue-review-title">
  <div class="heading">
    <div>
      <span class="eyebrow">{m.issue_review_proactive()}</span>
      <h2 id="issue-review-title">{m.issue_review_title()}</h2>
      <p>{m.issue_review_description()}</p>
    </div>
    <div class="heading-actions">
      {#if refreshing}<Spinner size={14} label={m.issue_review_refreshing()} />{/if}
      <Button size="sm" onclick={() => refresh(true)} disabled={refreshing}>{m.issue_review_refresh_now()}</Button>
      <Button size="sm" onclick={clearFilters}>{m.issue_review_clear_filters()}</Button>
    </div>
  </div>

  <div class="saved-views">
    <Typeahead options={savedViewOptions} value={selectedView} fallbackLabel={m.issue_review_no_saved_view()} placeholder={m.issue_review_saved_view()} title={m.issue_review_saved_view()} emptyLabel={m.issue_review_no_saved_views()} onselect={selectSavedView} />
    <div class="view-name">
      <TextInput size="sm" block bind:value={viewName} placeholder={m.issue_review_view_name_placeholder()} ariaLabel={m.issue_review_view_name()} />
    </div>
    <Button size="sm" onclick={saveView} disabled={!viewName.trim()}>{m.issue_review_save_view()}</Button>
    <Button size="sm" onclick={deleteSavedView} disabled={!selectedView}>{m.issue_review_delete_view()}</Button>
  </div>

  <div class="filters" aria-label={m.issue_review_filters()}>
    <Typeahead options={chatOptions} value={sessionId} fallbackLabel={m.issue_review_all_chats()} placeholder={m.issue_review_chat()} title={m.issue_review_chat()} emptyLabel={m.issue_review_no_matches()} onselect={(value) => selectFilter((next) => { sessionId = next; if (next) minSessions = "1"; }, value)} />
    <Typeahead options={folderOptions} value={folder} fallbackLabel={m.issue_review_all_folders()} placeholder={m.issue_review_folder()} title={m.issue_review_folder()} emptyLabel={m.issue_review_no_matches()} onselect={(value) => selectFilter((next) => folder = next, value)} />
    <Typeahead options={categoryOptions} value={category} fallbackLabel={m.issue_review_all_categories()} placeholder={m.issue_review_category()} title={m.issue_review_category()} emptyLabel={m.issue_review_no_matches()} onselect={(value) => selectFilter((next) => category = next, value)} />
    <Typeahead options={toolOptions} value={tool} fallbackLabel={m.issue_review_all_tools()} placeholder={m.issue_review_tool()} title={m.issue_review_tool()} emptyLabel={m.issue_review_no_matches()} onselect={(value) => selectFilter((next) => tool = next, value)} />
    <Typeahead options={sourceOptions} value={source} fallbackLabel={m.issue_review_all_sources()} placeholder={m.issue_review_source()} title={m.issue_review_source()} emptyLabel={m.issue_review_no_matches()} onselect={(value) => selectFilter((next) => source = next, value)} />
    <Typeahead options={outcomeOptions} value={outcome} fallbackLabel={m.issue_review_all_outcomes()} placeholder={m.issue_review_outcome()} title={m.issue_review_outcome()} emptyLabel={m.issue_review_no_matches()} onselect={(value) => selectFilter((next) => outcome = next, value)} />
    <Typeahead options={severityOptions} value={severity} fallbackLabel={m.issue_review_all_severities()} placeholder={m.issue_review_severity()} title={m.issue_review_severity()} emptyLabel={m.issue_review_no_matches()} onselect={(value) => selectFilter((next) => severity = next, value)} />
    <Typeahead options={confidenceOptions} value={confidence} fallbackLabel={m.issue_review_all_confidences()} placeholder={m.issue_review_confidence()} title={m.issue_review_confidence()} emptyLabel={m.issue_review_no_matches()} onselect={(value) => selectFilter((next) => confidence = next, value)} />
    <Typeahead options={statusOptions} value={status} fallbackLabel={m.issue_review_all_statuses()} placeholder={m.issue_review_status()} title={m.issue_review_status()} emptyLabel={m.issue_review_no_matches()} onselect={(value) => selectFilter((next) => status = next, value)} />
    <Typeahead options={reviewStateOptions} value={reviewState} fallbackLabel={m.issue_review_all_review_states()} placeholder={m.issue_review_review_state()} title={m.issue_review_review_state()} emptyLabel={m.issue_review_no_matches()} onselect={(value) => selectFilter((next) => reviewState = next, value)} />
    <Typeahead options={actionOptions} value={recommendationType} fallbackLabel={m.issue_review_all_actions()} placeholder={m.issue_review_action()} title={m.issue_review_action()} emptyLabel={m.issue_review_no_matches()} onselect={(value) => selectFilter((next) => recommendationType = next, value)} />
    <Typeahead options={occurrenceOptions} value={minOccurrences} fallbackLabel={m.issue_review_min_occurrences_value({ count: Number(minOccurrences) })} placeholder={m.issue_review_min_occurrences()} title={m.issue_review_min_occurrences()} emptyLabel={m.issue_review_no_matches()} onselect={(value) => selectFilter((next) => minOccurrences = next, value)} />
    <Typeahead options={minSessionOptions} value={minSessions} fallbackLabel={m.issue_review_min_chats_value({ count: Number(minSessions) })} placeholder={m.issue_review_min_chats()} title={m.issue_review_min_chats()} emptyLabel={m.issue_review_no_matches()} onselect={(value) => selectFilter((next) => minSessions = next, value)} />
    <Typeahead options={projectOptions} value={minProjects} fallbackLabel={Number(minProjects) === 0 ? m.issue_review_any_projects() : m.issue_review_min_projects_value({ count: Number(minProjects) })} placeholder={m.issue_review_min_projects()} title={m.issue_review_min_projects()} emptyLabel={m.issue_review_no_matches()} onselect={(value) => selectFilter((next) => minProjects = next, value)} />
    <Typeahead options={wastedOptions} value={minWastedMs} fallbackLabel={Number(minWastedMs) === 0 ? m.issue_review_any_wasted_time() : m.issue_review_min_wasted_value({ duration: formatDuration(Number(minWastedMs)) })} placeholder={m.issue_review_min_wasted()} title={m.issue_review_min_wasted()} emptyLabel={m.issue_review_no_matches()} onselect={(value) => selectFilter((next) => minWastedMs = next, value)} />
    <Typeahead options={sortOptions} value={sort} fallbackLabel={sortLabel(sort)} placeholder={m.issue_review_sort()} title={m.issue_review_sort()} emptyLabel={m.issue_review_no_matches()} onselect={(value) => selectFilter((next) => sort = next, value)} />
  </div>

  {#if loading}
    <Card level="default" padding="none"><div class="state"><Spinner label={m.issue_review_loading()} /></div></Card>
  {:else if error && response === null}
    <Card level="default" padding="none">
      <div class="state error">
        <strong>{m.issue_review_load_failed()}</strong><span>{error}</span>
        <Button size="sm" onclick={() => refresh()}>{m.issue_review_retry()}</Button>
      </div>
    </Card>
  {:else}
    {#if error}<div class="cached-warning" role="status">{m.issue_review_cached_warning({ error })}</div>{/if}
    {#if reviewError}<div class="cached-warning error" role="alert">{m.issue_review_update_failed({ error: reviewError })}</div>{/if}
    <div class="scan-summary">
      <span>{m.issue_review_scanned_sessions({ count: response?.scanned_sessions ?? 0 })}</span>
      <span>{m.issue_review_scanned_messages({ count: response?.scanned_messages ?? 0 })}</span>
      <span>{m.issue_review_scanned_calls({ count: response?.scanned_tool_calls ?? 0 })}</span>
      {#if response?.duplicate_tool_calls || response?.duplicate_messages}<span>{m.issue_review_duplicates_excluded({ count: (response?.duplicate_tool_calls ?? 0) + (response?.duplicate_messages ?? 0) })}</span>{/if}
      {#if response?.scanned_telemetry}<span>{m.issue_review_scanned_logs({ count: response.scanned_telemetry })}</span>{/if}
      <span>{m.issue_review_showing_findings({ shown: findings.length, total: response?.total_findings ?? findings.length })}</span>
    </div>
    {#if response?.telemetry_status && response.telemetry_status !== "available"}
      <div class="cached-warning" role="status">{m.issue_review_telemetry_unavailable()}</div>
    {/if}
    {#if findings.length === 0}
      <Card level="default" padding="none"><div class="state"><strong>{m.issue_review_empty()}</strong><span>{m.issue_review_empty_hint()}</span></div></Card>
    {:else}
      <div class="finding-grid">
        {#each findings as finding (finding.id)}
          <Card level="default" padding="none">
            <div class={`finding severity-${finding.severity}`}>
              <article>
              <header>
                <div><h3>{reasonLabel(finding.reason_code)}</h3><code>{finding.tool || finding.signature}</code></div>
                <div class="chips"><span>{severityLabel(finding.severity)}</span><span>{confidenceLabel(finding.confidence)}</span><span>{statusLabel(finding.status)}</span>{#if findingReviewState(finding) !== "active"}<span>{reviewStateLabel(findingReviewState(finding))}</span>{/if}<span>{actionLabel(finding.recommendation_type)}</span></div>
              </header>
              <p class="signature">{finding.signature}</p>
              <div class="counts">
                <strong>{m.issue_review_occurrences({ count: finding.occurrences })}</strong>
                <span>{m.issue_review_chats({ count: finding.session_count })}</span>
                <span>{m.issue_review_projects({ count: finding.project_count })}</span>
              </div>
              {#if finding.p95_duration_ms != null || finding.wasted_duration_ms > 0}
                <div class="timing">
                  {#if finding.p95_duration_ms != null}<span>{m.issue_review_p95({ duration: formatDuration(finding.p95_duration_ms) })}</span>{/if}
                  <span>{m.issue_review_coverage({ value: Math.round(finding.duration_coverage * 100) })}</span>
                  {#if finding.wasted_duration_ms > 0}<span>{m.issue_review_wasted_proxy({ duration: formatDuration(finding.wasted_duration_ms) })}</span>{/if}
                </div>
              {/if}
              <p class="suggestion"><strong>{m.issue_review_suggestion_label()}</strong> {finding.recommendation}</p>
              <div class="review-actions">
                <Button size="sm" disabled={updatingFinding !== "" || findingReviewState(finding) === "acknowledged"} onclick={() => setReviewState(finding, "acknowledged")}>{m.issue_review_acknowledge()}</Button>
                <Typeahead options={suppressionOptions} value={suppressionSelection(finding.id)} fallbackLabel={m.issue_review_suppress_seven_days()} placeholder={m.issue_review_suppress_for()} title={m.issue_review_suppress_for()} emptyLabel={m.issue_review_no_matches()} onselect={(value) => selectSuppression(finding.id, value)} />
                <Button size="sm" disabled={updatingFinding !== ""} onclick={() => setReviewState(finding, "suppressed")}>{m.issue_review_suppress()}</Button>
                {#if findingReviewState(finding) !== "active"}<Button size="sm" disabled={updatingFinding !== ""} onclick={() => reopenFinding(finding)}>{m.issue_review_reopen()}</Button>{/if}
                {#if updatingFinding === finding.id}<Spinner size={14} label={m.issue_review_updating()} />{/if}
              </div>
              {#if finding.github_reference}<a class="github-link" href={githubHref(finding.github_reference)} target="_blank" rel="noreferrer">{m.issue_review_open_github_issue({ reference: finding.github_reference })}</a>{/if}
              <div class="evidence">
                {#each finding.evidence as item}
                  <a href={evidenceHref(item)} onclick={(event) => openEvidence(item, event)}>
                    <strong>{item.project || m.issue_review_global_project()}</strong>
                    <span>{item.excerpt}</span>
                    <small>{item.date}{item.message_ordinal == null ? "" : ` · #${item.message_ordinal}`}</small>
                    <small class="evidence-meta">{sourceLabel(item.source)}{item.tool ? ` · ${item.tool}` : ""}{item.outcome ? ` · ${item.outcome}` : ""}{item.cwd ? ` · ${item.cwd}` : ""}</small>
                  </a>
                {/each}
              </div>
              </article>
            </div>
          </Card>
        {/each}
      </div>
      {#if response && findings.length < response.total_findings}
        <div class="load-more">
          <Button size="sm" disabled={refreshing} onclick={() => refresh(false, findings.length)}>
            {m.issue_review_load_more()}
          </Button>
        </div>
      {/if}
    {/if}
  {/if}
</section>

<style>
  .issue-review { display: grid; gap: 14px; }
  .heading { display: flex; justify-content: space-between; align-items: start; gap: 16px; }
  .heading-actions { display: flex; align-items: center; gap: 6px; }
  .heading h2 { margin: 3px 0 4px; font-size: 18px; }
  .heading p { margin: 0; color: var(--text-muted); font-size: 13px; }
  .eyebrow { color: var(--accent-blue); font-size: 11px; font-weight: 700; letter-spacing: .08em; text-transform: uppercase; }
  .saved-views { display: flex; flex-wrap: wrap; align-items: center; gap: 6px; }
  .view-name { flex: 1 1 180px; max-width: 300px; }
  .filters { display: grid; grid-template-columns: repeat(auto-fit, minmax(150px, 1fr)); gap: 8px; }
  .state { min-height: 110px; display: grid; place-content: center; gap: 6px; padding: 20px; text-align: center; }
  .state span, .cached-warning, .scan-summary { color: var(--text-muted); font-size: 12px; }
  .cached-warning { padding: 8px 10px; border: 1px solid var(--border-default); border-radius: 8px; }
  .scan-summary { display: flex; flex-wrap: wrap; gap: 12px; }
  .finding-grid { display: grid; grid-template-columns: repeat(auto-fit, minmax(340px, 1fr)); gap: 12px; }
  .load-more { display: flex; justify-content: center; }
  .finding { border-left: 3px solid var(--border-default); }
  .finding.severity-high { border-left-color: var(--accent-red); }
  .finding.severity-medium { border-left-color: var(--accent-amber); }
  .finding article { display: grid; gap: 10px; padding: 14px; }
  .finding header { display: flex; justify-content: space-between; gap: 10px; }
  .finding h3 { margin: 0 0 4px; font-size: 14px; }
  .finding code { color: var(--text-muted); font-size: 11px; }
  .chips { display: flex; flex-wrap: wrap; justify-content: end; gap: 4px; }
  .chips span { padding: 2px 6px; border: 1px solid var(--border-default); border-radius: 999px; color: var(--text-muted); font-size: 10px; }
  .signature, .suggestion { margin: 0; font-size: 12px; line-height: 1.45; }
  .counts, .timing { display: flex; flex-wrap: wrap; gap: 10px; font-size: 11px; color: var(--text-muted); }
  .counts strong { color: var(--text-primary); }
  .suggestion { padding: 8px; border-radius: 6px; background: var(--bg-inset); }
  .review-actions { display: flex; flex-wrap: wrap; align-items: center; gap: 6px; }
  .review-actions :global(.kit-typeahead) { min-width: 150px; }
  .github-link { width: fit-content; color: var(--accent-blue); font-size: 11px; text-decoration: none; }
  .github-link:hover { text-decoration: underline; }
  .evidence { display: grid; gap: 5px; }
  .evidence a { display: grid; grid-template-columns: minmax(70px, auto) 1fr auto; gap: 8px; align-items: baseline; color: inherit; text-decoration: none; padding: 7px 8px; border: 1px solid var(--border-default); border-radius: 6px; }
  .evidence a:hover { border-color: var(--accent-blue); }
  .evidence strong, .evidence span, .evidence small { overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
  .evidence strong { font-size: 11px; }
  .evidence span { font-size: 11px; color: var(--text-muted); }
  .evidence small { font-size: 10px; color: var(--text-muted); }
  .evidence .evidence-meta { grid-column: 1 / -1; }
  @media (max-width: 720px) { .finding-grid { grid-template-columns: 1fr; } .finding header { display: grid; } .chips { justify-content: start; } .evidence a { grid-template-columns: 1fr; } }
</style>
