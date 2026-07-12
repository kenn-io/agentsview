export interface ReadProgressMarker {
  token: string;
  ordinal: number | null;
  touched_at: number;
}

interface StoredReadProgress {
  version: 1;
  sessions: Record<string, ReadProgressMarker>;
}

type StorageLike = Pick<Storage, "getItem" | "setItem">;

const STORAGE_KEY = "agentsview-read-progress";
const STORAGE_VERSION = 1;
const DEFAULT_MAX_ENTRIES = 500;

type TokenSource = {
  transcript_revision?: string | null;
  local_modified_at?: string | null;
};

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

function isOrdinal(value: unknown): value is number | null {
  return value === null ||
    (Number.isInteger(value) && (value as number) >= 0);
}

function isTouchedAt(value: unknown): value is number {
  return Number.isInteger(value) && (value as number) >= 0;
}

function isMarker(value: unknown): value is ReadProgressMarker {
  const candidate = value as Record<string, unknown> | null;
  return !!value &&
    typeof value === "object" &&
    candidate !== null &&
    typeof candidate.token === "string" &&
    candidate.token.length > 0 &&
    isOrdinal(candidate.ordinal) &&
    isTouchedAt(candidate.touched_at);
}

function pruneMarkers(
  markers: Record<string, ReadProgressMarker>,
  maxEntries: number,
): Record<string, ReadProgressMarker> {
  const entries = Object.entries(markers);
  if (entries.length <= maxEntries) return markers;
  return Object.fromEntries(
    entries
      .sort((a, b) => b[1].touched_at - a[1].touched_at)
      .slice(0, maxEntries),
  );
}

function readStoredMarkers(maxEntries: number): Record<string, ReadProgressMarker> {
  try {
    const raw = storage()?.getItem(STORAGE_KEY);
    if (!raw) return {};
    const stored = JSON.parse(raw) as {
      version?: unknown;
      sessions?: unknown;
    };
    if (
      stored.version !== STORAGE_VERSION ||
      !stored.sessions ||
      typeof stored.sessions !== "object" ||
      Array.isArray(stored.sessions)
    ) {
      return {};
    }
    const sessions = Object.fromEntries(
      Object.entries(stored.sessions).filter(([, value]) =>
        isMarker(value)
      ),
    ) as Record<string, ReadProgressMarker>;
    return pruneMarkers(sessions, maxEntries);
  } catch {
    return {};
  }
}

export function buildReadProgressToken(source: TokenSource): string | null {
  return source.transcript_revision?.trim() || null;
}

export class ReadProgressStore {
  private markers: Record<string, ReadProgressMarker>;

  constructor(private maxEntries: number = DEFAULT_MAX_ENTRIES) {
    this.markers = $state(readStoredMarkers(maxEntries));
  }

  get(sessionId: string): ReadProgressMarker | null {
    return this.markers[sessionId] ?? null;
  }

  hasUnread(sessionId: string, currentToken: string | null): boolean {
    const marker = this.markers[sessionId];
    return !!marker && !!currentToken && marker.token !== currentToken;
  }

  baseline(
    sessionId: string,
    currentToken: string | null,
    ordinal: number | null,
  ) {
    if (!sessionId || !currentToken || this.markers[sessionId]) return;
    this.set(sessionId, currentToken, ordinal);
  }

  advanceOrdinal(sessionId: string, ordinal: number | null) {
    const marker = this.markers[sessionId];
    if (!marker || ordinal === null) return;
    if (marker.ordinal !== null && ordinal <= marker.ordinal) return;
    this.set(sessionId, marker.token, ordinal);
  }

  markRead(
    sessionId: string,
    currentToken: string | null,
    ordinal: number | null,
  ) {
    if (!sessionId || !currentToken) return;
    const marker = this.markers[sessionId];
    const nextOrdinal = marker?.ordinal !== null &&
        marker?.ordinal !== undefined &&
        ordinal !== null &&
        ordinal < marker.ordinal
      ? marker.ordinal
      : ordinal;
    this.set(sessionId, currentToken, nextOrdinal ?? marker?.ordinal ?? null);
  }

  clear(sessionId: string) {
    if (!this.markers[sessionId]) return;
    const { [sessionId]: _, ...rest } = this.markers;
    this.markers = rest;
    this.persist();
  }

  reset() {
    this.markers = {};
    this.persist();
  }

  private set(sessionId: string, token: string, ordinal: number | null) {
    this.markers = pruneMarkers(
      {
        ...this.markers,
        [sessionId]: {
          token,
          ordinal,
          touched_at: Date.now(),
        },
      },
      this.maxEntries,
    );
    this.persist();
  }

  private persist() {
    try {
      const value: StoredReadProgress = {
        version: STORAGE_VERSION,
        sessions: this.markers,
      };
      storage()?.setItem(STORAGE_KEY, JSON.stringify(value));
    } catch {
      // Storage is optional, keep the in-memory state usable.
    }
  }
}

export const readProgress = new ReadProgressStore();
