import { rollingRange } from "../utils/dates.js";
import {
  SESSION_LABEL_PARAM,
  SESSION_PR_PARAM,
  joinLabelFilterParam,
  normalizeLabelFilters,
  splitLabelFilterParam,
} from "./sessionRouteParams.js";

// Session filter values, their localStorage copy, and their URL form.

export interface Filters {
  project: string;
  machine: string;
  agent: string;
  termination: string;
  date: string;
  dateFrom: string;
  dateTo: string;
  recentlyActive: boolean;
  hideUnknownProject: boolean;
  minMessages: number;
  maxMessages: number;
  minUserMessages: number;
  includeOneShot: boolean;
  includeAutomated: boolean;
  automatedFilterSet: boolean;
  /** Exact labels a session must carry; every label must match. */
  labels: string[];
  /** Pull request reference: owner/repo or owner/repo#123. */
  pr: string;
}

export function defaultFilters(): Filters {
  return {
    project: "",
    machine: "",
    agent: "",
    termination: "",
    date: "",
    dateFrom: "",
    dateTo: "",
    recentlyActive: false,
    hideUnknownProject: false,
    minMessages: 0,
    maxMessages: 0,
    minUserMessages: 0,
    includeOneShot: true,
    includeAutomated: false,
    automatedFilterSet: false,
    labels: [],
    pr: "",
  };
}

const SESSION_FILTERS_KEY = "session-filters";
// v2 marks entries whose date bounds carry provenance: rolling bounds are
// persisted as intent (`windowDays`) and rematerialized on load, never as
// pinned dates. Unversioned entries predate that guarantee and may hold
// rolling bounds saved as if explicit (#1086).
const SESSION_FILTERS_VERSION = 2;

interface SavedFilters {
  filters: Filters;
  windowDays: number | null;
}

function validWindowDays(value: unknown): value is number {
  return typeof value === "number" && Number.isInteger(value) && value > 0;
}

export function loadSavedFilters(): SavedFilters {
  try {
    const raw = localStorage.getItem(SESSION_FILTERS_KEY);
    if (raw) {
      const { version, windowDays, ...saved } = JSON.parse(raw) as Partial<Filters> & {
        version?: unknown;
        windowDays?: unknown;
      };
      const filters = { ...defaultFilters(), ...saved };
      filters.labels = normalizeLabelFilters(filters.labels);
      filters.pr = typeof filters.pr === "string" ? filters.pr.trim() : "";
      // Deliberately `!==`, not `<`: an entry written by a newer (or older)
      // format is not trusted either way — dropping date bounds is the
      // fail-safe direction in both.
      if (version !== SESSION_FILTERS_VERSION) {
        // Legacy bounds have unknown provenance. Dropping them once is the
        // safe direction: an intentional range is re-picked in one click,
        // while a poisoned one keeps silently hiding new sessions.
        filters.date = "";
        filters.dateFrom = "";
        filters.dateTo = "";
        saveFilters(filters);
        return { filters, windowDays: null };
      }
      if (validWindowDays(windowDays)) {
        // Rolling intent survives restarts; its bounds are recomputed
        // against the current date so the window keeps rolling forward.
        const range = rollingRange(windowDays);
        filters.date = "";
        filters.dateFrom = range.from;
        filters.dateTo = range.to;
        return { filters, windowDays };
      }
      return { filters, windowDays: null };
    }
  } catch {
    // Corrupted localStorage — fall back to defaults.
  }
  return { filters: defaultFilters(), windowDays: null };
}

export function saveFilters(f: Filters, windowDays: number | null = null): void {
  // Rolling bounds are persisted as intent (windowDays) and rematerialized
  // on load; the materialized dates themselves are session-scoped. Storing
  // them verbatim would pin the window to the day it was saved, silently
  // hiding newer sessions (#1086).
  const toSave = windowDays !== null ? { ...f, date: "", dateFrom: "", dateTo: "", windowDays } : f;
  try {
    localStorage.setItem(
      SESSION_FILTERS_KEY,
      JSON.stringify({ ...toSave, version: SESSION_FILTERS_VERSION }),
    );
  } catch {
    // localStorage full or unavailable — silently skip.
  }
}

