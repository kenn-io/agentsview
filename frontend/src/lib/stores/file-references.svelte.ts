import { SvelteMap } from "svelte/reactivity";
import { getApiV1SessionsByIdFileReferences } from "../api/generated/sessions/sessions.js";
import type { ServiceFileReferencesResult } from "../api/generated/index.js";

type Lookup = { loading: boolean; result?: ServiceFileReferencesResult; failed?: boolean };

export class FileReferencesStore {
  entries = new SvelteMap<string, Lookup>();
  private pending = new Map<string, Promise<void>>();

  load(id: string): Promise<void> {
    if (this.entries.has(id)) return this.pending.get(id) ?? Promise.resolve();
    return this.refresh(id);
  }

  refresh(id: string): Promise<void> {
    const existing = this.pending.get(id);
    if (existing) return existing;
    this.entries.set(id, { loading: true });
    const request = getApiV1SessionsByIdFileReferences({ id })
      .then((result) => { this.entries.set(id, { loading: false, result }); })
      .catch(() => { this.entries.set(id, { loading: false, failed: true }); })
      .finally(() => { this.pending.delete(id); });
    this.pending.set(id, request);
    return request;
  }
}

export const fileReferences = new FileReferencesStore();
