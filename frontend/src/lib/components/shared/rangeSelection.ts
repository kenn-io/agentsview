import {
  allFromDate,
  daysAgo,
  localDateStr,
  presetRange,
  todayStr,
  type DateRange,
} from "./dateRangeSelector.js";

/**
 * The three ways a user can pick a range with the unified RangePicker. Every
 * view selects exactly one of these; resolveRange() turns any of them into a
 * concrete {from, to} the stores already understand.
 *
 * - relative: a rolling window ending today (last N days; days === 0 means
 *   "all", anchored to the earliest session).
 * - calendar: a single day/week/month period the user can step through.
 * - custom: an explicit from/to span.
 */
export type RangeMode = "relative" | "calendar" | "custom";
export type CalendarUnit = "day" | "week" | "month";

export interface RelativeSelection {
  mode: "relative";
  days: number;
}
export interface CalendarSelection {
  mode: "calendar";
  unit: CalendarUnit;
  /** Any YYYY-MM-DD inside the period; bounds derive from it. */
  anchor: string;
}
export interface CustomSelection {
  mode: "custom";
  from: string;
  to: string;
}
export type RangeSelection =
  | RelativeSelection
  | CalendarSelection
  | CustomSelection;

export interface RelativePreset {
  /** Compact pill label. */
  label: string;
  /** Trigger-button label. */
  longLabel: string;
  /** Days back from today; 0 means all-time. */
  days: number;
}

export const RELATIVE_PRESETS: RelativePreset[] = [
  { label: "7d", longLabel: "Last 7 days", days: 7 },
  { label: "30d", longLabel: "Last 30 days", days: 30 },
  { label: "90d", longLabel: "Last 90 days", days: 90 },
  { label: "1y", longLabel: "Last year", days: 365 },
  { label: "All", longLabel: "All time", days: 0 },
];

export const CALENDAR_UNITS: { unit: CalendarUnit; label: string }[] = [
  { unit: "day", label: "Day" },
  { unit: "week", label: "Week" },
  { unit: "month", label: "Month" },
];

const MONTHS_SHORT = [
  "Jan", "Feb", "Mar", "Apr", "May", "Jun",
  "Jul", "Aug", "Sep", "Oct", "Nov", "Dec",
];
const MONTHS_LONG = [
  "January", "February", "March", "April", "May", "June",
  "July", "August", "September", "October", "November", "December",
];

/** Parse a YYYY-MM-DD date string as local midnight. */
function parseLocal(date: string): Date {
  return new Date(date + "T00:00:00");
}

/**
 * The inclusive from/to bounds of a calendar period containing `anchor`.
 * Day is a single date; week is the Monday-Sunday ISO week; month is the
 * calendar month.
 */
export function periodBounds(unit: CalendarUnit, anchor: string): DateRange {
  const d = parseLocal(anchor);
  if (unit === "day") {
    return { from: anchor, to: anchor };
  }
  if (unit === "week") {
    // getDay(): 0=Sun..6=Sat. Days since Monday = (day + 6) % 7.
    const sinceMonday = (d.getDay() + 6) % 7;
    const monday = new Date(d);
    monday.setDate(d.getDate() - sinceMonday);
    const sunday = new Date(monday);
    sunday.setDate(monday.getDate() + 6);
    return { from: localDateStr(monday), to: localDateStr(sunday) };
  }
  const first = new Date(d.getFullYear(), d.getMonth(), 1);
  const last = new Date(d.getFullYear(), d.getMonth() + 1, 0);
  return { from: localDateStr(first), to: localDateStr(last) };
}

/**
 * Move a calendar anchor one period in `dir`: one day, seven days, or one
 * calendar month (clamping the day so Jan 31 -> Feb 28 rather than overflowing
 * into March). Mirrors the activity store's step() so period navigation is
 * unchanged.
 */
export function stepAnchor(
  unit: CalendarUnit,
  anchor: string,
  dir: -1 | 1,
): string {
  const d = parseLocal(anchor);
  if (unit === "day") {
    d.setDate(d.getDate() + dir);
  } else if (unit === "week") {
    d.setDate(d.getDate() + 7 * dir);
  } else {
    const target = new Date(d.getFullYear(), d.getMonth() + dir, 1);
    const lastDay = new Date(
      target.getFullYear(),
      target.getMonth() + 1,
      0,
    ).getDate();
    target.setDate(Math.min(d.getDate(), lastDay));
    d.setTime(target.getTime());
  }
  return localDateStr(d);
}

