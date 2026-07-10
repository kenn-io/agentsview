export interface ReadProgressMarker {
  seenOrdinal: number | null;
}

interface StoredReadProgress {
  version: 2;
  sessions: Record<string, ReadProgressMarker>;
}

const STORAGE_KEY = "agentsview-read-progress";

type StorageLike = Pick<Storage, "getItem" | "setItem">;

function storage(): StorageLike | null {
  try {
    if (
      typeof localStorage === "undefined" ||
      localStorage == null ||
      typeof localStorage.getItem !== "function" ||
      typeof localStorage.setItem !== "function"
    ) return null;
    return localStorage;
  } catch {
    return null;
  }
}

function isOrdinal(value: unknown): value is number | null {
  return value === null || (Number.isInteger(value) && (value as number) >= 0);
}

function isMarker(value: unknown): value is ReadProgressMarker {
  return !!value && typeof value === "object" &&
    isOrdinal((value as Record<string, unknown>).seenOrdinal);
}

function readStoredMarkers(): Record<string, ReadProgressMarker> {
  try {
    const raw = storage()?.getItem(STORAGE_KEY);
    if (!raw) return {};
    const stored = JSON.parse(raw) as {
      version?: unknown;
      sessions?: unknown;
    };
    if (!stored.sessions || typeof stored.sessions !== "object" ||
      Array.isArray(stored.sessions)) return {};
    const entries = Object.entries(stored.sessions);
    if (stored.version === 2) {
      return Object.fromEntries(entries.filter(([, marker]) => isMarker(marker)));
    }
    if (stored.version === 1) {
      return Object.fromEntries(entries.flatMap(([id, value]) => {
        const ordinal = (value as Record<string, unknown> | null)?.ordinal;
        return Number.isInteger(ordinal) && (ordinal as number) >= -1
          ? [[id, { seenOrdinal: ordinal === -1 ? null : ordinal as number }]]
          : [];
      }));
    }
    return {};
  } catch {
    return {};
  }
}

export class ReadProgressStore {
  private markers: Record<string, ReadProgressMarker> = $state(
    readStoredMarkers(),
  );

  get(sessionId: string): ReadProgressMarker | null {
    return this.markers[sessionId] ?? null;
  }

  baseline(sessionId: string, latestDisplayOrdinal: number | null) {
    if (!sessionId || !isOrdinal(latestDisplayOrdinal)) return;
    const marker = this.markers[sessionId];
    if (!marker || this.regressed(marker.seenOrdinal, latestDisplayOrdinal)) {
      this.set(sessionId, latestDisplayOrdinal);
    }
  }

  reconcile(sessionId: string, latestDisplayOrdinal: number | null) {
    if (!sessionId || !isOrdinal(latestDisplayOrdinal)) return;
    const marker = this.markers[sessionId];
    if (marker && this.regressed(marker.seenOrdinal, latestDisplayOrdinal)) {
      this.set(sessionId, latestDisplayOrdinal);
    }
  }

  recordVisible(sessionId: string, observedOrdinal: number) {
    const marker = this.markers[sessionId];
    if (!marker || !Number.isInteger(observedOrdinal) || observedOrdinal < 0) return;
    if (marker.seenOrdinal === null || observedOrdinal > marker.seenOrdinal) {
      this.set(sessionId, observedOrdinal);
    }
  }

  clear(sessionId: string) {
    if (!this.markers[sessionId]) return;
    const { [sessionId]: _, ...remaining } = this.markers;
    this.markers = remaining;
    this.persist();
  }

  hasUnread(sessionId: string, latestDisplayOrdinal: number | null): boolean {
    const marker = this.markers[sessionId];
    return !!marker && latestDisplayOrdinal !== null &&
      (marker.seenOrdinal === null || latestDisplayOrdinal > marker.seenOrdinal);
  }

  private regressed(seen: number | null, latest: number | null): boolean {
    return seen !== null && (latest === null || latest < seen);
  }

  private set(sessionId: string, seenOrdinal: number | null) {
    this.markers = { ...this.markers, [sessionId]: { seenOrdinal } };
    this.persist();
  }

  private persist() {
    try {
      const value: StoredReadProgress = { version: 2, sessions: this.markers };
      storage()?.setItem(STORAGE_KEY, JSON.stringify(value));
    } catch {
      // Storage is optional, keep the in-memory marker usable.
    }
  }
}

export const readProgress = new ReadProgressStore();
