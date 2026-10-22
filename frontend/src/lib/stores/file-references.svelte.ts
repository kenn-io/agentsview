import { getApiV1SessionsByIdFileReferences } from "../api/generated/sessions/sessions.js";
import type { ServiceFileReferencesResult } from "../api/generated/index.js";
import { LatestRead } from "../utils/latest-read.js";

export class FileReferencesStore {
  enabled = $state(false);
  result = $state<ServiceFileReferencesResult>();
  status = $state<"idle" | "loading" | "ok" | "failed">("idle");
  private sessionId = $state<string | null>(null);
  private revision: string | null = null;
  private read = new LatestRead();
  private pending: Promise<void> = Promise.resolve();

  track(sessionId: string | null, revision: string | null): Promise<void> {
    if (!this.enabled) return Promise.resolve();
    if (this.sessionId === sessionId && this.revision === revision) return this.pending;
    this.result = undefined;
    this.sessionId = sessionId;
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

  refresh(): Promise<void> {
    const signal = this.read.begin();
    if (!this.enabled || !this.sessionId) {
      this.status = "idle";
      this.result = undefined;
      return Promise.resolve();
    }
    this.status = "loading";
    this.pending = getApiV1SessionsByIdFileReferences({ id: this.sessionId }, { signal })
      .then((result) => {
        if (!this.read.isCurrent(signal)) return;
        this.result = result;
        this.status = "ok";
      })
      .catch(() => {
        if (this.read.isCurrent(signal)) this.status = "failed";
      });
    return this.pending;
  }

  private forOrdinal(sessionId: string, ordinal: number) {
    return this.enabled && sessionId === this.sessionId && this.result?.integration === "enabled"
      ? this.result.occurrences.filter((item) => item.message_ordinal === ordinal)
      : [];
  }

  forMessage(sessionId: string, ordinal: number) {
    return this.forOrdinal(sessionId, ordinal).filter((item) => item.tool_use_id === null);
  }

  forTool(sessionId: string, ordinal: number, toolUseId: string, eventIndex: number | null) {
    return this.forOrdinal(sessionId, ordinal).filter(
      (item) => item.tool_use_id === toolUseId && item.event_index === eventIndex,
    );
  }
}

export const fileReferences = new FileReferencesStore();
