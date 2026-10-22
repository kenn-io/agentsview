import { SvelteMap } from "svelte/reactivity";
import { getApiV1SessionsByIdFileReferences } from "../api/generated/sessions/sessions.js";
import type { ServiceFileReferencesResult } from "../api/generated/index.js";

type Lookup = { loading: boolean; result?: ServiceFileReferencesResult; failed?: boolean };

export class FileReferencesStore {
  enabled = $state(false);
  entries = new SvelteMap<string, Lookup>();
  private pending = new Map<string, Promise<void>>();
  private revisions = new Map<string, string | null>();

  reopen(id: string) {
    this.entries.delete(id);
    this.pending.delete(id);
    this.revisions.delete(id);
  }

  load(id: string, revision: string | null = null): Promise<void> {
    if (!this.enabled) return Promise.resolve();
    if (this.revisions.has(id) && this.revisions.get(id) !== revision) this.reopen(id);
    this.revisions.set(id, revision);
    if (this.entries.has(id)) return this.pending.get(id) ?? Promise.resolve();
    return this.refresh(id);
  }

  refresh(id: string): Promise<void> {
    if (!this.enabled) return Promise.resolve();
    const existing = this.pending.get(id);
    if (existing) return existing;
    const previous = this.entries.get(id)?.result;
    this.entries.set(id, { loading: true, result: previous });
    const request = getApiV1SessionsByIdFileReferences({ id })
      .then((result) => {
        if (this.pending.get(id) === request) this.entries.set(id, { loading: false, result });
      })
      .catch(() => {
        if (this.pending.get(id) === request)
          this.entries.set(id, { loading: false, failed: true, result: previous });
      })
      .finally(() => {
        if (this.pending.get(id) === request) this.pending.delete(id);
      });
    this.pending.set(id, request);
    return request;
  }
}

export const fileReferences = new FileReferencesStore();
