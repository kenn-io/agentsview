import * as api from "../api/client.js";
import type {
  SyncProgress,
  SyncStats,
  Stats,
  VersionInfo,
  UpdateCheck,
} from "../api/types.js";
import type { SessionTiming } from "../api/types/timing.js";

type SyncCompleteListener = () => void;

const POLL_INTERVAL_MS = 10_000;

/**
 * Compare two commit hashes, tolerating short vs full SHA.
 * Returns true when both are known and they disagree.
 * Uses prefix comparison only when one hash is shorter
 * than the other (i.e. an abbreviation).
 */
export function commitsDisagree(
  a: string | undefined,
  b: string | undefined,
): boolean {
  if (!a || !b) return false;
  if (a === "unknown" || b === "unknown") return false;
  if (a === b) return false;
  if (a.length === b.length) return true;
  const minLen = Math.min(a.length, b.length);
  return a.slice(0, minLen) !== b.slice(0, minLen);
}

class SyncStore {
  syncing: boolean = $state(false);
  progress: SyncProgress | null = $state(null);
  lastSync: string | null = $state(null);
  lastSyncStats: SyncStats | null = $state(null);
  stats: Stats | null = $state(null);
  serverVersion: VersionInfo | null = $state(null);
  versionMismatch: boolean = $state(false);
  updateAvailable: boolean = $state(false);
  latestVersion: string | null = $state(null);
  readonly buildCommit: string =
    import.meta.env.VITE_BUILD_COMMIT;
  readonly isDesktop: boolean =
    typeof window !== "undefined" &&
    new URLSearchParams(window.location.search).has("desktop");

  private watchEventSource: EventSource | null = null;
  private pollTimer: ReturnType<typeof setInterval> | null =
    null;
  private lastStatsParams: {
    include_one_shot?: boolean;
    include_automated?: boolean;
  } = { include_one_shot: true };
  private statsVersion = 0;
  private syncCompleteListeners: SyncCompleteListener[] = [];
  private statusHydrated = false;
  private pendingHydration = false;

  /** Register a callback invoked after any sync completes. */
  onSyncComplete(listener: SyncCompleteListener) {
    this.syncCompleteListeners.push(listener);
  }

  private notifySyncComplete() {
    for (const fn of this.syncCompleteListeners) {
      fn();
    }
  }

  async loadStatus() {
    try {
      const status = await api.getSyncStatus();
      const newLastSync = status.last_sync || null;
      const isInitial = !this.statusHydrated;
      this.statusHydrated = true;
      const changed =
        newLastSync !== null && newLastSync !== this.lastSync;
      this.lastSync = newLastSync;
      this.lastSyncStats = status.stats;
      // Suppress notifications on initial hydration and
      // when a local sync just completed (pendingHydration).
      if (this.pendingHydration) {
        this.pendingHydration = false;
      } else if (changed && !isInitial) {
        this.loadStats();
        this.notifySyncComplete();
      }
    } catch (error) {
      this.pendingHydration = false;
      console.warn("Failed to load sync status:", error);
    }
  }

  startPolling() {
    this.stopPolling();
    this.pollTimer = setInterval(
      () => this.loadStatus(),
      POLL_INTERVAL_MS,
    );
  }

  stopPolling() {
    if (this.pollTimer) {
      clearInterval(this.pollTimer);
      this.pollTimer = null;
    }
  }

  async loadStats(
    params?: {
      include_one_shot?: boolean;
      include_automated?: boolean;
    },
  ) {
    if (params !== undefined) {
      this.lastStatsParams = params;
    }
    const version = ++this.statsVersion;
    try {
      const result = await api.getStats(
        this.lastStatsParams,
      );
      if (this.statsVersion === version) {
        this.stats = result;
      }
    } catch (error) {
      console.warn("Failed to load sync stats:", error);
    }
  }

  async loadVersion() {
    try {
      this.serverVersion = await api.getVersion();
      this.versionMismatch = commitsDisagree(
        this.buildCommit,
        this.serverVersion.commit,
      );
    } catch (error) {
      console.warn("Failed to load version info:", error);
    }
  }

  async checkForUpdate() {
    // Desktop app uses the native Tauri updater; the
    // Go backend endpoint checks upstream releases which
    // is irrelevant and potentially wrong for forks.
    if (this.isDesktop) return;
    try {
      const result: UpdateCheck =
        await api.checkForUpdate();
      this.updateAvailable = result.update_available;
      this.latestVersion = result.latest_version ?? null;
    } catch (error) {
      console.warn("Failed to check for updates:", error);
    }
  }

  triggerSync(onComplete?: () => void) {
    this.runSync(api.triggerSync, onComplete);
  }

  triggerResync(
    onComplete?: () => void,
    onError?: (err: Error) => void,
  ): boolean {
    return this.runSync(
      api.triggerResync,
      onComplete,
      onError,
    );
  }

  private runSync(
    syncFn: (
      onProgress?: (p: SyncProgress) => void,
    ) => api.SyncHandle,
    onComplete?: () => void,
    onError?: (err: Error) => void,
  ): boolean {
    if (this.syncing) return false;
    this.syncing = true;
    this.progress = null;

    const finalizeSync = () => {
      this.syncing = false;
      this.progress = null;
    };

    const handle = syncFn((p: SyncProgress) => {
      this.progress = p;
    });

    handle.done
      .then((s: SyncStats) => {
        this.lastSyncStats = s;
        this.loadStats();
        finalizeSync();
        this.notifySyncComplete();
        // Hydrate the authoritative server timestamp.
        // pendingHydration suppresses the notification so
        // the poll path won't double-fire.
        this.pendingHydration = true;
        this.loadStatus();
        onComplete?.();
      })
      .catch((err: unknown) => {
        if (
          err instanceof DOMException &&
          err.name === "AbortError"
        ) {
          return;
        }
        finalizeSync();
        if (err instanceof Error) {
          onError?.(err);
        } else {
          onError?.(new Error("Sync failed"));
        }
      });

    return true;
  }

  watchSession(
    sessionId: string,
    onUpdate: () => void,
    onTiming?: (t: SessionTiming) => void,
  ) {
    this.unwatchSession();
    this.watchEventSource = api.watchSession(
      sessionId,
      onUpdate,
      onTiming,
    );
  }

  unwatchSession() {
    if (this.watchEventSource) {
      this.watchEventSource.close();
      this.watchEventSource = null;
    }
  }
}

export const sync = new SyncStore();
