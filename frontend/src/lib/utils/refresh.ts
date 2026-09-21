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
 * Every age variant paired with every duration unit at its widest, so the
 * label box is measured once against the widest localized phrase it can
 * show and never changes width afterwards.
 */
export function refreshStatusWidthSamples(): string[] {
  const durations = queryDurationWidthSamples();
  return refreshAgeWidthSamples().flatMap((age) =>
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
