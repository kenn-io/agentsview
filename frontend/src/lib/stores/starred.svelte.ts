import * as api from "../api/client.js";

const STORAGE_KEY = "agentsview-starred-sessions";

class StarredStore {
  // Seed from localStorage so legacy stars are visible immediately,
  // before the async server load and migration complete.
  ids: Set<string> = $state(readLocalStorage());
  filterOnly: boolean = $state(false);
  private loaded = false;
  private loading: Promise<void> | null = null;
  /** Global mutation counter for load/migration staleness detection. */
  private mutationVersion = 0;
  /** Monotonic counter for listStarred refresh calls so only the
   *  latest response applies when multiple are in-flight. */
  private refreshId = 0;
  /** Per-session promise chains to serialize server mutations. */
  private queues: Map<string, Promise<void>> = new Map();
  private retryCount = 0;
  private retryTimer: ReturnType<typeof setTimeout> | null = null;
  private reconcileTimer: ReturnType<typeof setTimeout> | null = null;
  private reconcileRetries = 0;

  async load() {
    if (this.loaded) return;
    if (this.loading) return this.loading;
    this.loading = this.doLoad();
    return this.loading;
  }

  private async doLoad() {
    const mutVer = this.mutationVersion;
    const rid = ++this.refreshId;
    try {
      const res = await api.listStarred();
      if (this.mutationVersion === mutVer && this.refreshId === rid) {
        this.ids = new Set(res.session_ids);
      }
      try {
        await this.migrateLocalStorage();
      } finally {
        // Mark loaded after migration completes (or fails) so
        // concurrent load() callers don't see partially-initialized
        // state. Only set when listStarred succeeded.
        this.loaded = true;
        this.cancelRetry();
      }
    } catch {
      const local = readLocalStorage();
      if (local.size > 0) {
        if (this.mutationVersion === mutVer && this.refreshId === rid) {
          // No mutations during load — safe to replace.
          this.ids = local;
        } else {
          // Mutations occurred — merge local stars into current
          // optimistic state so legacy IDs aren't lost, but skip
          // IDs with in-flight mutations to avoid resurrecting
          // explicitly unstarred sessions.
          const merged = new Set(this.ids);
          for (const id of local) {
            if (!this.queues.has(id)) merged.add(id);
          }
          this.ids = merged;
        }
      }
      this.scheduleRetry();
    } finally {
      this.loading = null;
    }
  }

  private cancelRetry() {
    if (this.retryTimer !== null) {
      clearTimeout(this.retryTimer);
      this.retryTimer = null;
    }
    this.retryCount = 0;
  }

  private scheduleRetry() {
    if (this.retryTimer !== null) return;
    if (this.retryCount >= 3) return;
    const delay = 2000 * 2 ** this.retryCount;
    this.retryCount++;
    this.retryTimer = setTimeout(() => {
      this.retryTimer = null;
      this.load();
    }, delay);
  }

  private async migrateLocalStorage() {
    const local = readLocalStorage();
    if (local.size === 0) return;

    const toMigrate = [...local].filter((id) => !this.ids.has(id));
    if (toMigrate.length > 0) {
      const mutVer = this.mutationVersion;
      const rid = ++this.refreshId;
      try {
        await api.bulkStarSessions(toMigrate);
      } catch {
        // Bulk star failed — merge into memory and preserve
        // localStorage for retry on next page reload.
        const merged = new Set(this.ids);
        for (const id of toMigrate) merged.add(id);
        this.ids = merged;
        return;
      }
      // Server has the data — clear localStorage immediately so
      // stale IDs are never re-migrated on a later reload.
      clearLocalStorage();
      try {
        const refreshed = await api.listStarred();
        if (this.mutationVersion === mutVer && this.refreshId === rid) {
          this.ids = new Set(refreshed.session_ids);
        }
      } catch {
        // Refresh failed — don't merge toMigrate IDs because
        // the server silently skips stale session IDs during
        // bulk star. Merging unverified IDs would introduce
        // phantom stars. Schedule retried reconciliation so
        // the correct server state is fetched once connectivity
        // recovers.
        this.scheduleReconcile();
      }
    } else {
      clearLocalStorage();
    }
  }

