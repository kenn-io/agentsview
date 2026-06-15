import {
  TrendsService,
} from "../api/generated/index";
import type { TrendsTermsResponse } from "../api/types.js";
import { callGenerated } from "../api/runtime.js";
import { daysAgo, today } from "../utils/dates.js";
import { perf } from "./perf.svelte.js";

type TrendsTermsParams = Parameters<
  typeof TrendsService.getApiV1TrendsTerms
>[0];
export type TrendsGranularity = NonNullable<
  TrendsTermsParams["granularity"]
>;

const DEFAULT_TERMS =
  "load bearing | load-bearing\nseam\nblast radius";

class TrendsStore {
  from: string = $state(daysAgo(365));
  to: string = $state(today());
  granularity: TrendsGranularity = $state("week");
  normalized: boolean = $state(false);
  termText: string = $state(DEFAULT_TERMS);
  response: TrendsTermsResponse | null = $state(null);
  loading = $state({ terms: false });
  errors = $state<{ terms: string | null }>({ terms: null });
  private version = 0;

  get timezone(): string {
    return Intl.DateTimeFormat().resolvedOptions().timeZone;
  }

  get terms(): string[] {
    return this.termText
      .split("\n")
      .map((s) => s.trim())
      .filter(Boolean);
  }

  private params(): TrendsTermsParams {
    return {
      from: this.from,
      to: this.to,
      timezone: this.timezone,
      granularity: this.granularity,
      term: this.terms,
    };
  }

  async fetchTerms(): Promise<void> {
    const v = ++this.version;
    const isFirstLoad = this.response === null;
    this.loading.terms = true;
    this.errors.terms = null;
    const started = performance.now();
    let status: "ok" | "error" = "ok";
    try {
      const data = await callGenerated(() =>
        TrendsService.getApiV1TrendsTerms(this.params()),
      ) as unknown as TrendsTermsResponse;
      if (this.version === v) {
        this.response = data;
        this.errors.terms = null;
      }
    } catch (e) {
      status = "error";
      if (this.version === v) {
        this.errors.terms =
          e instanceof Error ? e.message : "Failed to load";
        if (isFirstLoad) {
          this.response = null;
        } else {
          console.warn("trends.terms refetch failed:", e);
        }
      }
    } finally {
      perf.recordPanel({
        route: "trends",
        name: "terms",
        durationMs: performance.now() - started,
        status,
      });
      if (this.version === v) {
        this.loading.terms = false;
      }
    }
  }

  async setDateRange(from: string, to: string): Promise<void> {
    this.from = from;
    this.to = to;
    await this.fetchTerms();
  }

  async setGranularity(g: TrendsGranularity): Promise<void> {
    this.granularity = g;
    await this.fetchTerms();
  }

  async resetTerms(): Promise<void> {
    this.termText = DEFAULT_TERMS;
    await this.fetchTerms();
  }
}

export const trends = new TrendsStore();
