import { getApiV1SessionsByIdFileReferences } from "../api/generated/sessions/sessions.js";
import type {
  ServiceFileReferenceOccurrence,
  ServiceFileReferencesResult,
} from "../api/generated/index.js";
import { ApiError } from "../api/runtime.js";
import { LatestRead } from "../utils/latest-read.js";

// A live session gets a new revision on every sync. Each lookup rereads the
// whole transcript and asks Docbank about every digest, so refreshes of a
// session that already shows matches are spaced at least this far apart.
export const MIN_REFRESH_INTERVAL_MS = 15_000;

export class FileReferencesStore {
  enabled = $state(false);
  result = $state<ServiceFileReferencesResult>();
  status = $state<"idle" | "loading" | "ok" | "failed">("idle");
  private sessionId = $state<string | null>(null);
  private revision: string | null = null;
  private read = new LatestRead();
  private pending: Promise<void> = Promise.resolve();
  private lastRefresh = 0;
  private timer: ReturnType<typeof setTimeout> | null = null;

  track(id: string | null, revision: string | null): Promise<void> {
    if (!this.enabled) return Promise.resolve();
    if (this.sessionId === id && this.revision === revision) return this.pending;
    if (this.sessionId !== id || !revision || !this.result) {
      this.reset();
      [this.sessionId, this.revision] = [id, revision];
      return revision ? this.refresh() : this.pending;
    }
    // Same session, new revision: keep the current matches on screen and
    // refresh no more often than MIN_REFRESH_INTERVAL_MS.
    this.revision = revision;
    const wait = this.lastRefresh + MIN_REFRESH_INTERVAL_MS - Date.now();
    if (wait <= 0) return this.refresh();
    this.timer ??= setTimeout(() => void this.refresh(), wait);
    return this.pending;
  }

  reset(): void {
    this.cancelTimer();
    this.lastRefresh = 0;
    this.read.cancel();
    this.sessionId = null;
    this.revision = null;
    this.result = undefined;
    this.status = "idle";
    this.pending = Promise.resolve();
  }

  refresh(retryStale = true): Promise<void> {
    this.cancelTimer();
    this.lastRefresh = Date.now();
    const signal = this.read.begin();
    if (!this.enabled || !this.sessionId) {
      this.status = "idle";
      this.result = undefined;
      return Promise.resolve();
    }
    this.status = "loading";
    this.pending = getApiV1SessionsByIdFileReferences({ id: this.sessionId }, { signal })
      .then((result) => this.accept(result, signal, retryStale))
      .catch((error) => this.reject(error, signal, retryStale));
    return this.pending;
  }

  private cancelTimer(): void {
    if (this.timer !== null) clearTimeout(this.timer);
    this.timer = null;
  }

  private reject(error: unknown, signal: AbortSignal, retryStale: boolean): void | Promise<void> {
    if (!this.read.isCurrent(signal)) return;
    if (error instanceof ApiError && error.code === "source_changed") {
      return this.stale(retryStale);
    }
    this.status = "failed";
  }

  private accept(
    result: ServiceFileReferencesResult,
    signal: AbortSignal,
    retryStale: boolean,
  ): void | Promise<void> {
    if (!this.read.isCurrent(signal)) return;
    if (!this.revision || result.transcript_revision !== this.revision) {
      return this.stale(retryStale);
    }
    this.result = result;
    this.status = "ok";
  }

  private stale(retryStale: boolean): void | Promise<void> {
    this.status = "failed";
    return retryStale ? this.refresh(false) : undefined;
  }

  private visibleResult(sessionId: string) {
    const result = sessionId === this.sessionId ? this.result : undefined;
    return this.enabled && result?.integration === "enabled" ? result : undefined;
  }

  private forOrdinal(sessionId: string, ordinal: number) {
    const result = this.visibleResult(sessionId);
    return result ? result.occurrences.filter((item) => item.message_ordinal === ordinal) : [];
  }

  forMessage(sessionId: string, ordinal: number) {
    return this.forOrdinal(sessionId, ordinal).filter((item) => item.tool_use_id === null);
  }

  forTool(id: string, ordinal: number, toolID: string, event: number | null, index = 0) {
    return this.forOrdinal(id, ordinal).filter((item) => matchesTool(item, toolID, event, index));
  }
}

function matchesTool(
  item: ServiceFileReferenceOccurrence,
  id: string,
  event: number | null,
  index: number,
) {
  return (
    item.tool_use_id === id &&
    item.event_index === event &&
    (id !== "" || item.call_index === index)
  );
}

export const fileReferences = new FileReferencesStore();
