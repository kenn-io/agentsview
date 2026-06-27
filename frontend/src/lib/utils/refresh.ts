import { m } from "../i18n/index.js";

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

export function formatRefreshAge(
  updatedAt: number | null | undefined,
  now = Date.now(),
): string {
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

export function createRefreshScheduler(
  refresh: () => void | Promise<void>,
  intervalMs: number,
) {
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
