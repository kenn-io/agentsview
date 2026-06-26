import { parseLocalDate, rollingRange, today } from "../utils/dates.js";

export type YokedDateMode = "fixed" | "rolling";

export interface PanelDateState {
  from: string;
  to: string;
  mode?: YokedDateMode;
  windowDays?: number;
}

export interface YokedDateRange {
  from: string;
  to: string;
  mode: YokedDateMode;
  windowDays?: number;
  updatedAt: number;
}

export interface StoredYokedDates {
  version: 1;
  range: YokedDateRange | null;
}

export const YOKED_DATES_STORAGE_KEY = "yoked-dates";

const STORAGE_VERSION = 1;
const DATE_RE = /^\d{4}-\d{2}-\d{2}$/;
const ACTIVITY_MAX_CUSTOM_RANGE_MS = 365 * 24 * 60 * 60 * 1000;

function getLocalStorage(): Storage | null {
  try {
    return globalThis.localStorage ?? null;
  } catch {
    return null;
  }
}

function isDateString(value: unknown): value is string {
  return typeof value === "string" && DATE_RE.test(value);
}

function dateOnly(value: string | undefined): string | undefined {
  if (!value) return undefined;
  const candidate = value.slice(0, 10);
  return DATE_RE.test(candidate) ? candidate : undefined;
}

function validWindowDays(value: unknown): number | undefined {
  if (
    typeof value !== "number" ||
    !Number.isInteger(value) ||
    value <= 0
  ) {
    return undefined;
  }
  return value;
}

function copyRange(range: YokedDateRange): YokedDateRange {
  return range.windowDays === undefined
    ? {
        from: range.from,
        to: range.to,
        mode: range.mode,
        updatedAt: range.updatedAt,
      }
    : {
        from: range.from,
        to: range.to,
        mode: range.mode,
        windowDays: range.windowDays,
        updatedAt: range.updatedAt,
      };
}

function parseStoredRange(value: unknown): YokedDateRange | null {
  if (typeof value !== "object" || value === null) {
    return null;
  }
  const range = value as Partial<YokedDateRange>;
  if (
    !isDateString(range.from) ||
    !isDateString(range.to) ||
    range.from > range.to ||
    (range.mode !== "fixed" && range.mode !== "rolling") ||
    typeof range.updatedAt !== "number" ||
    !Number.isFinite(range.updatedAt)
  ) {
    return null;
  }

  if (range.mode === "rolling") {
    const windowDays = validWindowDays(range.windowDays);
    if (windowDays === undefined) return null;
    return {
      from: range.from,
      to: range.to,
      mode: "rolling",
      windowDays,
      updatedAt: range.updatedAt,
    };
  }

  return {
    from: range.from,
    to: range.to,
    mode: "fixed",
    updatedAt: range.updatedAt,
  };
}

function parseStored(value: unknown): StoredYokedDates | null {
  if (typeof value !== "object" || value === null) {
    return null;
  }
  const stored = value as Partial<StoredYokedDates>;
  if (stored.version !== STORAGE_VERSION) {
    return null;
  }
  if (stored.range === null) {
    return {
      version: STORAGE_VERSION,
      range: null,
    };
  }
  const range = parseStoredRange(stored.range);
  if (!range) return null;
  return {
    version: STORAGE_VERSION,
    range,
  };
}

export function panelDateState(
  from: string,
  to: string,
  options: { mode?: YokedDateMode; windowDays?: number } = {},
): PanelDateState | null {
  if (!isDateString(from) || !isDateString(to) || from > to) {
    return null;
  }

  const mode = options.mode ?? "fixed";
  if (mode === "rolling") {
    const windowDays = validWindowDays(options.windowDays);
    if (windowDays === undefined) return null;
    return { from, to, mode, windowDays };
  }

  if (mode !== "fixed") return null;
  return { from, to, mode };
}

export function panelStateToRange(
  current: PanelDateState,
  updatedAt: number,
): YokedDateRange | null {
  const state = panelDateState(current.from, current.to, {
    mode: current.mode,
    windowDays: current.windowDays,
  });
  if (!state || !Number.isFinite(updatedAt)) return null;
  return state.mode === "rolling"
    ? {
        from: state.from,
        to: state.to,
        mode: "rolling",
        windowDays: state.windowDays,
        updatedAt,
      }
    : {
        from: state.from,
        to: state.to,
        mode: "fixed",
        updatedAt,
    };
}