  isStarred(sessionId: string): boolean {
    return this.ids.has(sessionId);
  }

  toggle(sessionId: string) {
    if (this.ids.has(sessionId)) {
      this.unstar(sessionId);
    } else {
      this.star(sessionId);
    }
  }

  star(sessionId: string) {
    if (this.ids.has(sessionId)) return;
    const next = new Set(this.ids);
    next.add(sessionId);
    this.ids = next;
    this.mutationVersion++;
    this.enqueue(sessionId, () => api.starSession(sessionId));
  }

  unstar(sessionId: string) {
    if (!this.ids.has(sessionId)) return;
    const next = new Set(this.ids);
    next.delete(sessionId);
    this.ids = next;
    this.mutationVersion++;
    // Mirror into localStorage while the legacy key exists so
    // a migration retry doesn't re-star this session.
    removeFromLocalStorage(sessionId);
    this.enqueue(sessionId, () => api.unstarSession(sessionId));
  }

  private enqueue(
    sessionId: string,
    op: () => Promise<unknown>,
  ) {
    const prev = this.queues.get(sessionId) ?? Promise.resolve();
    const chain: Promise<void> = prev
      .then(() => op(), () => op())
      .then(() => {}, () => {})
      .then(() => {
        if (this.queues.get(sessionId) === chain) {
          this.queues.delete(sessionId);
        }
        this.reconcileIfIdle();
      });
    this.queues.set(sessionId, chain);
  }

  /**
   * After all in-flight mutations settle, re-fetch server state
   * to correct any drift from failed requests. Uses refreshId to
   * ensure only the latest listStarred response is applied when
   * multiple refreshes are in-flight.
   */
  private reconcileIfIdle() {
    if (this.queues.size > 0) return;
    const mutVer = this.mutationVersion;
    const rid = ++this.refreshId;
    api.listStarred().then((res) => {
      if (this.mutationVersion === mutVer && this.refreshId === rid) {
        this.ids = new Set(res.session_ids);
      }
    }).catch(() => {
      // Server unavailable; keep optimistic state.
    });
  }

  /**
   * Retried reconciliation for post-migration refresh failures.
   * Unlike reconcileIfIdle (single fire-and-forget), this retries
   * with backoff so migrated IDs eventually appear even if the
   * server is still temporarily unavailable.
   */
  private scheduleReconcile() {
    if (this.reconcileTimer !== null) return;
    if (this.reconcileRetries >= 3) return;
    const delay = 2000 * 2 ** this.reconcileRetries;
    this.reconcileRetries++;
    this.reconcileTimer = setTimeout(() => {
      this.reconcileTimer = null;
      const mutVer = this.mutationVersion;
      const rid = ++this.refreshId;
      api.listStarred().then((res) => {
        if (
          this.mutationVersion === mutVer &&
          this.refreshId === rid
        ) {
          this.ids = new Set(res.session_ids);
        }
        this.reconcileRetries = 0;
      }).catch(() => {
        this.scheduleReconcile();
      });
    }, delay);
  }

  get count(): number {
    return this.ids.size;
  }
}

function readLocalStorage(): Set<string> {
  try {
    const raw = localStorage?.getItem(STORAGE_KEY);
    if (raw) {
      const arr = JSON.parse(raw);
      if (Array.isArray(arr)) return new Set(arr);
    }
  } catch {
    // ignore
  }
  return new Set();
}

function clearLocalStorage() {
  try {
    localStorage?.removeItem(STORAGE_KEY);
  } catch {
    // ignore
  }
}

/** Remove a single ID from localStorage (if the key exists). */
function removeFromLocalStorage(id: string) {
  try {
    const raw = localStorage?.getItem(STORAGE_KEY);
    if (!raw) return;
    const arr = JSON.parse(raw);
    if (!Array.isArray(arr)) return;
    const filtered = arr.filter((v: unknown) => v !== id);
    if (filtered.length === arr.length) return;
    if (filtered.length === 0) {
      localStorage?.removeItem(STORAGE_KEY);
    } else {
      localStorage?.setItem(STORAGE_KEY, JSON.stringify(filtered));
    }
  } catch {
    // ignore
  }
}

export function createStarredStore(): StarredStore {
  return new StarredStore();
}

export const starred = createStarredStore();
