import { SessionsService } from "../api/generated/index.js";
import type { ServiceFileReferenceOccurrence as Occurrence } from "../api/generated/index.js";
import { isAbortError } from "../api/runtime.js";

type Window = {
  first: number;
  last: number;
  offset: number;
  next: number | null;
  retry: boolean;
  retryAfter: string | null;
};
export class SessionFileReferencesStore {
  occurrences = $state(new Map<string, Occurrence>());
  integration = $state("");
  unavailable = $state(false);
  busy = $state(false);
  canCheckRemaining = $state(false);
  private id: string | null = null;
  private binding: string | null = null;
  private generation = 0;
  private controller = new AbortController();
  private windows: Window[] = [];
  private pending: Window[] = [];
  private running: Promise<void> | null = null;
  private requestTail = Promise.resolve();
  constructor(private fetch = SessionsService.getApiV1SessionsByIdFileReferences) {}

  setSession(id: string | null, binding: string | null): void {
    if (this.id === id && this.binding === binding) return;
    this.dispose();
    this.id = id;
    this.binding = binding;
  }
  dispose(): void {
    this.generation++;
    this.controller.abort();
    this.controller = new AbortController();
    this.id = null;
    this.binding = null;
    this.windows = [];
    this.pending = [];
    this.running = null;
    this.requestTail = Promise.resolve();
    this.occurrences = new Map();
    this.integration = "";
    this.unavailable = false;
    this.busy = false;
    this.canCheckRemaining = false;
  }
  loadWindow(first: number, last: number): Promise<void> {
    if (!this.id || !this.binding || first > last || this.integration === "disabled")
      return Promise.resolve();
    let ranges: [number, number][] = [[first, last]];
    for (const window of this.windows) {
      ranges = ranges.flatMap(([start, end]) => {
        if (window.last < start || window.first > end) return [[start, end]];
        const uncovered: [number, number][] = [];
        if (start < window.first) uncovered.push([start, window.first - 1]);
        if (end > window.last) uncovered.push([window.last + 1, end]);
        return uncovered;
      });
    }
    for (const [start, end] of ranges) {
      const overlap = this.pending.find((w) => start <= w.last + 1 && end >= w.first - 1);
      if (overlap) {
        overlap.first = Math.min(start, overlap.first);
        overlap.last = Math.max(end, overlap.last);
      } else
        this.pending.push({
          first: start,
          last: end,
          offset: 0,
          next: null,
          retry: false,
          retryAfter: null,
        });
    }
    return this.drain();
  }
  private drain(): Promise<void> {
    if (this.running) return this.running;
    const generation = this.generation;
    const run = Promise.resolve().then(async () => {
      while (this.pending.length && generation === this.generation) {
        const window = this.pending.shift()!;
        this.windows.push(window);
        await this.request(
          {
            first_ordinal: window.first,
            last_ordinal: window.last,
            offset: window.offset,
            limit: 20,
          },
          window,
        );
      }
    });
    this.running = run.finally(() => {
      if (generation === this.generation) this.running = null;
    });
    return this.running;
  }
  private request(
    params: NonNullable<Parameters<typeof this.fetch>[1]>,
    window?: Window,
    more = false,
  ): Promise<void> {
    const generation = this.generation;
    const task = this.requestTail.then(() =>
      generation === this.generation ? this.performRequest(params, window, more) : undefined,
    );
    this.requestTail = task;
    return task;
  }
  private async performRequest(
    params: NonNullable<Parameters<typeof this.fetch>[1]>,
    window?: Window,
    more = false,
  ): Promise<void> {
    const id = this.id,
      binding = this.binding,
      generation = this.generation;
    if (!id || !binding) return;
    this.busy = true;
    try {
      const result = await this.fetch(
        { id },
        { binding, ...params },
        { signal: this.controller.signal, headers: { "X-AgentsView-Lookup-Intent": "docbank" } },
      );
      if (generation !== this.generation) return;
      if (result.binding !== binding) {
        this.dispose();
        return;
      }
      this.integration = result.integration;
      const records = new Map(this.occurrences);
      for (const occurrence of result.occurrences) {
        const previousOccurrence = records.get(occurrence.occurrence_id);
        const previous = more ? (previousOccurrence?.matches ?? []) : [];
        const unique = new Map(
          [...previous, ...occurrence.matches].map((match) => [
            `${match.node_id}/${match.version_id}`,
            match,
          ]),
        );
        const retryableContinuation =
          more && (occurrence.state === "deferred" || occurrence.state === "unavailable");
        records.set(occurrence.occurrence_id, {
          ...occurrence,
          matches: [...unique.values()],
          next_reference_offset: retryableContinuation
            ? (params.reference_offset ?? previousOccurrence?.next_reference_offset ?? null)
            : occurrence.next_reference_offset,
        });
      }
      this.occurrences = records;
      if (window) {
        if (params.first_ordinal !== undefined) window.next = result.next_offset;
        window.retry = [...records.values()].some(
          (o) =>
            o.message_ordinal >= window!.first &&
            o.message_ordinal <= window!.last &&
            (o.state === "deferred" || o.state === "unavailable"),
        );
      }
      this.unavailable =
        result.integration === "unavailable" ||
        (result.integration !== "disabled" &&
          [...records.values()].some((o) => o.state === "unavailable"));
    } catch (error) {
      if (generation !== this.generation || isAbortError(error)) return;
      if ((error as { status?: number } | null)?.status === 409) {
        this.dispose();
        return;
      }
      this.unavailable = true;
      if (window) window.retry = true;
    } finally {
      if (generation === this.generation) {
        this.busy = false;
        this.canCheckRemaining =
          this.integration !== "disabled" && this.windows.some((w) => w.retry || w.next !== null);
      }
    }
  }
  async checkRemaining(): Promise<void> {
    if (this.busy) return;
    const window = this.windows.find((w) => w.next !== null) ?? this.windows.find((w) => w.retry);
    if (!window) return;
    if (window.next !== null) {
      window.offset = window.next;
      await this.request(
        {
          first_ordinal: window.first,
          last_ordinal: window.last,
          offset: window.offset,
          limit: 20,
        },
        window,
      );
      return;
    }
    // Rotate attempted retries so a persistent outage cannot starve later windows.
    this.windows = this.windows.filter((w) => w !== window);
    this.windows.push(window);
    const unresolved = [...this.occurrences.values()]
      .filter(
        (o) =>
          o.message_ordinal >= window.first &&
          o.message_ordinal <= window.last &&
          (o.state === "deferred" || o.state === "unavailable"),
      )
      .sort(
        (a, b) =>
          a.message_ordinal - b.message_ordinal ||
          a.block_index - b.block_index ||
          a.occurrence_id.localeCompare(b.occurrence_id),
      );
    if (window.retry && unresolved.length > 0) {
      const previous = unresolved.findIndex((o) => o.occurrence_id === window.retryAfter);
      const target = unresolved[(previous + 1) % unresolved.length]!;
      window.retryAfter = target.occurrence_id;
      await this.request({ occurrence_id: target.occurrence_id, limit: 20, refresh: true }, window);
      return;
    }
    await this.request(
      {
        first_ordinal: window.first,
        last_ordinal: window.last,
        offset: window.offset,
        limit: 20,
        ...(window.retry ? { refresh: true } : {}),
      },
      window,
    );
  }
  async loadMore(occurrenceId: string): Promise<void> {
    if (this.busy) return;
    const occurrence = this.occurrences.get(occurrenceId);
    if (occurrence?.next_reference_offset == null) return;
    await this.request(
      {
        occurrence_id: occurrenceId,
        reference_offset: occurrence.next_reference_offset,
        limit: 20,
      },
      undefined,
      true,
    );
  }
}
