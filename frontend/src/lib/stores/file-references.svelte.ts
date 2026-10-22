import { getApiV1SessionsByIdFileReferences } from "../api/generated/sessions/sessions.js";
import type { ServiceFileReferencesResult } from "../api/generated/index.js";

export class FileReferencesStore {
  enabled = $state(false);
  result = $state<ServiceFileReferencesResult>();
  status = $state<"idle" | "loading" | "ok" | "failed">("idle");
  private sessionId = $state<string | null>(null);
  private revision: string | null = null;
  private request = 0;
  private pending: Promise<void> = Promise.resolve();

  track(sessionId: string | null, revision: string | null): Promise<void> {
    if (!this.enabled) return Promise.resolve();
    if (this.sessionId === sessionId && this.revision === revision) return this.pending;
    if (this.sessionId !== sessionId) this.result = undefined;
    this.sessionId = sessionId;
    this.revision = revision;
    return this.refresh();
  }

  refresh(): Promise<void> {
    const request = ++this.request;
    if (!this.enabled || !this.sessionId) {
      this.status = "idle";
      this.result = undefined;
      return Promise.resolve();
    }
    this.status = "loading";
    this.pending = getApiV1SessionsByIdFileReferences({ id: this.sessionId })
      .then((result) => {
        if (request !== this.request) return;
        this.result = result;
        this.status = "ok";
      })
      .catch(() => {
        if (request === this.request) this.status = "failed";
      });
    return this.pending;
  }

  forMessage(sessionId: string, ordinal: number) {
    return this.enabled && sessionId === this.sessionId && this.result?.integration === "enabled"
      ? this.result.occurrences.filter(
          (item) => item.message_ordinal === ordinal && item.tool_use_id === null,
        )
      : [];
  }

  forTool(toolUseId: string, eventIndex: number | null) {
    return this.enabled && this.result?.integration === "enabled"
      ? this.result.occurrences.filter(
          (item) => item.tool_use_id === toolUseId && item.event_index === eventIndex,
        )
      : [];
  }
}

export const fileReferences = new FileReferencesStore();
