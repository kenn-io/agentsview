import type { DbCaptureCoverageView, DbPlanUsageView } from "../api/generated/index.js";
import { getLocale, m } from "../i18n/index.js";
import { formatTimestamp } from "./format.js";

// The agent whose sessions always carry a plan usage state, even before
// the first usage request finishes.
const PLAN_USAGE_AGENT = "codex-cloud";

// Outcomes recorded after the newest row that say the row may be stale, so
// the outcome shows next to the row instead of replacing it.
const STALE_ROW_OUTCOMES = new Set([
  "left_out",
  "account_unavailable",
  "too_many_descendants",
  "could_not_check",
]);

/**
 * Formats a plan usage percentage or credit amount: two decimal places with
 * trailing zeros dropped, and "<0.01" for a nonzero value that would round
 * to zero, so a tiny share never reads as no usage at all.
 */
export function formatPlanNumber(value: number): string {
  const format = new Intl.NumberFormat(getLocale(), {
    maximumFractionDigits: 2,
  });
  if (value !== 0 && Math.round(value * 100) === 0) {
    return `<${format.format(0.01)}`;
  }
  return format.format(value);
}

/**
 * Formats credits sent as a decimal string, which can use exponent form
 * ("0E-10"). A string that is not a number is shown as sent.
 */
export function formatPlanCredits(credits: string): string {
  const value = Number(credits);
  return Number.isFinite(value) ? formatPlanNumber(value) : credits;
}

function creditsText(credits: string): string {
  const value = Number(credits);
  return m.plan_usage_credits({
    count: Number.isFinite(value) ? value : 0,
    countLabel: formatPlanCredits(credits),
  });
}

export interface PlanUsageGroupRow {
  key: string;
  label: string;
  weekly: string;
  fiveHour: string;
  credits: string;
}

export interface PlanUsageDisplay {
  /** Short text for the collapsed control. */
  trigger: string;
  /** The state line: what AgentsView knows and as of when. Empty when the
   * thread is counted under another, which shows as a link instead. */
  headline: string;
  /** An outcome newer than the row that says the row may be stale. */
  staleNote: string | null;
  /** Thread-level figures, one line each. */
  totals: string[];
  groups: PlanUsageGroupRow[];
  /** Whether the row holds figures, so the credits note applies. */
  hasFigures: boolean;
  /** The usage root's session ID when this thread is counted under it. */
  countedUnder: string | null;
}

function hasFigures(plan: DbPlanUsageView): boolean {
  return plan.weekly_percent != null || plan.five_hour_percent != null || plan.credits != null;
}

function outcomeMessage(plan: DbPlanUsageView): string | null {
  switch (plan.outcome_kind) {
    case "left_out":
      return m.plan_usage_not_reported_yet({
        time: formatTimestamp(plan.outcome_at),
      });
    case "settled_no_row":
      return m.plan_usage_not_reported();
    case "account_unavailable":
      return m.plan_usage_account_unavailable();
    case "too_many_descendants":
      return m.plan_usage_too_many_subagents();
    case "could_not_check":
      return m.plan_usage_could_not_check();
    default:
      return null;
  }
}

// staleNote says what a check after the row found, for an outcome that
// leaves the row's figures possibly out of date. "Not reported yet" would
// promise a report that may never come, so a left-out chat says so.
function staleNote(plan: DbPlanUsageView): string | null {
  if (!STALE_ROW_OUTCOMES.has(plan.outcome_kind ?? "")) return null;
  const reason =
    plan.outcome_kind === "left_out" ? m.plan_usage_left_out_later() : outcomeMessage(plan);
  return m.plan_usage_stale({ reason: reason ?? "", time: formatTimestamp(plan.outcome_at) });
}

function groupLabel(group: DbPlanUsageView["groups"][number]): string {
  return [group.model, group.reasoning_effort, group.speed].filter(Boolean).join(" · ");
}

function percentCell(value: number | null): string {
  return value == null ? "" : `${formatPlanNumber(value)}%`;
}

