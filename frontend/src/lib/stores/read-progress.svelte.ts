export interface ReadProgressMarker {
  seenOrdinal: number | null;
  seenContentLength?: number | null;
}

const INDEX_KEY = "agentsview-read-progress:index";
const MARKER_KEY_PREFIX = "agentsview-read-progress:session:";
const DEFAULT_CAPACITY = 1_000;

type StorageLike = Pick<Storage, "getItem" | "setItem" | "removeItem">;

function storage(): StorageLike | null {
  try {
    if (
      typeof localStorage === "undefined" ||
      localStorage == null ||
      typeof localStorage.getItem !== "function" ||
      typeof localStorage.setItem !== "function" ||
      typeof localStorage.removeItem !== "function"
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

function markerKey(sessionId: string): string {
  return `${MARKER_KEY_PREFIX}${sessionId}`;
}

function readStoredRecency(): string[] {
  try {
    const raw = storage()?.getItem(INDEX_KEY);
    if (!raw) return [];
    const parsed = JSON.parse(raw) as unknown;
    if (!Array.isArray(parsed)) return [];
    const seen = new Set<string>();
    return [...parsed].reverse().flatMap((id) => {
      if (typeof id !== "string" || !id || seen.has(id)) return [];
      seen.add(id);
      return [id];
    }).reverse();
  } catch {
    return [];
  }
}

function readStoredMarker(sessionId: string): ReadProgressMarker | null {
  try {
    const raw = storage()?.getItem(markerKey(sessionId));
    if (!raw) return null;
    const marker = JSON.parse(raw) as unknown;
    return isMarker(marker) ? marker : null;
  } catch {
    return null;
  }
}

function removeStoredMarker(sessionId: string) {
  try {
    storage()?.removeItem(markerKey(sessionId));
  } catch {
    // Storage is optional, keep the in-memory marker usable.
  }
}

function persistRecency(recency: string[]) {
  try {
    storage()?.setItem(INDEX_KEY, JSON.stringify(recency));
  } catch {
    // Storage is optional, keep the in-memory marker usable.
  }
}

function persistMarker(sessionId: string, marker: ReadProgressMarker) {
  try {
    storage()?.setItem(markerKey(sessionId), JSON.stringify(marker));
  } catch {
    // Storage is optional, keep the in-memory marker usable.
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
    const storedRecency = readStoredRecency();
    const retained = storedRecency.slice(-capacity);
    for (const id of storedRecency.slice(0, -capacity)) {
      removeStoredMarker(id);
    }
    for (const id of retained) {
      const marker = readStoredMarker(id);
      if (marker) {
        this.markers[id] = marker;
        this.recency.push(id);
      } else {
        removeStoredMarker(id);
      }
    }
    if (
      this.recency.length !== storedRecency.length ||
      this.recency.some((id, index) => id !== storedRecency[index])
    ) {
      persistRecency(this.recency);
    }
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
    }
    this.touch(sessionId);
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
    delete this.markers[sessionId];
    this.recency = this.recency.filter((id) => id !== sessionId);
    removeStoredMarker(sessionId);
    persistRecency(this.recency);
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
    this.markers[sessionId] = marker;
    persistMarker(sessionId, marker);
  }

  private touch(sessionId: string) {
    if (!this.markers[sessionId] || this.recency.at(-1) === sessionId) return;
    const recency = this.recency.filter((id) => id !== sessionId);
    recency.push(sessionId);
    const evicted = recency.length > this.capacity
      ? recency.splice(0, recency.length - this.capacity)
      : [];
    for (const id of evicted) {
      delete this.markers[id];
      removeStoredMarker(id);
    }
    this.recency = recency;
    persistRecency(this.recency);
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
