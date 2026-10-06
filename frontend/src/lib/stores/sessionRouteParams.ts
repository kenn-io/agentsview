import { rollingRange } from "../utils/dates.js";

export const SESSION_ANALYTICS_WINDOW_PARAM = "window_days";
export const SESSION_LABEL_PARAM = "label";
export const SESSION_PR_PARAM = "pr";

// Route params are a flat string record, but `label` repeats in the URL
// (?label=a&label=b) to match the API. Inside the record its values are
// joined with a newline: the server rejects labels containing control
// characters, so the separator never collides with a real label.
const REPEATED_QUERY_PARAMS: ReadonlySet<string> = new Set([SESSION_LABEL_PARAM]);
const REPEATED_VALUE_SEPARATOR = "\n";

/** Trims label filter values, drops empty ones, and removes duplicates
 *  while keeping the first-seen order. */
export function normalizeLabelFilters(labels: unknown): string[] {
  if (!Array.isArray(labels)) return [];
  const out: string[] = [];
  for (const value of labels) {
    if (typeof value !== "string") continue;
    const label = value.trim();
    if (label && !out.includes(label)) out.push(label);
  }
  return out;
}

/** Encodes label filters as one route param value. */
export function joinLabelFilterParam(labels: readonly string[]): string {
  return normalizeLabelFilters(labels).join(REPEATED_VALUE_SEPARATOR);
}

/** Decodes a route param value written by joinLabelFilterParam. */
export function splitLabelFilterParam(raw: string | undefined): string[] {
  if (!raw) return [];
  return normalizeLabelFilters(raw.split(REPEATED_VALUE_SEPARATOR));
}

/** Reads a query string into route params. Repeated keys such as `label`
 *  keep every value; other keys keep the last value. */
export function routeParamsFromSearch(search: URLSearchParams): Record<string, string> {
  const params: Record<string, string> = {};
  for (const [key, value] of search) {
    if (REPEATED_QUERY_PARAMS.has(key) && params[key] !== undefined) {
      params[key] = `${params[key]}${REPEATED_VALUE_SEPARATOR}${value}`;
    } else {
      params[key] = value;
    }
  }
  return params;
}

/** Writes route params as a query string, expanding repeated keys. */
export function searchFromRouteParams(params: Record<string, string>): URLSearchParams {
  const search = new URLSearchParams();
  for (const [key, value] of Object.entries(params)) {
    if (REPEATED_QUERY_PARAMS.has(key)) {
      for (const part of value.split(REPEATED_VALUE_SEPARATOR)) {
        if (part) search.append(key, part);
      }
    } else {
      search.append(key, value);
    }
  }
  return search;
}

/** True when URL params contain session filter keys (deep-link). */
export const SESSION_FILTER_KEYS: ReadonlySet<string> = new Set([
  "project",
  "starred",
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
  SESSION_LABEL_PARAM,
  SESSION_PR_PARAM,
  SESSION_ANALYTICS_WINDOW_PARAM,
]);

export function hasFilterParams(params: Record<string, string>): boolean {
  return Object.keys(params).some((k) => SESSION_FILTER_KEYS.has(k));
}

function hasFixedSessionDateParams(params: Record<string, string>): boolean {
  return !!params["date"] || !!params["date_from"] || !!params["date_to"];
}

export function hasSessionDateIntent(params: Record<string, string>): boolean {
  return hasFixedSessionDateParams(params) || !!params[SESSION_ANALYTICS_WINDOW_PARAM];
}

export function hasSessionRouteDateIntent(route: string, params: Record<string, string>): boolean {
  return route === "sessions" && hasSessionDateIntent(params);
}

export function sessionDateIntentCleared(
  currentParams: Record<string, string>,
  nextParams: Record<string, string>,
): boolean {
  return hasSessionDateIntent(currentParams) && !hasSessionDateIntent(nextParams);
}

/** Parses a window_days param; null unless a canonical positive integer. */
export function parseWindowDaysParam(raw: string | undefined): number | null {
  if (!raw) return null;
  const n = Number.parseInt(raw, 10);
  if (!Number.isInteger(n) || n <= 0 || String(n) !== raw) return null;
  return n;
}

function isValidWindowDaysParam(raw: string | undefined): raw is string {
  return parseWindowDaysParam(raw) !== null;
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

function currentSessionRouteParams(currentParams: Record<string, string>): Record<string, string> {
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

export function filterParamsEqual(a: Record<string, string>, b: Record<string, string>): boolean {
  for (const k of SESSION_FILTER_KEYS) {
    if ((a[k] ?? "") !== (b[k] ?? "")) return false;
  }
  return true;
}
