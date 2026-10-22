import { getApiV1SessionsByIdFileReferences } from "../api/generated/sessions/sessions.js";
import type {
  ServiceFileReferenceOccurrence,
  ServiceFileReferencesResult,
} from "../api/generated/index.js";
import { ApiError } from "../api/runtime.js";
import { LatestRead } from "../utils/latest-read.js";

export class FileReferencesStore {
  enabled = $state(false);
  result = $state<ServiceFileReferencesResult>();
  status = $state<"idle" | "loading" | "ok" | "failed">("idle");
  private sessionId = $state<string | null>(null);
  private revision: string | null = null;
  private read = new LatestRead();
  private pending: Promise<void> = Promise.resolve();

  track(id: string | null, revision: string | null): Promise<void> {
    if (!this.enabled) return Promise.resolve();
    if (this.sessionId === id && this.revision === revision) return this.pending;
    this.result = undefined;
    this.sessionId = id;
    this.revision = revision;
    return this.refresh();
  }

  reset(): void {
    this.read.cancel();
    this.sessionId = null;
    this.revision = null;
    this.result = undefined;
    this.status = "idle";
    this.pending = Promise.resolve();
  }

  refresh(retryStale = true): Promise<void> {
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

  private reject(error: unknown, signal: AbortSignal, retryStale: boolean): void | Promise<void> {
    if (!this.read.isCurrent(signal)) return;
    if (error instanceof ApiError && error.code === "source_changed") {
      return retryStale ? this.refresh(false) : undefined;
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
      return retryStale ? this.refresh(false) : undefined;
    }
    this.result = result;
    this.status = "ok";
  }

  private visibleResult(sessionId: string) {
    return this.enabled && sessionId === this.sessionId && this.result?.integration === "enabled"
      ? this.result
      : undefined;
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
