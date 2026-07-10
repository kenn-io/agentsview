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
    (marker.messageCount as number) >= 0 &&
    (marker.totalMessageCount === undefined ||
      (Number.isInteger(marker.totalMessageCount) &&
        (marker.totalMessageCount as number) >= (marker.messageCount as number)))
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

function acknowledgedTotal(
  displayCount: number,
  eligibleTotal: number | undefined,
): number | undefined {
  if (
    eligibleTotal === undefined ||
    !Number.isInteger(eligibleTotal) ||
    eligibleTotal < safeCount(displayCount)
  ) {
    return undefined;
  }
  return eligibleTotal;
}

export class ReadProgressStore {
  private markers: Record<string, ReadProgressMarker> = $state(
    readStoredMarkers(),
  );
  private visibleCounts: Record<string, number> = $state({});

  get(sessionId: string): ReadProgressMarker | null {
    return this.markers[sessionId] ?? null;
  }

  baseline(
    sessionId: string,
    displayOrdinal: number,
    displayCount: number,
    eligibleAcknowledgedTotal?: number,
  ) {
    if (!sessionId || this.markers[sessionId]) return;
    const totalMessageCount = acknowledgedTotal(
      displayCount,
      eligibleAcknowledgedTotal,
    );
    this.markers = {
      ...this.markers,
      [sessionId]: {
        ordinal: Number.isInteger(displayOrdinal) && displayOrdinal >= -1
          ? displayOrdinal
          : -1,
        messageCount: safeCount(displayCount),
        ...(totalMessageCount !== undefined ? { totalMessageCount } : {}),
      },
    };
    this.persist();
  }

  recordVisible(
    sessionId: string,
    observedOrdinal: number,
    latestDisplayOrdinal: number,
    displayCount: number,
    eligibleAcknowledgedTotal?: number,
  ) {
    const marker = this.markers[sessionId];
    if (!marker || !Number.isInteger(observedOrdinal)) return;
    const totalMessageCount = observedOrdinal === latestDisplayOrdinal
      ? acknowledgedTotal(displayCount, eligibleAcknowledgedTotal)
      : undefined;
    if (observedOrdinal <= marker.ordinal) {
      if (
        safeCount(displayCount) <= marker.messageCount &&
        totalMessageCount !== undefined &&
        totalMessageCount > (marker.totalMessageCount ?? marker.messageCount)
      ) {
        this.markers = {
          ...this.markers,
          [sessionId]: { ...marker, totalMessageCount },
        };
        this.persist();
      }
      return;
    }
    this.markers = {
      ...this.markers,
      [sessionId]: {
        ordinal: observedOrdinal,
        messageCount: Math.max(
          marker.messageCount,
          Math.min(safeCount(displayCount), observedOrdinal + 1),
        ),
        ...(totalMessageCount !== undefined &&
          totalMessageCount > (marker.totalMessageCount ?? marker.messageCount)
          ? { totalMessageCount }
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
