import type { ResponseTiming } from "../api/runtime.js";
import { getLocale, m } from "../i18n/index.js";

const MINUTE_MS = 60_000;
const HOUR_MS = 60 * MINUTE_MS;
const DAY_MS = 24 * HOUR_MS;

/**
 * Default auto-refresh cadence shared by every dashboard. Long enough that a
 * session actively writing files never thrashes the aggregation, short enough
 * that an idle dashboard stays current. Override per call site when a view
 * needs a different cadence.
 */
export const DEFAULT_REFRESH_INTERVAL_MS = 5 * MINUTE_MS;

export function formatRefreshAge(updatedAt: number | null | undefined, now = Date.now()): string {
  if (updatedAt == null) return m.shared_refresh_not_updated();

  const ageMs = Math.max(0, now - updatedAt);
  if (ageMs < MINUTE_MS) return m.shared_refresh_just_now();
  if (ageMs < HOUR_MS) {
    return m.shared_refresh_minutes_ago({
      count: Math.floor(ageMs / MINUTE_MS),
    });
  }
  if (ageMs < DAY_MS) {
    return m.shared_refresh_hours_ago({
      count: Math.floor(ageMs / HOUR_MS),
    });
  }
  return m.shared_refresh_days_ago({
    count: Math.floor(ageMs / DAY_MS),
  });
}

/**
 * Label variants the refresh control's age box must fit without resizing.
 * Covers every branch of `formatRefreshAge` at its widest digit budget so
 * the box is measured once against the widest localized rendering.
 */
export function refreshAgeWidthSamples(): string[] {
  return [
    m.shared_refresh_not_updated(),
    m.shared_refresh_just_now(),
    m.shared_refresh_minutes_ago({ count: 59 }),
    m.shared_refresh_hours_ago({ count: 23 }),
    m.shared_refresh_days_ago({ count: 999 }),
  ];
}

const SECOND_MS = 1000;

/** Phases of one request, as the browser's network panel draws them: the
 * server (sent until the first byte back), the transfer of the body, and the
 * page rendering the data in. */
export type QueryPhase = "wait" | "download" | "apply";

export interface QuerySegment {
  phase: QueryPhase;
  startMs: number;
  durationMs: number;
}

/** One measured step of a page's last data query. `name` is a stable key
 * (see `formatQueryStepLabel`), never user-facing on its own. `startMs` is
 * the offset from the query's start, so parallel steps can be drawn on one
 * time axis. `segments` split the step into request phases when known.
 * `running` marks a step of an in-flight query that has not finished yet;
 * the refresh control draws it up to the current time. */
export interface QueryStep {
  name: string;
  startMs: number;
  durationMs: number;
  segments?: QuerySegment[];
  running?: boolean;
}

/**
 * Splits one request into wait, download, and apply segments, offset from
 * `originMs` (the query's start on the `performance.now()` clock). The apply
 * segment runs from `applyStartedAt` (the body's arrival unless the store
 * had to wait for a sibling request first) to `appliedAt`, when the page
 * finished applying the parsed response; any wait in between stays a gap.
 */
export function querySegmentsFrom(
  timing: ResponseTiming,
  appliedAt: number,
  originMs: number,
  applyStartedAt: number = timing.bodyAt,
): QuerySegment[] {
  return [
    {
      phase: "wait",
      startMs: timing.sentAt - originMs,
      durationMs: timing.headersAt - timing.sentAt,
    },
    {
      phase: "download",
      startMs: timing.headersAt - originMs,
      durationMs: timing.bodyAt - timing.headersAt,
    },
    { phase: "apply", startMs: applyStartedAt - originMs, durationMs: appliedAt - applyStartedAt },
  ];
}

/**
 * A query step for one request: from the moment it was sent to the moment
 * its data was applied, with phase segments. Falls back to the caller's own
 * start when the request carried no timing (a mocked or non-JSON response).
 */
