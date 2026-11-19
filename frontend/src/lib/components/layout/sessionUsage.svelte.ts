import type { Session } from "../../api/types.js";
import {
  SessionsService,
  type DbCaptureCoverageView,
  type DbPlanUsageView,
} from "../../api/generated/index";
import { isAbortError } from "../../api/runtime.js";
import { m } from "../../i18n/index.js";
import type { Money } from "../../money.js";
import { sessions } from "../../stores/sessions.svelte.js";
import { formatCost, formatCount, formatTokenUsage } from "../../utils/format.js";
import { LatestRead } from "../../utils/latest-read.js";
import { normalizeMessagePreview } from "../../utils/messages.js";
import { describeCaptureCoverage, describePlanUsage } from "../../utils/planUsage.js";
import { sessionDisplayId } from "../../utils/sessionDisplayId.js";

export interface SessionUsageBreakdownEntry {
  ordinal: number;
  message_ordinal?: number;
  source: string;
  label: string;
  timestamp: string;
  model: string;
  input_tokens: number;
  output_tokens: number;
  cache_creation_input_tokens: number;
  cache_read_input_tokens: number;
  cost: Money;
  has_cost: boolean;
}

function childUsageFetchKey(parentId: string): string {
  return Array.from(sessions.childSessions.values())
    .filter((child) => child.parent_session_id === parentId)
    .map((child) =>
      [
        child.id,
        child.relationship_type ?? "",
        child.transcript_revision ?? "",
        child.message_count ?? 0,
        child.total_output_tokens ?? 0,
        child.peak_context_tokens ?? 0,
        child.ended_at ?? "",
      ].join("\t"),
    )
    .sort()
    .join("\n");
}

function usageFetchKey(s: Session): string {
  return [
    s.id,
    s.transcript_revision ?? "",
    s.total_output_tokens ?? 0,
    s.peak_context_tokens ?? 0,
    s.has_total_output_tokens ?? "",
    s.has_peak_context_tokens ?? "",
    s.message_count ?? 0,
    s.ended_at ?? "",
    sessions.activeSessionUsageVersion,
    childUsageFetchKey(s.id),
  ].join("\n");
}

/**
 * The usage figures the session header shows: token totals, cost, the
 * per-step breakdown, and plan usage. Create it while a component
 * initializes; its reads follow the session `getSession` returns and stop
 * when that component is destroyed.
 */
export class SessionUsage {
  cost = $state<Money | null>(null);
  costIsRollup = $state(false);
  rollupSubagentCount = $state(0);
  breakdownCount = $state(0);
  breakdown = $state<SessionUsageBreakdownEntry[]>([]);
  // Plan usage and live capture coverage from the same usage read as the
  // cost; null until it answers or when the session has none.
  planUsage = $state<DbPlanUsageView | null>(null);
  captureCoverage = $state<DbCaptureCoverageView | null>(null);
  loaded = $state(false);
  // Title of the thread this one is counted under, once read.
  rootTitle = $state<string | null>(null);
  // Menu rows render only while open, so the collapsed dropdown
  // stays DOM-free.
  breakdownOpen = $state(false);
  breakdownLoading = $state(false);
  planUsageOpen = $state(false);

  private readonly costRead = new LatestRead();
  private readonly breakdownRead = new LatestRead();
  private readonly rootRead = new LatestRead();
  // Key of the last successful usage fetch. Cost depends on more
  // than output tokens (input/cache tokens and explicit usage-event
  // costs), so the key includes every cost-affecting field present
  // in API session responses. A resync that changes none of these
  // (e.g. a cost-only usage event) keeps a stale cost until the
  // next keyed field moves; closing that would need a freshness
  // marker in the session API.
  private costFetchKey: string | null = null;
  private costSessionId: string | null = null;
  private breakdownFetchKey: string | null = null;
  private readonly getSession: () => Session | undefined;