/**
 * Describes a session's plan usage for session details, or returns null
 * when the session has none to show: it is not a codex-cloud thread and
 * carries no plan usage.
 */
export function describePlanUsage(
  agent: string,
  plan: DbPlanUsageView | null | undefined,
): PlanUsageDisplay | null {
  if (!plan) {
    if (agent !== PLAN_USAGE_AGENT) return null;
    return {
      trigger: m.plan_usage_title(),
      headline: m.plan_usage_not_checked(),
      staleNote: null,
      totals: [],
      groups: [],
      hasFigures: false,
      countedUnder: null,
    };
  }
  const base = {
    trigger: m.plan_usage_title(),
    staleNote: null,
    totals: [],
    groups: [],
    hasFigures: false,
    countedUnder: null,
  };
  if (plan.outcome_kind === "counted_under" && plan.counted_under) {
    return {
      ...base,
      headline: "",
      countedUnder: plan.counted_under,
    };
  }
  if (!hasFigures(plan)) {
    const outcome = outcomeMessage(plan);
    if (outcome) return { ...base, headline: outcome };
    if (plan.final) return { ...base, headline: m.plan_usage_not_reported() };
    const time = plan.outcome_at || plan.data_as_of;
    return {
      ...base,
      headline: time
        ? m.plan_usage_not_reported_yet({ time: formatTimestamp(time) })
        : m.plan_usage_not_checked(),
    };
  }

  const time = formatTimestamp(plan.data_as_of);
  let headline = m.plan_usage_as_of({ time });
  if (plan.data_status === "partial") {
    headline = plan.final ? m.plan_usage_partial({ time }) : m.plan_usage_still_updating({ time });
  }
  const totals: string[] = [];
  if (plan.weekly_percent != null) {
    totals.push(
      m.plan_usage_weekly_share({
        percent: formatPlanNumber(plan.weekly_percent),
      }),
    );
  }
  if (plan.five_hour_percent != null) {
    totals.push(
      m.plan_usage_five_hour_share({
        percent: formatPlanNumber(plan.five_hour_percent),
      }),
    );
  }
  if (plan.credits != null) totals.push(creditsText(plan.credits));

  return {
    trigger:
      plan.weekly_percent != null
        ? m.plan_usage_trigger_weekly({
            percent: formatPlanNumber(plan.weekly_percent),
          })
        : m.plan_usage_title(),
    headline,
    staleNote: staleNote(plan),
    totals,
    groups: plan.groups.map((group, index) => ({
      key: `${index}`,
      label: groupLabel(group),
      weekly: percentCell(group.weekly_percent),
      fiveHour: percentCell(group.five_hour_percent),
      credits: group.credits == null ? "" : formatPlanCredits(group.credits),
    })),
    hasFigures: true,
    countedUnder: null,
  };
}

/**
 * Describes live token capture for session details, one line each, or
 * returns an empty list when live capture never saw the session.
 */
export function describeCaptureCoverage(
  coverage: DbCaptureCoverageView | null | undefined,
  plan: DbPlanUsageView | null | undefined,
): string[] {
  if (!coverage) return [];
  const lines = [m.capture_tokens_live({ time: formatTimestamp(coverage.last_capture_at) })];
  const turns = coverage.uncaptured_turns;
  if (turns <= 0) return lines;
  const params = { count: turns, countLabel: turns.toLocaleString(getLocale()) };
  // A complete row with no later outcome counts every turn that ran before
  // its data time. Whether a missed turn ran before then is not known, so
  // the line names the time instead of claiming it.
  const covered =
    plan != null &&
    plan.data_status === "available" &&
    !plan.outcome_kind &&
    plan.data_as_of != null &&
    hasFigures(plan);
  lines.push(
    covered
      ? m.capture_turns_not_captured_covered({ ...params, time: formatTimestamp(plan.data_as_of) })
      : m.capture_turns_not_captured(params),
  );
  return lines;
}
