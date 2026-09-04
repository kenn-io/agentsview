export type PerfEntryKind = "api" | "panel";
export type PerfEntryStatus = "ok" | "error" | "aborted" | number;

export interface PerfEntry {
  id: number;
  kind: PerfEntryKind;
  name: string;
  route: string;
  durationMs: number;
  status: PerfEntryStatus;
  at: number;
  method?: string;
  path?: string;
  sizeBytes?: number;
}

interface PerfStoreOptions {
  maxEntries?: number;
}

type ApiTiming = {
  method: string;
  path: string;
  status: number | "error";
  durationMs: number;
  route: string;
  sizeBytes?: number;
};

type PanelTiming = {
  name: string;
  route: string;
  durationMs: number;
  status: "ok" | "error" | "aborted";
};

class PerfStore {
  entries: PerfEntry[] = $state([]);
  panelOpen = $state(false);
  enabled = $state(true);

  private nextId = 1;
  private readonly maxEntries: number;

  constructor(options: PerfStoreOptions = {}) {
    this.maxEntries = Math.max(1, options.maxEntries ?? 200);
  }

  recordApi(timing: ApiTiming): void {
    this.record({
      kind: "api",
      name: apiName(timing.path),
      route: timing.route,
      durationMs: timing.durationMs,
      status: timing.status,
      method: timing.method,
      path: timing.path,
      sizeBytes: timing.sizeBytes,
    });
  }

  recordPanel(timing: PanelTiming): void {
    this.record({
      kind: "panel",
      name: timing.name,
      route: timing.route,
      durationMs: timing.durationMs,
      status: timing.status,
    });
  }

  clear(): void {
    this.entries = [];
  }

  togglePanel(): void {
    this.panelOpen = !this.panelOpen;
  }

  private record(entry: Omit<PerfEntry, "id" | "at">): void {
    if (!this.enabled) return;
    this.entries = [
      {
        ...entry,
        id: this.nextId++,
        at: Date.now(),
      },
      ...this.entries,
    ].slice(0, this.maxEntries);
  }
}

let fetchInstrumentationInstalled = false;

export function installPerfFetchInstrumentation(): void {
  if (fetchInstrumentationInstalled) return;
  if (typeof globalThis.fetch !== "function") return;
  fetchInstrumentationInstalled = true;

  const originalFetch = globalThis.fetch.bind(globalThis);
  globalThis.fetch = async (input, init) => {
    const request = requestInfo(input);
    const started = performance.now();
    try {
      const response = await originalFetch(input, init);
      const path = apiPath(request.url);
      if (path) {
        perf.recordApi({
          method: init?.method ?? request.method,
          path,
          status: response.status,
          durationMs: performance.now() - started,
          route: currentRoute(),
          sizeBytes: contentLength(response),
        });
      }
      return response;
    } catch (e) {
      const path = apiPath(request.url);
      if (path) {
        perf.recordApi({
          method: init?.method ?? request.method,
          path,
          status: "error",
          durationMs: performance.now() - started,
          route: currentRoute(),
        });
      }
      throw e;
    }
  };
}

function requestInfo(input: RequestInfo | URL): {
  url: string;
  method: string;
} {
  if (typeof input === "string") {
    return { url: input, method: "GET" };
  }
  if (input instanceof URL) {
    return { url: input.toString(), method: "GET" };
  }
  if (typeof Request !== "undefined" && input instanceof Request) {
    return { url: input.url, method: input.method };
  }
  return { url: String(input), method: "GET" };
}

function apiPath(url: string): string | null {
  try {
    const base = typeof window !== "undefined" ? window.location.origin : "http://localhost";
    const parsed = new URL(url, base);
    return parsed.pathname.startsWith("/api/") ? parsed.pathname : null;
  } catch {
    return url.startsWith("/api/") ? url : null;
  }
}

function currentRoute(): string {
  if (typeof window === "undefined") return "unknown";
  const first = window.location.pathname.split("/").filter(Boolean)[0];
  return first || "sessions";
}

function contentLength(response: Response): number | undefined {
  const raw = response.headers.get("content-length");
  if (!raw) return undefined;
  const value = Number.parseInt(raw, 10);
  return Number.isFinite(value) ? value : undefined;
}

function apiName(path: string): string {
  return path.replace(/^\/api\/v1\//, "").replace(/\/[0-9a-f-]{16,}/gi, "/:id");
}

export function createPerfStore(options: PerfStoreOptions = {}): PerfStore {
  return new PerfStore(options);
}

export const perf = createPerfStore();