  constructor(getSession: () => Session | undefined) {
    this.getSession = getSession;
    $effect(() => this.readUsage());
    // Breakdown rows are fetched only when the menu opens; large
    // sessions can have thousands of entries and the count-only
    // /usage fetch happens automatically on every session.
    $effect(() => this.readBreakdown());
    $effect(() => this.readRootTitle());
    $effect(() => () => {
      this.rootRead.cancel();
      this.costRead.cancel();
      this.breakdownRead.cancel();
    });
  }

  // The usage root's title names the link; the link itself needs only the ID.
  readonly rootId = $derived(
    this.planUsage?.outcome_kind === "counted_under"
      ? (this.planUsage.counted_under ?? null)
      : null,
  );

  // Waits for the usage read, so a slow read never shows "not checked yet"
  // for a thread that has plan usage.
  readonly planUsageDisplay = $derived.by(() => {
    const session = this.getSession();
    return session && this.loaded ? describePlanUsage(session.agent, this.planUsage) : null;
  });

  readonly captureCoverageLines = $derived(
    describeCaptureCoverage(this.captureCoverage, this.planUsage),
  );

  // The missed-turn line comes last in the coverage lines; the closed
  // trigger repeats it so its warning icon is explained without opening.
  private readonly missedTurnsLine = $derived(
    (this.captureCoverage?.uncaptured_turns ?? 0) > 0
      ? (this.captureCoverageLines.at(-1) ?? null)
      : null,
  );

  // Shared by the closed trigger and the opened menu so both name the
  // same root session.
  readonly countedUnderLabel = $derived.by(() => {
    const rootId = this.planUsageDisplay?.countedUnder;
    return rootId
      ? m.plan_usage_counted_under({
          title: this.rootTitle ?? sessionDisplayId(rootId),
        })
      : null;
  });

  // Warnings the closed trigger explains with its icon: figures a later
  // check made stale, and turns live capture missed.
  readonly planUsageWarnings = $derived(
    [this.planUsageDisplay?.staleNote, this.missedTurnsLine].filter(
      (line): line is string => !!line,
    ),
  );

  readonly planUsageTitle = $derived.by(() => {
    if (!this.planUsageDisplay) return undefined;
    const state = this.countedUnderLabel ?? this.planUsageDisplay.headline;
    return [state, ...this.planUsageWarnings].filter(Boolean).join("\n") || undefined;
  });

  // Screen readers hear the closed trigger's state and warnings as part of
  // its name; the title alone may not be read.
  readonly planUsageSrText = $derived(
    [this.countedUnderLabel ?? this.planUsageDisplay?.headline, ...this.planUsageWarnings]
      .filter(Boolean)
      .join(", "),
  );

  readonly costLabel = $derived(this.cost !== null ? formatCost(this.cost) : null);

  readonly costTitle = $derived(
    this.costIsRollup
      ? m.session_breadcrumb_total_cost_title({
          count: this.rollupSubagentCount,
          countLabel: formatCount(this.rollupSubagentCount),
        })
      : m.session_breadcrumb_estimated_session_cost(),
  );

  readonly tokenSummary = $derived.by(() => {
    const session = this.getSession();
    if (!session) return null;
    return formatTokenUsage(
      session.peak_context_tokens ?? 0,
      session.has_peak_context_tokens ?? session.peak_context_tokens > 0,
      session.total_output_tokens ?? 0,
      session.has_total_output_tokens ?? session.total_output_tokens > 0,
    );
  });

  // The stats group renders only when it has a figure, so no empty group
  // leaves a stray separator.
  readonly hasStats = $derived(
    this.breakdownCount > 0 || !!this.tokenSummary || !!this.costLabel || !!this.planUsageDisplay,
  );

  private resetPlanUsage() {
    this.planUsage = null;
    this.captureCoverage = null;
    this.loaded = false;
    this.planUsageOpen = false;
  }

