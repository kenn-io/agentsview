import { rollingRange } from "../utils/dates.js";
import type { PanelDateState } from "./yokedDates.svelte.js";

export type AnalyticsDatePage = "sessions" | "insights";

export interface RetainedAnalyticsPageDate {
  state: PanelDateState;
  explicitDateIntent: boolean;
}

const DEFAULT_WINDOW_DAYS = 365;

// Sessions and Insights share analytics results and filters, but an opted-out
// date selection and its explicit-intent provenance must survive navigation
// without becoming the other page's selection. Rolling ranges are
// rematerialized so retained presets stay fresh.
class AnalyticsPageDatesStore {
  #retained: Partial<
    Record<AnalyticsDatePage, RetainedAnalyticsPageDate>
  > = {};

  retain(
    page: AnalyticsDatePage,
    state: PanelDateState,
    explicitDateIntent = false,
  ): void {
    this.#retained[page] = {
      state: { ...state },
      explicitDateIntent,
    };
  }

  restore(page: AnalyticsDatePage): PanelDateState {
    return this.restoreWithIntent(page).state;
  }

  restoreWithIntent(
    page: AnalyticsDatePage,
  ): RetainedAnalyticsPageDate {
    const retained = this.#retained[page];
    if (retained?.state.mode === "fixed") {
      return {
        state: { ...retained.state },
        explicitDateIntent: retained.explicitDateIntent,
      };
    }

    const windowDays =
      retained?.state.windowDays ?? DEFAULT_WINDOW_DAYS;
    const range = rollingRange(windowDays);
    return {
      state: {
        from: range.from,
        to: range.to,
        mode: "rolling",
        windowDays,
      },
      explicitDateIntent: retained?.explicitDateIntent ?? false,
    };
  }

  clear(): void {
    this.#retained = {};
  }
}

export const analyticsPageDates = new AnalyticsPageDatesStore();
