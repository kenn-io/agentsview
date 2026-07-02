import {
  allFromDate,
  daysAgo,
  localDateStr,
  presetRange,
  todayStr,
  type DateRange,
} from "./dateRangeSelector.js";
import { m } from "../../i18n/index.js";

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
  label: () => string;
  /** Trigger-button label. */
  longLabel: () => string;
  /** Days back from today; 0 means all-time. */
  days: number;
}

export const RELATIVE_PRESETS: RelativePreset[] = [
  { label: () => "7d", longLabel: () => m.shared_range_last_days({ count: 7 }), days: 7 },
  { label: () => "30d", longLabel: () => m.shared_range_last_days({ count: 30 }), days: 30 },
  { label: () => "90d", longLabel: () => m.shared_range_last_days({ count: 90 }), days: 90 },
  { label: () => "1y", longLabel: m.shared_range_preset_last_year, days: 365 },
  { label: m.shared_range_preset_all, longLabel: m.shared_range_preset_all_time, days: 0 },
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
