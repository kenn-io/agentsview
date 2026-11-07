import { m } from "../i18n/index.js";
import { FrictionService, KataService } from "../api/generated/index";
import type {
  FrictionDigestItem,
  FrictionDigestResponse,
  FrictionPatternItem,
} from "../api/generated/index.js";
import { isAbortError, isNotFoundError } from "../api/runtime.js";
import { LatestRead } from "../utils/latest-read.js";
import { perf } from "./perf.svelte.js";

type ReadStatus = "ok" | "error" | "aborted";

/** The fields of a pattern an issue action changes. */
type IssueChange = Partial<FrictionPatternItem>;

function withIssueChanges(
  digest: FrictionDigestResponse,
  changes: { fingerprint: string; change: IssueChange }[],
): FrictionDigestResponse {
  if (changes.length === 0 || !digest.patterns) return digest;
  return {
    ...digest,
    patterns: digest.patterns.map((p) =>
      changes.reduce(
        (acc, c) => (c.fingerprint === p.fingerprint ? { ...acc, ...c.change } : acc),
        p,
      ),
    ),
  };
}

function errorMessage(e: unknown): string {
  return e instanceof Error && e.message ? e.message : m.friction_load_error();
}

function newestFirst(a: FrictionDigestItem, b: FrictionDigestItem): number {
  if (a.date < b.date) return 1;
  if (a.date > b.date) return -1;
  return 0;
}

class FrictionStore {
  dates: FrictionDigestItem[] = $state([]);
  selectedDate: string | null = $state(null);
  digest: FrictionDigestResponse | null = $state(null);
  markdown: string | null = $state(null);
  loading = $state({ dates: false, digest: false, markdown: false });
  errors = $state<{
    dates: string | null;
    digest: string | null;
    markdown: string | null;
    build: string | null;
  }>({ dates: null, digest: null, markdown: null, build: null });
  building: boolean = $state(false);
  lastBuildWritten: number | null = $state(null);
  lastUpdatedAt: number | null = $state(null);
  lastQueryDurationMs: number | null = $state(null);
  /** Counts list loads that succeeded, from any caller, including Build now. */
  listLoads = $state(0);
  /** Whether this server's Kata connection is ready to file; checked once per
   *  visit by checkKata, since kata_available reports only the last check. */
  kataReady: boolean = $state(false);
  /** Fingerprints with a Kata action in flight, and the last error per pattern. */
  issueBusy = $state<Record<string, boolean>>({});
  issueErrors = $state<Record<string, string>>({});
  #datesRead = new LatestRead();
  #digestRead = new LatestRead();
  #markdownRead = new LatestRead();
  /** Issue actions that succeeded, numbered, so a digest read that started
   *  before one can reapply it to its older response. */
  #issueChanges: { seq: number; fingerprint: string; change: IssueChange }[] = [];
  #issueSeq = 0;

  get latestDate(): string | null {
    return this.dates[0]?.date ?? null;
  }