export function queryStepFrom(
  name: string,
  timing: ResponseTiming | undefined,
  startedAt: number,
  appliedAt: number,
  originMs: number,
  applyStartedAt?: number,
): QueryStep {
  const sentAt = timing?.sentAt ?? startedAt;
  const step: QueryStep = { name, startMs: sentAt - originMs, durationMs: appliedAt - sentAt };
  if (timing) step.segments = querySegmentsFrom(timing, appliedAt, originMs, applyStartedAt);
  return step;
}

export function formatQueryPhaseLabel(phase: QueryPhase): string {
  switch (phase) {
    case "wait":
      return m.shared_refresh_phase_wait();
    case "download":
      return m.shared_refresh_phase_download();
    case "apply":
      return m.shared_refresh_phase_apply();
  }
}

/**
 * Tick positions for a time axis spanning `axisMs`: the smallest 1, 2, or 5
 * times a power of ten step that fits in at most four intervals, from zero.
 * Four keeps labels such as "400 ms" from running together on the 200 px
 * track. Never finer than a millisecond, since labels are whole
 * milliseconds.
 */
export function queryAxisTicks(axisMs: number): number[] {
  if (!(axisMs > 0)) return [0];
  const raw = Math.max(axisMs / 4, 1);
  const magnitude = 10 ** Math.floor(Math.log10(raw));
  const step = [1, 2, 5, 10].map((m) => m * magnitude).find((candidate) => candidate >= raw)!;
  const ticks: number[] = [];
  for (let tick = 0; tick <= axisMs; tick += step) ticks.push(tick);
  return ticks;
}

/** Axis tick label: exact, not rounded like `formatQueryDuration`. */
export function formatQueryTick(ms: number): string {
  if (ms === 0) return "0";
  const locale = getLocale();
  if (ms < 1000) {
    return m.shared_refresh_duration_ms({
      value: new Intl.NumberFormat(locale, { maximumFractionDigits: 0 }).format(ms),
    });
  }
  return m.shared_refresh_duration_seconds({
    value: new Intl.NumberFormat(locale, { maximumFractionDigits: 1 }).format(ms / 1000),
  });
}

const STEP_LABELS: Record<string, () => string> = {
  summary: () => m.shared_refresh_step_summary(),
  activity: () => m.shared_refresh_step_activity(),
  heatmap: () => m.shared_refresh_step_heatmap(),
  projects: () => m.shared_refresh_step_projects(),
  hourOfWeek: () => m.shared_refresh_step_hour_of_week(),
  sessionShape: () => m.shared_refresh_step_session_shape(),
  velocity: () => m.shared_refresh_step_velocity(),
  tools: () => m.shared_refresh_step_tools(),
  skills: () => m.shared_refresh_step_skills(),
  topSessions: () => m.shared_refresh_step_top_sessions(),
  contextSummary: () => m.shared_refresh_step_context_summary(),
  signals: () => m.shared_refresh_step_signals(),
  comparison: () => m.shared_refresh_step_comparison(),
  pairwise: () => m.shared_refresh_step_pairwise(),
  report: () => m.shared_refresh_step_report(),
  sessions: () => m.shared_refresh_step_sessions(),
  usage: () => m.shared_refresh_step_usage(),
  scan: () => m.shared_refresh_step_scan(),
  finalize: () => m.shared_refresh_step_finalize(),
  entries: () => m.shared_refresh_step_entries(),
  status: () => m.shared_refresh_step_status(),
};

/** Localized name for a query step; unknown keys render as-is. */
export function formatQueryStepLabel(name: string): string {
  return STEP_LABELS[name]?.() ?? name;
}

/**
 * How long the last data query took, in a short fixed-format string:
 * whole milliseconds under a second, whole seconds under a minute, then
 * minutes plus zero-padded seconds. Sub-second precision stops mattering
 * once a query takes seconds, so no decimals. Each unit boundary is
 * applied after rounding so a value like 999.6 ms reads "1 s" rather
 * than "1000 ms". Returns "" for a missing duration so the reserved box
 * stays empty instead of showing a placeholder.
 */
