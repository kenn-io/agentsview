export interface ReadProgressMarker {
  ordinal: number;
  messageCount: number;
  totalMessageCount?: number;
}

interface StoredReadProgress {
  version: 1;
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
    ) {
      return null;
    }
    return localStorage;
  } catch {
    return null;
  }
}

function isMarker(value: unknown): value is ReadProgressMarker {
  if (!value || typeof value !== "object") return false;
  const marker = value as Record<string, unknown>;
  return (
    Number.isInteger(marker.ordinal) &&
    (marker.ordinal as number) >= -1 &&
    Number.isInteger(marker.messageCount) &&
    (marker.messageCount as number) >= 0
  );
}

function readStoredMarkers(): Record<string, ReadProgressMarker> {
  try {
    const local = storage();
    if (!local) return {};
    const raw = local.getItem(STORAGE_KEY);
    if (!raw) return {};
    const stored = JSON.parse(raw) as Partial<StoredReadProgress>;
    if (
      stored.version !== 1 ||
      !stored.sessions ||
      typeof stored.sessions !== "object" ||
      Array.isArray(stored.sessions)
    ) {
      return {};
    }
    return Object.fromEntries(
      Object.entries(stored.sessions).filter(([, marker]) => isMarker(marker)),
    );
  } catch {
    return {};
  }
}

function safeCount(value: number): number {
  return Number.isInteger(value) && value >= 0 ? value : 0;
}

export class ReadProgressStore {
  private markers: Record<string, ReadProgressMarker> = $state(
    readStoredMarkers(),
  );
  private visibleCounts: Record<string, number> = $state({});

  get(sessionId: string): ReadProgressMarker | null {
    return this.markers[sessionId] ?? null;
  }

  baseline(sessionId: string, ordinal: number, messageCount: number) {
    if (!sessionId || this.markers[sessionId]) return;
    this.markers = {
      ...this.markers,
      [sessionId]: {
        ordinal: Number.isInteger(ordinal) && ordinal >= -1 ? ordinal : -1,
        messageCount: safeCount(messageCount),
      },
    };
    this.persist();
  }

  recordVisible(sessionId: string, ordinal: number, messageCount: number, totalMessageCount = messageCount) {
    const marker = this.markers[sessionId];
    if (!marker || !Number.isInteger(ordinal)) return;
    if (ordinal <= marker.ordinal) {
      if (safeCount(messageCount) <= marker.messageCount && safeCount(totalMessageCount) > (marker.totalMessageCount ?? marker.messageCount)) {
        this.markers = { ...this.markers, [sessionId]: { ...marker, totalMessageCount: safeCount(totalMessageCount) } };
        this.persist();
      }
      return;
    }
    this.markers = {
      ...this.markers,
      [sessionId]: {
        ordinal,
        messageCount: Math.max(
          marker.messageCount,
          Math.min(safeCount(messageCount), ordinal + 1),
        ),
        ...(safeCount(totalMessageCount) > safeCount(messageCount)
          ? { totalMessageCount: safeCount(totalMessageCount) }
          : {}),
      },
    };
    this.persist();
  }

  setVisibleCount(sessionId: string, messageCount: number) {
    if (!sessionId) return;
    const count = safeCount(messageCount);
    if (this.visibleCounts[sessionId] === count) return;
    this.visibleCounts = { ...this.visibleCounts, [sessionId]: count };
  }

  clearVisibleCount(sessionId: string) {
    if (!(sessionId in this.visibleCounts)) return;
    const { [sessionId]: _, ...remaining } = this.visibleCounts;
    this.visibleCounts = remaining;
  }

  clear(sessionId: string) {
    if (!this.markers[sessionId]) return;
    const { [sessionId]: _, ...remaining } = this.markers;
    this.markers = remaining;
    this.persist();
  }

  hasUnread(sessionId: string, messageCount: number): boolean {
    const marker = this.markers[sessionId];
    if (!marker) return false;
    if (this.visibleCounts[sessionId] !== undefined) return this.visibleCounts[sessionId] > marker.messageCount;
    return safeCount(messageCount) > (marker.totalMessageCount ?? marker.messageCount);
  }

  private persist() {
    try {
      const local = storage();
      if (!local) return;
      const value: StoredReadProgress = {
        version: 1,
        sessions: this.markers,
      };
      local.setItem(STORAGE_KEY, JSON.stringify(value));
    } catch {
      // Storage is optional, keep the in-memory marker usable.
    }
  }
}

export const readProgress = new ReadProgressStore();
