export interface ReadProgressMarker {
  seenOrdinal: number | null;
  seenContentLength?: number | null;
}

interface StoredReadProgress {
  version: 3;
  /** Session IDs ordered from least to most recently visited. */
  recency: string[];
  sessions: Record<string, ReadProgressMarker>;
}

const STORAGE_KEY = "agentsview-read-progress";
const DEFAULT_CAPACITY = 1_000;

interface ReadProgressSnapshot {
  sessions: Record<string, ReadProgressMarker>;
  recency: string[];
}

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

function isContentLength(value: unknown): value is number | null | undefined {
  return value === undefined || value === null ||
    (Number.isInteger(value) && (value as number) >= 0);
}

function isMarker(value: unknown): value is ReadProgressMarker {
  return !!value && typeof value === "object" &&
    isOrdinal((value as Record<string, unknown>).seenOrdinal) &&
    isContentLength((value as Record<string, unknown>).seenContentLength);
}

function snapshot(
  sessions: Record<string, ReadProgressMarker>,
  recency: unknown,
): ReadProgressSnapshot {
  const ordered: string[] = [];
  const seen = new Set<string>();
  if (Array.isArray(recency)) {
    for (const id of recency) {
      if (typeof id !== "string" || seen.has(id) || !sessions[id]) continue;
      seen.add(id);
      ordered.push(id);
    }
  }
  for (const id of Object.keys(sessions)) {
    if (seen.has(id)) continue;
    ordered.push(id);
  }
  return { sessions, recency: ordered };
}

function readStoredMarkers(): ReadProgressSnapshot {
  try {
    const raw = storage()?.getItem(STORAGE_KEY);
    if (!raw) return snapshot({}, []);
    const stored = JSON.parse(raw) as {
      version?: unknown;
      sessions?: unknown;
      recency?: unknown;
    };
    if (!stored.sessions || typeof stored.sessions !== "object" ||
      Array.isArray(stored.sessions)) return snapshot({}, []);
    const entries = Object.entries(stored.sessions);
    if (stored.version === 3) {
      if (!Array.isArray(stored.recency)) return snapshot({}, []);
      return snapshot(
        Object.fromEntries(entries.filter(([, marker]) => isMarker(marker))),
        stored.recency,
      );
    }
    if (stored.version === 2) {
      const sessions = Object.fromEntries(entries.flatMap(([id, value]) =>
        isMarker(value)
          ? [[id, { seenOrdinal: value.seenOrdinal }]]
          : []
      ));
      return snapshot(sessions, Object.keys(sessions));
    }
    if (stored.version === 1) {
      const sessions = Object.fromEntries(entries.flatMap(([id, value]) => {
        const ordinal = (value as Record<string, unknown> | null)?.ordinal;
        return Number.isInteger(ordinal) && (ordinal as number) >= -1
          ? [[id, { seenOrdinal: ordinal === -1 ? null : ordinal as number }]]
          : [];
      }));
      return snapshot(sessions, Object.keys(sessions));
    }
    return snapshot({}, []);
  } catch {
    return snapshot({}, []);
  }
}

export class ReadProgressStore {
  private markers: Record<string, ReadProgressMarker> = $state({});
  private recency: string[] = [];

  constructor(private readonly capacity = DEFAULT_CAPACITY) {
    if (!Number.isInteger(capacity) || capacity < 1) {
      throw new Error(
        `Read progress capacity must be a positive integer, got ${capacity}`,
      );
    }
    const stored = readStoredMarkers();
    this.recency = stored.recency.slice(-capacity);
    this.markers = Object.fromEntries(
      this.recency.map((id) => [id, stored.sessions[id]!]),
    );
    if (this.recency.length < stored.recency.length) this.persist();
  }

  get(sessionId: string): ReadProgressMarker | null {
    return this.markers[sessionId] ?? null;
  }

  baseline(
    sessionId: string,
    latestDisplayOrdinal: number | null,
    latestDisplayContentLength: number | null = null,
  ) {
    if (!sessionId || !this.isCursor(latestDisplayOrdinal, latestDisplayContentLength)) return;
    const marker = this.markers[sessionId];
    if (
      !marker ||
      this.regressed(marker, latestDisplayOrdinal, latestDisplayContentLength) ||
      this.shouldEnrich(marker, latestDisplayOrdinal, latestDisplayContentLength)
    ) {
      this.set(sessionId, latestDisplayOrdinal, latestDisplayContentLength);
    } else {
      this.touch(sessionId);
    }
  }