export function formatQueryDuration(durationMs: number | null | undefined): string {
  if (durationMs == null || !Number.isFinite(durationMs)) return "";
  const locale = getLocale();
  const ms = Math.max(0, durationMs);
  const wholeMs = Math.round(ms);
  if (wholeMs < SECOND_MS) {
    return m.shared_refresh_duration_ms({
      value: new Intl.NumberFormat(locale, { maximumFractionDigits: 0 }).format(wholeMs),
    });
  }
  const totalSeconds = Math.round(ms / SECOND_MS);
  if (totalSeconds < 60) {
    return m.shared_refresh_duration_seconds({
      value: new Intl.NumberFormat(locale, { maximumFractionDigits: 0 }).format(totalSeconds),
    });
  }
  return m.shared_refresh_duration_minutes({
    minutes: new Intl.NumberFormat(locale, { maximumFractionDigits: 0 }).format(
      Math.floor(totalSeconds / 60),
    ),
    seconds: new Intl.NumberFormat(locale, {
      minimumIntegerDigits: 2,
      maximumFractionDigits: 0,
    }).format(totalSeconds % 60),
  });
}

/** Widest rendering of each `formatQueryDuration` unit. */
export function queryDurationWidthSamples(): string[] {
  return [
    formatQueryDuration(999),
    formatQueryDuration(59 * SECOND_MS),
    formatQueryDuration(99 * MINUTE_MS + 59 * SECOND_MS),
  ];
}

/**
 * The refresh control's label: the age, followed by how long the last query
 * took when one has completed ("Updated just now · 2 s"). One phrase so the
 * number reads as part of the sentence instead of a stray figure.
 */
export function formatRefreshStatus(
  updatedAt: number | null | undefined,
  durationMs: number | null | undefined,
  now = Date.now(),
): string {
  const age = formatRefreshAge(updatedAt, now);
  const duration = formatQueryDuration(durationMs);
  if (duration === "") return age;
  return m.shared_refresh_age_with_duration({ age, duration });
}

/**
 * The label while a page reports progress in place of the age: the
 * progress text, then how long the running query has taken so far.
 */
export function formatRefreshProgress(
  status: string,
  elapsedMs: number | null | undefined,
): string {
  const duration = formatQueryDuration(elapsedMs);
  if (duration === "") return status;
  return m.shared_refresh_age_with_duration({ age: status, duration });
}

/**
 * Every age variant, and every progress text a page can show in its place,
 * paired with every duration unit at its widest, so the label box is
 * measured once against the widest localized phrase it can show and never
 * changes width afterwards.
 */
export function refreshStatusWidthSamples(statuses: readonly string[] = []): string[] {
  const durations = queryDurationWidthSamples();
  return [...refreshAgeWidthSamples(), ...statuses].flatMap((age) =>
    durations.map((duration) => m.shared_refresh_age_with_duration({ age, duration })),
  );
}

export function createRefreshScheduler(refresh: () => void | Promise<void>, intervalMs: number) {
  let timer: ReturnType<typeof setTimeout> | undefined;

  function stop() {
    if (timer !== undefined) {
      clearTimeout(timer);
      timer = undefined;
    }
  }

  function runAndReschedule() {
    stop();
    void refresh();
    timer = setTimeout(runAndReschedule, intervalMs);
  }

  // Arm the interval without an immediate refresh. Callers that load their
  // initial data separately (e.g. after URL/filter hydration) use this so the
  // first automatic refresh lands one interval out instead of racing mount.
  function scheduleNext() {
    stop();
    timer = setTimeout(runAndReschedule, intervalMs);
  }

  return {
    refreshNow: runAndReschedule,
    scheduleNext,
    stop,
  };
}
