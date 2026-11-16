import { untrack } from "svelte";
import {
  AnalyticsService,
  type DbSignalSessionExample as SignalSessionExample,
} from "../../api/generated/index.js";
import { isAbortError } from "../../api/runtime.js";
import { m } from "../../i18n/index.js";
import { analytics } from "../../stores/analytics.svelte.js";
import { LatestRead } from "../../utils/latest-read.js";

type AnalyticsParams = NonNullable<Parameters<typeof AnalyticsService.getApiV1AnalyticsSignals>[0]>;

function signalEvidenceKey(signal: string, params: AnalyticsParams): string {
  const entries = Object.entries(params)
    .filter(([, value]) => value !== undefined && value !== "")
    .sort(([a], [b]) => a.localeCompare(b));
  return JSON.stringify({ signal, params: Object.fromEntries(entries) });
}

/**
 * The sessions behind one quality signal, shown when someone picks a signal
 * on a pattern card. The examples follow the Quality filters: a filter
 * change reloads them for the open signal. Create it while a component
 * initializes; its read stops when that component is destroyed.
 */
export class SignalEvidence {
  selectedSignalId: string | null = $state(null);
  examples: SignalSessionExample[] = $state([]);
  loading = $state(false);
  error: string | null = $state(null);

  private filterKey: string | null = $state(null);
  private request = 0;
  private readonly read = new LatestRead();

  constructor() {
    $effect(() => {
      const signal = this.selectedSignalId;
      if (!signal) return;
      const params = analytics.signalEvidenceParams();
      const nextKey = signalEvidenceKey(signal, params);
      if (this.filterKey === nextKey) return;
      this.examples = [];
      this.error = null;
      untrack(() => void this.open(signal));
    });
    $effect(() => () => this.read.cancel());
  }

  close(): void {
    this.read.cancel();
    this.selectedSignalId = null;
    this.examples = [];
    this.loading = false;
    this.error = null;
    this.filterKey = null;
  }

  async open(signal: string): Promise<void> {
    const params = analytics.signalEvidenceParams();
    const requestKey = signalEvidenceKey(signal, params);
    const request = ++this.request;
    const requestSignal = this.read.begin();
    this.selectedSignalId = signal;
    this.filterKey = requestKey;
    this.loading = true;
    this.error = null;
    const current = () =>
      this.selectedSignalId === signal && this.filterKey === requestKey && this.request === request;
    try {
      const response = await AnalyticsService.getApiV1AnalyticsSignalSessions(
        {
          ...params,
          signal,
          limit: 8,
        },
        { signal: requestSignal },
      );
      if (this.read.isCurrent(requestSignal) && current()) {
        this.examples = response.sessions ?? [];
      }
    } catch (err) {
      if (isAbortError(err) || !this.read.isCurrent(requestSignal)) return;
      if (current()) {
        this.examples = [];
        this.error = err instanceof Error ? err.message : m.insights_page_could_not_load_examples();
      }
    } finally {
      if (this.read.finish(requestSignal) && current()) {
        this.loading = false;
      }
    }
  }
}