export function rangeToPanelDate(
  range: YokedDateRange,
  now: Date = new Date(),
): PanelDateState | null {
  if (range.mode === "rolling") {
    const windowDays = validWindowDays(range.windowDays);
    if (windowDays === undefined) return null;
    const rangeDates = rollingRange(windowDays, now);
    return panelDateState(rangeDates.from, rangeDates.to, {
      mode: "rolling",
      windowDays,
    });
  }

  return panelDateState(range.from, range.to, { mode: "fixed" });
}

export function sessionParamsToPanelDate(
  params: Record<string, string>,
  bounds: { earliest?: string; latest?: string } = {},
): PanelDateState | null {
  if (params["date"]) {
    return panelDateState(params["date"], params["date"], {
      mode: "fixed",
    });
  }
  if (params["date_from"] || params["date_to"]) {
    const from =
      dateOnly(params["date_from"]) ??
      dateOnly(bounds.earliest) ??
      dateOnly(params["date_to"])!;
    const to =
      dateOnly(params["date_to"]) ??
      dateOnly(bounds.latest) ??
      today();
    return panelDateState(from, to, {
      mode: "fixed",
    });
  }
  return null;
}

export function rangeToSessionParams(
  range: YokedDateRange,
): Record<string, string> {
  if (range.mode === "rolling" && range.windowDays) {
    return { window_days: String(range.windowDays) };
  }
  if (range.mode === "fixed" && range.from === range.to) {
    return { date: range.from };
  }
  return {
    date_from: range.from,
    date_to: range.to,
  };
}

export function rangeToActivityParams(
  range: YokedDateRange,
  now: Date = new Date(),
): Record<string, string> {
  const state = rangeToPanelDate(range, now);
  if (!state) return {};
  if (!activityCustomRangeWithinLimit(state.from, state.to)) return {};
  const params: Record<string, string> = {
    preset: "custom",
    from: state.from,
    to: state.to,
  };
  if (state.mode === "rolling" && state.windowDays) {
    params.window_days = String(state.windowDays);
  }
  return params;
}

function activityCustomRangeWithinLimit(from: string, to: string): boolean {
  const start = parseLocalDate(from);
  const inclusiveEnd = parseLocalDate(to);
  if (!start || !inclusiveEnd) return false;

  // Activity sends date-only custom ranges as [from, to + 1 day), and the
  // backend rejects anything wider than 365 * 24h.
  const exclusiveEnd = new Date(inclusiveEnd);
  exclusiveEnd.setDate(exclusiveEnd.getDate() + 1);
  const durationMs = exclusiveEnd.getTime() - start.getTime();
  return durationMs > 0 && durationMs <= ACTIVITY_MAX_CUSTOM_RANGE_MS;
}

export function rangeToInsightParams(
  range: YokedDateRange,
): Record<string, string> {
  const state = rangeToPanelDate(range);
  if (!state) return {};
  const params: Record<string, string> = {
    date_from: state.from,
    date_to: state.to,
  };
  if (state.mode === "rolling" && state.windowDays) {
    params.window_days = String(state.windowDays);
  }
  return params;
}

export class YokedDatesStore {
  range: YokedDateRange | null = $state(null);

  constructor(
    private readonly storage: Storage | null = getLocalStorage(),
    private readonly now: () => number = () => Date.now(),
  ) {
    this.hydrate();
  }

  hydrate(): void {
    if (!this.storage) return;
    try {
      const raw = this.storage.getItem(YOKED_DATES_STORAGE_KEY);
      if (!raw) return;
      const stored = parseStored(JSON.parse(raw));
      if (!stored) return;
      this.range = stored.range ? copyRange(stored.range) : null;
    } catch {
      this.range = null;
    }
  }

  updateFromPanel(current: PanelDateState): void {
    const range = panelStateToRange(current, this.now());
    if (!range) return;
    this.range = range;
    this.persist();
  }

  clear(): void {
    this.range = null;
    this.persist();
  }

  seedForPanel(): YokedDateRange | null {
    if (!this.range) return null;
    return copyRange(this.range);
  }

  private persist(): void {
    if (!this.storage) return;
    const stored: StoredYokedDates = {
      version: STORAGE_VERSION,
      range: this.range ? copyRange(this.range) : null,
    };
    try {
      this.storage.setItem(
        YOKED_DATES_STORAGE_KEY,
        JSON.stringify(stored),
      );
    } catch {
      // Storage can be unavailable or full; current-tab state still works.
    }
  }
}

export const yokedDates = new YokedDatesStore();