  reconcile(
    sessionId: string,
    latestDisplayOrdinal: number | null,
    latestDisplayContentLength: number | null = null,
  ) {
    if (!sessionId || !this.isCursor(latestDisplayOrdinal, latestDisplayContentLength)) return;
    const marker = this.markers[sessionId];
    if (marker && this.regressed(marker, latestDisplayOrdinal, latestDisplayContentLength)) {
      this.set(sessionId, latestDisplayOrdinal, latestDisplayContentLength);
    }
  }

  recordVisible(
    sessionId: string,
    observedOrdinal: number,
    observedContentLength?: number | null,
  ) {
    const marker = this.markers[sessionId];
    if (!marker || !this.isCursor(observedOrdinal, observedContentLength)) return;
    if (
      this.cursorGreater(observedOrdinal, observedContentLength, marker) ||
      this.shouldEnrich(marker, observedOrdinal, observedContentLength)
    ) {
      this.set(sessionId, observedOrdinal, observedContentLength);
    }
  }

  clear(sessionId: string) {
    if (!this.markers[sessionId]) return;
    const { [sessionId]: _, ...remaining } = this.markers;
    this.markers = remaining;
    this.recency = this.recency.filter((id) => id !== sessionId);
    this.persist();
  }

  hasUnread(
    sessionId: string,
    latestDisplayOrdinal: number | null,
    latestDisplayContentLength: number | null = null,
  ): boolean {
    const marker = this.markers[sessionId];
    return !!marker &&
      this.isCursor(latestDisplayOrdinal, latestDisplayContentLength) &&
      this.cursorGreater(latestDisplayOrdinal, latestDisplayContentLength, marker);
  }

  private regressed(
    marker: ReadProgressMarker,
    latestOrdinal: number | null,
    latestContentLength?: number | null,
  ): boolean {
    if (marker.seenOrdinal === null) return false;
    if (latestOrdinal === null) return true;
    if (latestOrdinal < marker.seenOrdinal) return true;
    return latestOrdinal === marker.seenOrdinal &&
      this.contentLengthLess(latestContentLength, marker.seenContentLength);
  }

  private set(
    sessionId: string,
    seenOrdinal: number | null,
    seenContentLength?: number | null,
  ) {
    const marker: ReadProgressMarker = { seenOrdinal };
    if (seenContentLength !== undefined && seenContentLength !== null) {
      marker.seenContentLength = seenContentLength;
    }
    this.storeMarker(sessionId, marker);
  }

  private touch(sessionId: string) {
    const marker = this.markers[sessionId];
    if (!marker || this.recency.at(-1) === sessionId) return;
    this.storeMarker(sessionId, marker);
  }

  private storeMarker(sessionId: string, marker: ReadProgressMarker) {
    const recency = this.recency.filter((id) => id !== sessionId);
    recency.push(sessionId);
    const evicted = recency.length > this.capacity
      ? recency.splice(0, recency.length - this.capacity)
      : [];
    const markers = { ...this.markers, [sessionId]: marker };
    for (const id of evicted) delete markers[id];
    this.recency = recency;
    this.markers = markers;
    this.persist();
  }

  private persist() {
    try {
      const value: StoredReadProgress = {
        version: 3,
        recency: this.recency,
        sessions: this.markers,
      };
      storage()?.setItem(STORAGE_KEY, JSON.stringify(value));
    } catch {
      // Storage is optional, keep the in-memory marker usable.
    }
  }

  private isCursor(
    ordinal: number | null,
    contentLength?: number | null,
  ): boolean {
    return isOrdinal(ordinal) && isContentLength(contentLength);
  }

  private shouldEnrich(
    marker: ReadProgressMarker,
    ordinal: number | null,
    contentLength: number | null | undefined,
  ): boolean {
    return ordinal !== null &&
      marker.seenOrdinal === ordinal &&
      marker.seenContentLength == null &&
      contentLength != null;
  }

  private cursorGreater(
    ordinal: number | null,
    contentLength: number | null | undefined,
    marker: ReadProgressMarker,
  ): boolean {
    if (ordinal === null) return false;
    if (marker.seenOrdinal === null) return true;
    if (ordinal > marker.seenOrdinal) return true;
    if (ordinal < marker.seenOrdinal) return false;
    if (contentLength == null || marker.seenContentLength == null) return false;
    return contentLength > marker.seenContentLength;
  }

  private contentLengthLess(
    current: number | null | undefined,
    seen: number | null | undefined,
  ): boolean {
    return current != null && seen != null && current < seen;
  }
}

export const readProgress = new ReadProgressStore();
