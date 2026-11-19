export const DEFAULT_WINDOW_DAYS = 30;

// 100 years is well beyond any realistic session history and stays
// inside Date#setDate's safe range, so rollingRange(MAX_WINDOW_DAYS)
// always produces valid YYYY-MM-DD strings.
const MAX_WINDOW_DAYS = 36500;

export function joinCsvParts(...parts: string[]): string {
  const out: string[] = [];
  const seen = new Set<string>();
  for (const part of parts) {
    for (const value of part.split(",")) {
      const trimmed = value.trim();
      if (!trimmed || seen.has(trimmed)) continue;
      seen.add(trimmed);
      out.push(trimmed);
    }
  }
  return out.join(",");
}

export interface UsageUrlState {
  from: string;
  to: string;
  isPinned: boolean;
  windowDays: number;
  excludedProjects: string;
  excludedProjectKeys: string;
  excludedAgents: string;
  excludedModels: string;
}

export function parseWindowDays(raw: string | undefined): number | null {
  if (!raw) return null;
  const n = Number.parseInt(raw, 10);
  if (!Number.isFinite(n) || n <= 0 || n > MAX_WINDOW_DAYS || String(n) !== raw) {
    return null;
  }
  return n;
}

export function buildUsageUrlParams(state: UsageUrlState): Record<string, string> {
  const params: Record<string, string> = {};
  if (state.isPinned) {
    if (state.from) params["from"] = state.from;
    if (state.to) params["to"] = state.to;
  } else if (state.windowDays > 0 && state.windowDays !== DEFAULT_WINDOW_DAYS) {
    params["window_days"] = String(state.windowDays);
  }
  if (state.excludedModels) {
    params["exclude_model"] = state.excludedModels;
  }
  if (state.excludedProjects) {
    params["exclude_project"] = state.excludedProjects;
  }
  // Shared-store project keys are scoped to the current aggregate archive
  // set. Keep them in live request state only; URLs outlive that scope.
  if (state.excludedAgents) {
    params["exclude_agent"] = state.excludedAgents;
  }
  return params;
}

const CSV_MERGE_URL_KEYS = new Set(["exclude_project"]);
const SESSION_DATE_URL_KEYS = new Set(["date", "date_from", "date_to"]);

export function mergeUsageAndSessionUrlParams(
  usageParams: Record<string, string>,
  sessionParams: Record<string, string>,
): Record<string, string> {
  const params = { ...usageParams };
  for (const [key, value] of Object.entries(sessionParams)) {
    if (SESSION_DATE_URL_KEYS.has(key)) continue;
    if (CSV_MERGE_URL_KEYS.has(key) && params[key]) {
      params[key] = joinCsvParts(params[key], value);
    } else {
      params[key] = value;
    }
  }
  return params;
}