/** Turn any selection into the concrete {from, to} the stores consume. */
export function resolveRange(
  sel: RangeSelection,
  earliestSession?: string | null,
): DateRange {
  switch (sel.mode) {
    case "relative":
      return presetRange(sel.days, earliestSession);
    case "calendar":
      return periodBounds(sel.unit, sel.anchor);
    case "custom":
      return { from: sel.from, to: sel.to };
  }
}

/** Human label for the period a calendar selection currently points at. */
export function calendarLabel(unit: CalendarUnit, anchor: string): string {
  const d = parseLocal(anchor);
  if (unit === "day") {
    return `${MONTHS_SHORT[d.getMonth()]} ${d.getDate()}, ${d.getFullYear()}`;
  }
  if (unit === "month") {
    return `${MONTHS_LONG[d.getMonth()]} ${d.getFullYear()}`;
  }
  const start = parseLocal(periodBounds("week", anchor).from);
  return `Week of ${MONTHS_SHORT[start.getMonth()]} ${start.getDate()}`;
}

/** Short label for the trigger button reflecting the current selection. */
export function rangeLabel(sel: RangeSelection): string {
  if (sel.mode === "relative") {
    const preset = RELATIVE_PRESETS.find((p) => p.days === sel.days);
    return preset ? preset.longLabel : `Last ${sel.days} days`;
  }
  if (sel.mode === "calendar") {
    return calendarLabel(sel.unit, sel.anchor);
  }
  if (!sel.from || !sel.to) return "Custom range";
  const from = parseLocal(sel.from);
  const to = parseLocal(sel.to);
  if (sel.from === sel.to) {
    return `${MONTHS_SHORT[from.getMonth()]} ${from.getDate()}`;
  }
  return (
    `${MONTHS_SHORT[from.getMonth()]} ${from.getDate()} - ` +
    `${MONTHS_SHORT[to.getMonth()]} ${to.getDate()}`
  );
}

/**
 * Build the default selection for a tab the user just switched to, seeded from
 * the current selection so switching tabs never jumps the visible range
 * unexpectedly. `current` supplies a sensible anchor/range; `earliestSession`
 * feeds the "All" fallback.
 */
export function defaultForMode(
  mode: RangeMode,
  current: RangeSelection,
  earliestSession?: string | null,
): RangeSelection {
  if (mode === "relative") {
    return { mode: "relative", days: 30 };
  }
  if (mode === "calendar") {
    const anchor =
      current.mode === "custom" && current.to
        ? current.to
        : current.mode === "calendar"
          ? current.anchor
          : todayStr();
    const unit = current.mode === "calendar" ? current.unit : "week";
    return { mode: "calendar", unit, anchor };
  }
  const resolved = resolveRange(current, earliestSession);
  return { mode: "custom", from: resolved.from, to: resolved.to };
}

/**
 * Reconstruct the picker selection for stores that track a rolling-vs-pinned
 * window (analytics, usage). A non-pinned window is the rolling preset; a
 * pinned range that exactly matches the all-time bounds shows as the "All"
 * preset; anything else is a custom range.
 */
export function selectionFromWindow(opts: {
  isPinned: boolean;
  windowDays: number;
  from: string;
  to: string;
  earliestSession?: string | null;
}): RangeSelection {
  if (!opts.isPinned) {
    return { mode: "relative", days: opts.windowDays };
  }
  const all = presetRange(0, opts.earliestSession);
  if (opts.from === all.from && opts.to === all.to) {
    return { mode: "relative", days: 0 };
  }
  return { mode: "custom", from: opts.from, to: opts.to };
}

/**
 * Reconstruct a selection for stores that only persist a from/to span (trends,
 * insights). If the span exactly matches a relative preset's current bounds it
 * shows as that preset (so a default 1y range reads "Last year"); otherwise it
 * is a custom range.
 */
export function selectionFromRange(
  from: string,
  to: string,
  earliestSession?: string | null,
): RangeSelection {
  for (const preset of RELATIVE_PRESETS) {
    const range = presetRange(preset.days, earliestSession);
    if (range.from === from && range.to === to) {
      return { mode: "relative", days: preset.days };
    }
  }
  return { mode: "custom", from, to };
}

export { allFromDate, daysAgo, localDateStr, todayStr };
export type { DateRange };
