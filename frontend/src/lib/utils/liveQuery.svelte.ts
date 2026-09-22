import { untrack } from "svelte";
import type { QueryStep } from "./refresh.js";

/**
 * The data query a page is running right now, so the refresh control can
 * count its time up and draw each step as it starts and settles. Stores
 * keep their last completed query separately; this only describes the one
 * in flight and is empty when nothing is running.
 *
 * Times are on the `performance.now()` clock. Step offsets are relative to
 * `startedAt`, like the completed steps the stores record.
 *
 * Loaders call these methods from inside effects, so the methods read their
 * own state untracked: a load must not make its caller depend on the
 * timeline it writes.
 */
export class LiveQuery {
  /** When the running query began, or null when none is running. */
  startedAt: number | null = $state(null);
  /** Steps seen so far, in start order. Running ones carry `running: true`
   * and a zero duration; the control extends them to the current time. */
  steps: QueryStep[] = $state([]);

  private generation = 0;
  private nextStepId = 0;
  // Name of each step still running, by the handle `start` returned.
  private running = new Map<number, string>();

  /** Starts a new query, replacing any earlier one. Returns the token that
   * `end` needs, so a superseded query cannot end its replacement. */
  begin(at: number): number {
    this.generation++;
    this.startedAt = at;
    this.steps = [];
    this.running.clear();
    return this.generation;
  }

  /** Ends the query `generation` started, or whichever is running when no
   * token is given. */
  end(generation?: number): void {
    if (generation !== undefined && generation !== this.generation) return;
    this.startedAt = null;
    this.steps = [];
    this.running.clear();
  }

  /** Records a step that began at `at` and returns its handle for `settle`
   * or `abandon`. A listed step of the same name, such as an earlier try of
   * a retried request, is replaced and its handle stops working. Returns 0,
   * which matches nothing, when no query is running. */
  start(name: string, at: number): number {
    const startedAt = untrack(() => this.startedAt);
    if (startedAt === null) return 0;
    const id = ++this.nextStepId;
    for (const [other, otherName] of this.running) {
      if (otherName === name) this.running.delete(other);
    }
    this.running.set(id, name);
    this.steps = [
      ...untrack(() => this.steps).filter((step) => step.name !== name),
      { name, startMs: at - startedAt, durationMs: 0, running: true },
    ];
    return id;
  }

  /** Replaces the running step `id` with its measured timing. */
  settle(id: number, step: QueryStep): void {
    const name = this.running.get(id);
    if (name === undefined) return;
    this.running.delete(id);
    this.steps = untrack(() => this.steps).map((s) => (s.name === name ? step : s));
  }

  /** Drops the step `id` if it stopped without data (an error or an abort).
   * A step that already settled stays. */
  abandon(id: number): void {
    const name = this.running.get(id);
    if (name === undefined) return;
    this.running.delete(id);
    this.steps = untrack(() => this.steps).filter((s) => s.name !== name);
  }
}
