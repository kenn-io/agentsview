// Compact, non-localized duration abbreviations for rate-limit windows and
// reset countdowns (e.g. "5h", "7d", "2h 15m"), mirroring the existing
// non-localized unit abbreviations in utils/duration.ts (formatDuration).
// These are short technical units, not sentences, so they are not routed
// through the Paraglide message catalogues; the surrounding sentence
// ("Resets in {value}") is.

const MINUTES_PER_HOUR = 60;
const MINUTES_PER_DAY = MINUTES_PER_HOUR * 24;

/** Formats a rate-limit window length (in minutes) as a compact unit, e.g. "5h", "7d", "30m". Undefined (Codex reported no duration) renders as "—". */
export function formatWindowLength(minutes: number | undefined): string {
  if (minutes === undefined || !Number.isFinite(minutes) || minutes <= 0) return "—";
  if (minutes % MINUTES_PER_DAY === 0) return `${minutes / MINUTES_PER_DAY}d`;
  if (minutes % MINUTES_PER_HOUR === 0) return `${minutes / MINUTES_PER_HOUR}h`;
  if (minutes < MINUTES_PER_HOUR) return `${minutes}m`;
  const hours = Math.floor(minutes / MINUTES_PER_HOUR);
  const mins = minutes % MINUTES_PER_HOUR;
  return `${hours}h ${mins}m`;
}

/**
 * Converts a browser-local calendar date range (YYYY-MM-DD, inclusive on
 * both ends -- the shape the Usage page's date pickers and time-range
 * brush use) into UTC instant bounds for a rate-limit history request:
 * `since` is local midnight of `from`, and `until` is local midnight of
 * the day AFTER `to` (an exclusive upper bound). Building local Date
 * objects from the calendar fields (rather than parsing the strings as if
 * they were already UTC) ties the boundary to the viewer's own calendar
 * day; the exclusive next-day upper bound means a sub-second observation
 * on `to`'s last moment is never dropped by a same-day 23:59:59Z cutoff
 * that has no fractional part.
 */
export function localDateRangeToUTCBounds(from: string, to: string): { since: string; until: string } {
  const parseYMD = (date: string): [number, number, number] => {
    const [y, m, d] = date.split("-");
    return [Number(y), Number(m), Number(d)];
  };
  const [fy, fm, fd] = parseYMD(from);
  const [ty, tm, td] = parseYMD(to);
  return {
    since: new Date(fy, fm - 1, fd).toISOString(),
    until: new Date(ty, tm - 1, td + 1).toISOString(),
  };
}

/**
 * Formats the time remaining until a unix-seconds reset timestamp as a
 * compact countdown ("2h 15m", "3d 4h"), or null once the reset has passed
 * (the caller should show a "resets now"-style message instead).
 */
export function formatResetCountdown(
  resetsAtSeconds: number,
  nowMs: number = Date.now(),
): string | null {
  if (!Number.isFinite(resetsAtSeconds)) return null;
  const deltaMs = resetsAtSeconds * 1000 - nowMs;
  if (deltaMs <= 0) return null;
  const totalMinutes = Math.round(deltaMs / 60_000);
  const days = Math.floor(totalMinutes / MINUTES_PER_DAY);
  const hours = Math.floor((totalMinutes % MINUTES_PER_DAY) / MINUTES_PER_HOUR);
  const mins = totalMinutes % MINUTES_PER_HOUR;
  if (days > 0) return `${days}d ${hours}h`;
  if (hours > 0) return `${hours}h ${mins}m`;
  return `${Math.max(mins, 1)}m`;
}
