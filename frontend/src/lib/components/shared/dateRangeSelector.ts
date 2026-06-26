import {
  daysAgo,
  localDateStr,
  today,
} from "../../utils/dates.js";

export interface DateRange {
  from: string;
  to: string;
}

export interface DateRangePreset {
  label: string;
  days: number;
}

export const DATE_RANGE_PRESETS: DateRangePreset[] = [
  { label: "7d", days: 7 },
  { label: "30d", days: 30 },
  { label: "90d", days: 90 },
  { label: "1y", days: 365 },
  { label: "All", days: 0 },
];

export { daysAgo, localDateStr };

export function todayStr(): string {
  return today();
}

export function allFromDate(earliestSession: string | null | undefined): string {
  if (earliestSession && earliestSession.length >= 10) {
    return earliestSession.slice(0, 10);
  }
  return daysAgo(365);
}

export function presetRange(
  days: number,
  earliestSession: string | null | undefined,
): DateRange {
  return {
    from: days === 0 ? allFromDate(earliestSession) : daysAgo(days),
    to: todayStr(),
  };
}

export function isPresetActive(
  from: string,
  to: string,
  days: number,
  earliestSession: string | null | undefined,
): boolean {
  const range = presetRange(days, earliestSession);
  return from === range.from && to === range.to;
}

export function activePresetDays(
  from: string,
  to: string,
  earliestSession: string | null | undefined,
  rollingDays?: number | null,
  isPinned?: boolean,
): number | null {
  if (!isPinned && rollingDays != null) {
    return rollingDays;
  }

  const matches = DATE_RANGE_PRESETS.filter((preset) =>
    isPresetActive(from, to, preset.days, earliestSession),
  );
  if (matches.length === 0) return null;

  const all = matches.find((preset) => preset.days === 0);
  if (all) return all.days;
  return matches[0]?.days ?? null;
}