  private resetBreakdown() {
    this.breakdownRead.cancel();
    this.breakdownCount = 0;
    this.breakdown = [];
    this.breakdownOpen = false;
    this.breakdownLoading = false;
    this.breakdownFetchKey = null;
  }

  private readUsage() {
    const session = this.getSession();
    if (!session) {
      this.costRead.cancel();
      this.cost = null;
      this.costIsRollup = false;
      this.rollupSubagentCount = 0;
      this.resetPlanUsage();
      this.resetBreakdown();
      this.costFetchKey = null;
      this.costSessionId = null;
      return;
    }
    const id = session.id;
    const key = usageFetchKey(session);
    if (id !== this.costSessionId) {
      // Entering a different session invalidates both the displayed
      // cost and the fetch cache; the cached key must never satisfy
      // the early return below while another session's request is
      // still in flight.
      this.cost = null;
      this.costIsRollup = false;
      this.rollupSubagentCount = 0;
      this.resetPlanUsage();
      this.resetBreakdown();
      this.costFetchKey = null;
    }
    if (key === this.costFetchKey) return;
    const signal = this.costRead.begin();
    this.costSessionId = id;
    SessionsService.getApiV1SessionsByIdUsage({ id }, { rollup: true }, { signal })
      .then((res) => {
        if (!this.costRead.isCurrent(signal)) return;
        this.costFetchKey = key;
        this.rollupSubagentCount = res.rollup_subagent_count ?? 0;
        this.costIsRollup = this.rollupSubagentCount > 0 && res.has_rollup_cost === true;
        this.cost = this.costIsRollup ? (res.rollup_cost ?? null) : res.has_cost ? res.cost : null;
        this.breakdownCount = res.breakdown_count ?? 0;
        this.planUsage = res.plan_usage ?? null;
        this.captureCoverage = res.capture_coverage ?? null;
        this.loaded = true;
      })
      .catch((e) => {
        if (isAbortError(e) || !this.costRead.isCurrent(signal)) return;
        this.breakdownCount = 0;
        // Leave the fetch key unset so the next
        // session refresh retries the lookup.
      })
      .finally(() => this.costRead.finish(signal));
  }

  private readBreakdown() {
    const session = this.getSession();
    if (!this.breakdownOpen || !session) {
      this.breakdownRead.cancel();
      this.breakdownLoading = false;
      return;
    }
    const id = session.id;
    const key = usageFetchKey(session);
    if (key === this.breakdownFetchKey) return;
    const signal = this.breakdownRead.begin();
    this.breakdownLoading = true;
    SessionsService.getApiV1SessionsByIdUsage({ id }, { breakdown: true }, { signal })
      .then((res) => {
        if (!this.breakdownRead.isCurrent(signal)) return;
        this.breakdownFetchKey = key;
        this.breakdownLoading = false;
        this.breakdown = Array.isArray(res.breakdown) ? res.breakdown : [];
      })
      .catch((e) => {
        if (isAbortError(e) || !this.breakdownRead.isCurrent(signal)) return;
        this.breakdownLoading = false;
        this.breakdown = [];
        // Leave the fetch key unset so reopening retries.
      })
      .finally(() => this.breakdownRead.finish(signal));
  }

  private readRootTitle() {
    const id = this.rootId;
    this.rootTitle = null;
    if (!id) {
      this.rootRead.cancel();
      return;
    }
    const signal = this.rootRead.begin();
    SessionsService.getApiV1SessionsById({ id }, { signal })
      .then((root) => {
        if (!this.rootRead.isCurrent(signal)) return;
        this.rootTitle = root.display_name || normalizeMessagePreview(root.first_message) || null;
      })
      .catch((e) => {
        // A root missing from this archive keeps its thread ID as the label.
        if (!isAbortError(e)) console.warn("Failed to read usage root session:", e);
      })
      .finally(() => this.rootRead.finish(signal));
  }
}
