import { rollingRange } from "../utils/dates.js";

export const SESSION_ANALYTICS_WINDOW_PARAM = "window_days";

/** True when URL params contain session filter keys (deep-link). */
export const SESSION_FILTER_KEYS: ReadonlySet<string> = new Set([
  "project",
  "machine",
  "agent",
  "termination",
  "date",
  "date_from",
  "date_to",
  "active_since",
  "exclude_project",
  "min_messages",
  "max_messages",
  "min_user_messages",
  "include_one_shot",
  "include_automated",
  SESSION_ANALYTICS_WINDOW_PARAM,
]);

export function hasFilterParams(params: Record<string, string>): boolean {
  return Object.keys(params).some((k) => SESSION_FILTER_KEYS.has(k));
}

function hasFixedSessionDateParams(
  params: Record<string, string>,
): boolean {
  return !!params["date"] || !!params["date_from"] || !!params["date_to"];
}

export function hasSessionDateIntent(
  params: Record<string, string>,
): boolean {
  return hasFixedSessionDateParams(params) ||
    !!params[SESSION_ANALYTICS_WINDOW_PARAM];
}

export function hasSessionRouteDateIntent(
  route: string,
  params: Record<string, string>,
): boolean {
  return route === "sessions" && hasSessionDateIntent(params);
}

export function sessionDateIntentCleared(
  currentParams: Record<string, string>,
  nextParams: Record<string, string>,
): boolean {
  return hasSessionDateIntent(currentParams) &&
    !hasSessionDateIntent(nextParams);
}

function isValidWindowDaysParam(raw: string | undefined): raw is string {
  if (!raw) return false;
  const n = Number.parseInt(raw, 10);
  return Number.isInteger(n) && n > 0 && String(n) === raw;
}

function fixedSessionDateParamsEqual(
  a: Record<string, string>,
  b: Record<string, string>,
): boolean {
  return (
    (a["date"] ?? "") === (b["date"] ?? "") &&
    (a["date_from"] ?? "") === (b["date_from"] ?? "") &&
    (a["date_to"] ?? "") === (b["date_to"] ?? "")
  );
}

function fixedSessionDateParamsMatchRollingWindow(
  params: Record<string, string>,
  windowDays: string,
  now: Date,
): boolean {
  const range = rollingRange(Number.parseInt(windowDays, 10), now);
  return (
    !params["date"] &&
    (params["date_from"] ?? "") === range.from &&
    (params["date_to"] ?? "") === range.to
  );
}

function shouldPreserveSessionWindowDays(
  nextParams: Record<string, string>,
  currentParams: Record<string, string>,
  now: Date,
): boolean {
  const windowDays = currentParams[SESSION_ANALYTICS_WINDOW_PARAM];
  if (!isValidWindowDaysParam(windowDays)) return false;
  const nextHasFixedDates = hasFixedSessionDateParams(nextParams);
  const currentHasFixedDates = hasFixedSessionDateParams(currentParams);
  return (
    (!nextHasFixedDates && !currentHasFixedDates) ||
    fixedSessionDateParamsMatchRollingWindow(nextParams, windowDays, now) ||
    fixedSessionDateParamsEqual(nextParams, currentParams)
  );
}

export function sessionRouteParamsForFilters(
  filterParams: Record<string, string>,
  currentParams: Record<string, string>,
  now: Date = new Date(),
): Record<string, string> {
  const next = { ...filterParams };
  const windowDays = currentParams[SESSION_ANALYTICS_WINDOW_PARAM];
  if (shouldPreserveSessionWindowDays(next, currentParams, now)) {
    next[SESSION_ANALYTICS_WINDOW_PARAM] = windowDays!;
  }
  return next;
}

export function currentSessionRouteParams(
  currentParams: Record<string, string>,
): Record<string, string> {
  const next: Record<string, string> = {};
  for (const key of SESSION_FILTER_KEYS) {
    const value = currentParams[key];
    if (value !== undefined) {
      next[key] = value;
    }
  }
  return next;
}

export function sessionRouteParamsForDetailExit(
  filterParams: Record<string, string>,
  currentParams: Record<string, string>,
): Record<string, string> {
  const currentRouteParams = currentSessionRouteParams(currentParams);
  if (hasFilterParams(currentRouteParams)) return currentRouteParams;
  return sessionRouteParamsForFilters(filterParams, currentParams);
}

export function filterParamsEqual(
  a: Record<string, string>,
  b: Record<string, string>,
): boolean {
  for (const k of SESSION_FILTER_KEYS) {
    if ((a[k] ?? "") !== (b[k] ?? "")) return false;
  }
  return true;
}