  #selectedIndex(): number {
    return this.dates.findIndex((d) => d.date === this.selectedDate);
  }

  get olderDate(): string | null {
    const i = this.#selectedIndex();
    return i >= 0 ? (this.dates[i + 1]?.date ?? null) : null;
  }

  get newerDate(): string | null {
    const i = this.#selectedIndex();
    return i > 0 ? this.dates[i - 1]!.date : null;
  }

  async load(requestedDate: string | null = null): Promise<void> {
    const signal = this.#datesRead.begin();
    const selectedAtStart = this.selectedDate;
    const started = performance.now();
    let status: ReadStatus = "ok";
    this.loading.dates = true;
    this.errors.dates = null;
    try {
      const res = await FrictionService.getApiV1FrictionDigests(undefined, { signal });
      if (!this.#datesRead.isCurrent(signal)) {
        status = "aborted";
        return;
      }
      this.dates = [...(res.digests ?? [])].sort(newestFirst);
      // A date picked while the list was loading wins over the request.
      const wanted = this.selectedDate !== selectedAtStart ? this.selectedDate : requestedDate;
      const target = wanted && this.dates.some((d) => d.date === wanted) ? wanted : this.latestDate;
      if (target === null) {
        this.#digestRead.cancel();
        this.selectedDate = null;
        this.digest = null;
        this.markdown = null;
        this.lastUpdatedAt = Date.now();
      } else {
        await this.selectDate(target);
      }
      this.listLoads += 1;
      this.lastQueryDurationMs = performance.now() - started;
    } catch (e) {
      if (isAbortError(e) || !this.#datesRead.isCurrent(signal)) {
        status = "aborted";
        return;
      }
      status = "error";
      this.errors.dates = errorMessage(e);
    } finally {
      perf.recordPanel({
        route: "friction",
        name: "digests",
        durationMs: performance.now() - started,
        status,
      });
      if (this.#datesRead.finish(signal)) this.loading.dates = false;
    }
  }

  async selectDate(date: string): Promise<void> {
    const signal = this.#digestRead.begin();
    const changesSince = this.#issueSeq;
    this.#markdownRead.cancel();
    this.selectedDate = date;
    this.markdown = null;
    this.errors.markdown = null;
    this.loading.markdown = false;
    this.loading.digest = true;
    this.errors.digest = null;
    try {
      const digest = await FrictionService.getApiV1FrictionDigestsByDate({ date }, { signal });
      if (!this.#digestRead.isCurrent(signal)) return;
      // Markdown read for the digest this one replaces must not stay on screen.
      this.#markdownRead.cancel();
      this.markdown = null;
      this.loading.markdown = false;
      // Actions that finished while this read was in flight are newer than
      // its response. Later reads start after every recorded action.
      const missed = this.#issueChanges.filter((c) => c.seq > changesSince);
      this.#issueChanges = [];
      this.digest = withIssueChanges(digest, missed);
      this.lastUpdatedAt = Date.now();
    } catch (e) {
      if (isAbortError(e) || !this.#digestRead.isCurrent(signal)) return;
      this.digest = null;
      this.errors.digest = isNotFoundError(e)
        ? m.friction_digest_missing({ date })
        : errorMessage(e);
    } finally {
      if (this.#digestRead.finish(signal)) this.loading.digest = false;
    }
  }

  // loadMarkdown reads the Markdown of the digest on screen, which can lag
  // selectedDate while a newly selected digest loads.
  async loadMarkdown(date: string): Promise<void> {
    const signal = this.#markdownRead.begin();
    this.loading.markdown = true;
    this.errors.markdown = null;
    try {
      const res = await FrictionService.getApiV1FrictionDigestsByDateMd({ date }, { signal });
      const text = await res.text();
      if (!this.#markdownRead.isCurrent(signal) || this.digest?.date !== date) return;
      this.markdown = text;
    } catch (e) {
      if (isAbortError(e) || !this.#markdownRead.isCurrent(signal)) return;
      this.errors.markdown = m.friction_markdown_error();
    } finally {
      if (this.#markdownRead.finish(signal)) this.loading.markdown = false;
    }
  }

  async buildNow(): Promise<void> {
    if (this.building) return;
    this.building = true;
    this.errors.build = null;
    this.lastBuildWritten = null;
    try {
      const res = await FrictionService.postApiV1FrictionRun({});
      const written = (res.reports ?? []).filter((r) => r.written).length;
      this.lastBuildWritten = written;
      await this.load(written > 0 ? null : this.selectedDate);
    } catch (e) {
      this.errors.build = errorMessage(e);
    } finally {
      this.building = false;
    }
  }

  async checkKata(): Promise<void> {
    try {
      const status = await KataService.getApiV1KataStatus();
      this.kataReady = status.state === "ready";
    } catch {
      this.kataReady = false;
    }
  }

  fileIssue(fingerprint: string): Promise<void> {
    return this.#issueAction(fingerprint, async () => {
      const res = await FrictionService.postApiV1FrictionPatternsByFingerprintIssue(
        { fingerprint },
        {},
      );
      return { issue: res.issue ?? undefined, filing: undefined };
    });
  }

  linkIssue(fingerprint: string, issue: string): Promise<void> {
    return this.#issueAction(fingerprint, async () => {
      const res = await FrictionService.putApiV1FrictionPatternsByFingerprintIssue(
        { fingerprint },
        { issue },
      );
      return { issue: res.issue, filing: undefined };
    });
  }

  unlinkIssue(fingerprint: string): Promise<void> {
    return this.#issueAction(fingerprint, async () => {
      await FrictionService.deleteApiV1FrictionPatternsByFingerprintIssue({ fingerprint });
      // The server pauses automatic filing for a pattern a person unlinked.
      return { issue: undefined, filing: { state: "paused", attempts: 0 } };
    });
  }

  // The server owns filing; on success the digest on screen shows the new
  // link and filing state without reloading, and a failure stays with its
  // pattern.
  async #issueAction(fingerprint: string, act: () => Promise<IssueChange>): Promise<void> {
    if (this.issueBusy[fingerprint]) return;
    this.issueBusy[fingerprint] = true;
    delete this.issueErrors[fingerprint];
    try {
      const change = await act();
      const recorded = { seq: ++this.#issueSeq, fingerprint, change };
      if (this.loading.digest) this.#issueChanges.push(recorded);
      if (this.digest) this.digest = withIssueChanges(this.digest, [recorded]);
    } catch (e) {
      this.issueErrors[fingerprint] = errorMessage(e);
    } finally {
      delete this.issueBusy[fingerprint];
    }
  }

  cancelInFlightReads(): void {
    this.#datesRead.cancel();
    this.#digestRead.cancel();
    this.#markdownRead.cancel();
    this.loading.dates = false;
    this.loading.digest = false;
    this.loading.markdown = false;
  }

  reset(): void {
    this.cancelInFlightReads();
    this.dates = [];
    this.selectedDate = null;
    this.digest = null;
    this.markdown = null;
    this.listLoads = 0;
    this.errors.dates = null;
    this.errors.digest = null;
    this.errors.markdown = null;
    this.errors.build = null;
    this.building = false;
    this.lastBuildWritten = null;
    this.lastUpdatedAt = null;
    this.lastQueryDurationMs = null;
    this.kataReady = false;
    this.issueBusy = {};
    this.issueErrors = {};
    this.#issueChanges = [];
  }
}

export const friction = new FrictionStore();
