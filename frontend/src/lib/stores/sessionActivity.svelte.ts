import { SessionsService } from "../api/generated/index";
import {
  callGenerated,
  configureGeneratedClient,
  isAbortError,
} from "../api/runtime.js";
import type {
  SessionActivityBucket,
  SessionActivityResponse,
} from "../api/types/session-activity.js";
import { LatestRead } from "../utils/latest-read.js";

export function findActiveBucketIndex(
  buckets: SessionActivityBucket[],
  timestamp: string | null,
): number | null {
  if (!timestamp || buckets.length === 0) return null;
  const ts = new Date(timestamp).getTime();
  for (let i = 0; i < buckets.length; i++) {
    const start = new Date(buckets[i]!.start_time).getTime();
    const end = new Date(buckets[i]!.end_time).getTime();
    if (ts >= start && ts < end) return i;
  }
  return null;
}

class SessionActivityStore {
  buckets: SessionActivityBucket[] = $state([]);
  intervalSeconds: number = $state(0);
  totalMessages: number = $state(0);
  loading: boolean = $state(false);
  loaded: boolean = $state(false);
  error: string | null = $state(null);
  firstVisibleTimestamp: string | null = $state(null);

  private cachedSessionId: string | null = null;
  private loadVersion = 0;
  private activityRead = new LatestRead();

  /** True when buckets are loaded for the given session. */
  isForSession(sessionId: string): boolean {
    return this.cachedSessionId === sessionId;
  }

  get activeBucketIndex(): number | null {
    return findActiveBucketIndex(
      this.buckets,
      this.firstVisibleTimestamp,
    );
  }

  async load(sessionId: string) {
    if (
      this.cachedSessionId === sessionId &&
      this.loaded
    ) {
      return;
    }
    // Clear stale data from a different session before
    // fetching so the component never renders old buckets.
    if (this.cachedSessionId !== sessionId) {
      this.buckets = [];
      this.loaded = false;
    }
    const version = ++this.loadVersion;
    const signal = this.activityRead.begin();
    this.loading = true;
    this.error = null;
    this.firstVisibleTimestamp = null;
    try {
      configureGeneratedClient();
      const resp = await callGenerated(
        () => SessionsService.getApiV1SessionsIdActivity({ id: sessionId }),
        signal,
      ) as unknown as SessionActivityResponse;
      // Ignore stale responses from previous sessions.
      if (
        version !== this.loadVersion ||
        !this.activityRead.isCurrent(signal)
      ) return;
      this.buckets = resp.buckets;
      this.intervalSeconds = resp.interval_seconds;
      this.totalMessages = resp.total_messages;
      this.cachedSessionId = sessionId;
      this.loaded = true;
    } catch (e) {
      if (
        isAbortError(e) ||
        version !== this.loadVersion ||
        !this.activityRead.isCurrent(signal)
      ) return;
      this.error =
        e instanceof Error
          ? e.message
          : "Failed to load activity";
      this.buckets = [];
      this.cachedSessionId = sessionId;
      this.loaded = true;
    } finally {
      if (this.activityRead.finish(signal)) {
        this.loading = false;
      }
    }
  }

  reload(sessionId: string) {
    this.loaded = false;
    return this.load(sessionId);
  }

  /** Mark cached data as stale so the next load() refetches.
   *  Also discards any in-flight response. */
  invalidate() {
    this.activityRead.cancel();
    this.loadVersion++;
    this.cachedSessionId = null;
    this.loaded = false;
  }

  clear() {
    this.activityRead.cancel();
    this.loadVersion++;
    this.buckets = [];
    this.intervalSeconds = 0;
    this.totalMessages = 0;
    this.loading = false;
    this.loaded = false;
    this.error = null;
    this.cachedSessionId = null;
    this.firstVisibleTimestamp = null;
  }

  cancelInFlight(): void {
    this.loadVersion++;
    this.activityRead.cancel();
    this.loading = false;
  }
}

export const sessionActivity = new SessionActivityStore();
