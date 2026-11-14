import {
  isTreemapLabelScale,
  type TreemapLabelScale,
} from "../components/usage/treemapLabelSize.js";

// Usage page choices remembered in localStorage: chart and attribution
// toggles, and the picker exclusions.

export type GroupBy = "project" | "model" | "agent";
export type TimeSeriesView = "smooth" | "lines" | "bars";
export type AttributionView = "treemap" | "list" | "bars";

export interface Toggles {
  timeSeries: { groupBy: GroupBy; view: TimeSeriesView };
  attribution: { groupBy: GroupBy; view: AttributionView; labelScale: TreemapLabelScale };
}

const TOGGLES_KEY = "usage-toggles";

function defaultToggles(): Toggles {
  return {
    timeSeries: { groupBy: "project", view: "smooth" },
    attribution: { groupBy: "project", view: "treemap", labelScale: "sqrt" },
  };
}

function isGroupBy(value: unknown): value is GroupBy {
  return value === "project" || value === "model" || value === "agent";
}

function isTimeSeriesView(value: unknown): value is TimeSeriesView {
  return value === "smooth" || value === "lines" || value === "bars";
}

export function loadToggles(): Toggles {
  try {
    const raw = localStorage.getItem(TOGGLES_KEY);
    if (raw) {
      const parsed = JSON.parse(raw) as Partial<Toggles>;
      const defaults = defaultToggles();
      // `Project | Model | Agent` selector is shared across usage
      // panels. Migrate legacy split state by choosing one value
      // and applying it to both widgets.
      const sharedGroupBy = isGroupBy(parsed.timeSeries?.groupBy)
        ? parsed.timeSeries.groupBy
        : isGroupBy(parsed.attribution?.groupBy)
          ? parsed.attribution.groupBy
          : defaults.timeSeries.groupBy;
      return {
        timeSeries: {
          groupBy: sharedGroupBy,
          view: isTimeSeriesView(parsed.timeSeries?.view)
            ? parsed.timeSeries.view
            : defaults.timeSeries.view,
        },
        attribution: {
          groupBy: sharedGroupBy,
          view: parsed.attribution?.view ?? defaults.attribution.view,
          labelScale: isTreemapLabelScale(parsed.attribution?.labelScale)
            ? parsed.attribution.labelScale
            : defaults.attribution.labelScale,
        },
      };
    }
  } catch {
    // Corrupted localStorage — fall back to defaults.
  }
  return defaultToggles();
}

export function saveToggles(t: Toggles): void {
  try {
    localStorage.setItem(TOGGLES_KEY, JSON.stringify(t));
  } catch {
    // localStorage full or unavailable — silently skip.
  }
}

const USAGE_FILTERS_KEY = "usage-filters";

export interface UsageFilterState {
  excludedProjects: string;
  excludedProjectKeys?: string;
  excludedAgents: string;
  excludedModels: string;
}

export function loadUsageFilters(): UsageFilterState {
  try {
    const raw = localStorage.getItem(USAGE_FILTERS_KEY);
    if (raw) {
      const saved = JSON.parse(raw) as Partial<UsageFilterState>;
      return {
        excludedProjects: saved.excludedProjects ?? "",
        excludedProjectKeys: "",
        excludedAgents: saved.excludedAgents ?? "",
        excludedModels: saved.excludedModels ?? "",
      };
    }
  } catch {
    // Corrupted localStorage — fall back to defaults.
  }
  return {
    excludedProjects: "",
    excludedProjectKeys: "",
    excludedAgents: "",
    excludedModels: "",
  };
}

export function saveUsageFilters(f: UsageFilterState): void {
  try {
    const data: UsageFilterState = {
      excludedProjects: f.excludedProjects,
      excludedAgents: f.excludedAgents,
      excludedModels: f.excludedModels,
    };
    localStorage.setItem(USAGE_FILTERS_KEY, JSON.stringify(data));
  } catch {
    // localStorage full or unavailable — silently skip.
  }
}