/** Serialize a Filters object into URL query params.
 *  Default-valued fields are omitted so the URL stays clean. */
export function filtersToParams(f: Filters): Record<string, string> {
  const p: Record<string, string> = {};
  if (f.project) p["project"] = f.project;
  if (f.machine) p["machine"] = f.machine;
  if (f.agent) p["agent"] = f.agent;
  if (f.termination) p["termination"] = f.termination;
  if (f.date) p["date"] = f.date;
  if (f.dateFrom) p["date_from"] = f.dateFrom;
  if (f.dateTo) p["date_to"] = f.dateTo;
  if (f.recentlyActive) p["active_since"] = "true";
  if (f.hideUnknownProject) p["exclude_project"] = "unknown";
  if (f.minMessages > 0) p["min_messages"] = String(f.minMessages);
  if (f.maxMessages > 0) p["max_messages"] = String(f.maxMessages);
  if (f.minUserMessages > 0) {
    p["min_user_messages"] = String(f.minUserMessages);
  }
  if (!f.includeOneShot) p["include_one_shot"] = "false";
  if (f.includeAutomated || f.automatedFilterSet)
    p["include_automated"] = String(f.includeAutomated);
  const labels = joinLabelFilterParam(f.labels);
  if (labels) p[SESSION_LABEL_PARAM] = labels;
  const pr = f.pr.trim();
  if (pr) p[SESSION_PR_PARAM] = pr;
  return p;
}

export function hasDateFilters(f: Filters): boolean {
  return !!(f.date || f.dateFrom || f.dateTo);
}

export function splitExcludeProjectParam(raw: string | undefined): {
  hideUnknownProject: boolean;
  usageExcludedProjects: string;
} {
  const projects: string[] = [];
  const seen = new Set<string>();
  let hideUnknownProject = false;
  for (const value of (raw ?? "").split(",")) {
    const trimmed = value.trim();
    if (!trimmed) continue;
    if (trimmed === "unknown") {
      hideUnknownProject = true;
      continue;
    }
    if (seen.has(trimmed)) continue;
    seen.add(trimmed);
    projects.push(trimmed);
  }
  return {
    hideUnknownProject,
    usageExcludedProjects: projects.join(","),
  };
}

/** Parse URL query params into a typed Filters object.
 *  Unknown/missing params fall back to defaults. */
export function parseFiltersFromParams(params: Record<string, string>): Filters {
  const minMsgs = parseInt(params["min_messages"] ?? "", 10);
  const maxMsgs = parseInt(params["max_messages"] ?? "", 10);
  const minUserMsgs = parseInt(params["min_user_messages"] ?? "", 10);

  const { hideUnknownProject: hideUnknown } = splitExcludeProjectParam(params["exclude_project"]);
  let project = params["project"] ?? "";
  if (hideUnknown && project === "unknown") {
    project = "";
  }

  const oneShotParam = params["include_one_shot"];
  const includeOneShot = oneShotParam === undefined ? true : oneShotParam === "true";

  return {
    project,
    machine: params["machine"] ?? "",
    agent: params["agent"] ?? "",
    termination: params["termination"] ?? "",
    date: params["date"] ?? "",
    dateFrom: params["date_from"] ?? "",
    dateTo: params["date_to"] ?? "",
    recentlyActive: params["active_since"] === "true",
    hideUnknownProject: hideUnknown,
    minMessages: Number.isFinite(minMsgs) ? minMsgs : 0,
    maxMessages: Number.isFinite(maxMsgs) ? maxMsgs : 0,
    minUserMessages: Number.isFinite(minUserMsgs) ? minUserMsgs : 0,
    includeOneShot,
    includeAutomated: params["include_automated"] === "true",
    automatedFilterSet: params["include_automated"] !== undefined,
    labels: splitLabelFilterParam(params[SESSION_LABEL_PARAM]),
    pr: (params[SESSION_PR_PARAM] ?? "").trim(),
  };
}
