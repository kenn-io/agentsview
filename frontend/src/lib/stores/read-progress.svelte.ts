export interface ReadProgressMarker {
  ordinal: number;
  messageCount: number;
}

interface StoredReadProgress {
  version: 1;
  sessions: Record<string, ReadProgressMarker>;
}

const STORAGE_KEY = "agentsview-read-progress";

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
    if (typeof localStorage === "undefined") return {};
    const raw = localStorage.getItem(STORAGE_KEY);
    if (!raw) return {};
    const stored = JSON.parse(raw) as Partial<StoredReadProgress>;
    if (stored.version !== 1 || !stored.sessions || typeof stored.sessions !== "object") {
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

  recordVisible(sessionId: string, ordinal: number, messageCount: number) {
    const marker = this.markers[sessionId];
    if (!marker || !Number.isInteger(ordinal) || ordinal <= marker.ordinal) return;
    this.markers = {
      ...this.markers,
      [sessionId]: {
        ordinal,
        messageCount: Math.max(
          marker.messageCount,
          Math.min(safeCount(messageCount), ordinal + 1),
        ),
      },
    };
    this.persist();
  }

  clear(sessionId: string) {
    if (!this.markers[sessionId]) return;
    const { [sessionId]: _, ...remaining } = this.markers;
    this.markers = remaining;
    this.persist();
  }

  hasUnread(sessionId: string, messageCount: number): boolean {
    const marker = this.markers[sessionId];
    return marker !== undefined && safeCount(messageCount) > marker.messageCount;
  }

  private persist() {
    try {
      if (typeof localStorage === "undefined") return;
      const value: StoredReadProgress = {
        version: 1,
        sessions: this.markers,
      };
      localStorage.setItem(STORAGE_KEY, JSON.stringify(value));
    } catch {
      // Storage is optional, keep the in-memory marker usable.
    }
  }
}

export const readProgress = new ReadProgressStore();
