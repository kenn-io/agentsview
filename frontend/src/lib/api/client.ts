import type {
  SyncProgress,
  SyncStats,
  Insight,
  GenerateInsightRequest,
} from "./types.js";
import type { SessionTiming } from "./types/timing.js";
import {
  ApiError,
  authHeaders,
  getAuthToken,
  getBase,
  isRemoteConnection,
  responseErrorMessage,
} from "./runtime.js";

export interface SyncHandle {
  abort: () => void;
  done: Promise<SyncStats>;
}

function streamSyncSSE(
  path: string,
  onProgress?: (p: SyncProgress) => void,
): SyncHandle {
  const controller = new AbortController();

  const done = (async () => {
    const res = await fetch(`${getBase()}${path}`, authHeaders({
      method: "POST",
      signal: controller.signal,
    }));

    if (!res.ok || !res.body) {
      throw new Error(`Sync request failed: ${res.status}`);
    }

    const reader = res.body.getReader();
    const decoder = new TextDecoder();
    let buf = "";
    let stats: SyncStats | undefined;

    for (;;) {
      const { done: eof, value } = await reader.read();
      if (eof) break;
      buf += decoder.decode(value, { stream: true });
      buf = buf.replaceAll("\r\n", "\n");

      const result = processFrames(buf, onProgress);
      if (result) {
        stats = result;
        reader.cancel();
        break;
      }
      const last = buf.lastIndexOf("\n\n");
      if (last !== -1) buf = buf.slice(last + 2);
    }

    // Flush any remaining multibyte bytes from decoder
    buf += decoder.decode();

    if (!stats && buf.trim()) {
      stats = processFrame(buf, onProgress);
    }

    if (!stats) {
      throw new Error("Sync stream ended without done event");
    }

    return stats;
  })();

  return { abort: () => controller.abort(), done };
}

export function triggerSync(
  onProgress?: (p: SyncProgress) => void,
): SyncHandle {
  return streamSyncSSE("/sync", onProgress);
}

export function triggerResync(
  onProgress?: (p: SyncProgress) => void,
): SyncHandle {
  return streamSyncSSE("/resync", onProgress);
}

/**
 * Parse all complete SSE frames in buf.
 * Returns the SyncStats if a "done" event was received, undefined otherwise.
 */
function processFrames(
  buf: string,
  onProgress?: (p: SyncProgress) => void,
): SyncStats | undefined {
  let idx: number;
  let start = 0;
  while ((idx = buf.indexOf("\n\n", start)) !== -1) {
    const frame = buf.slice(start, idx);
    start = idx + 2;
    const stats = processFrame(frame, onProgress);
    if (stats) return stats;
  }
  return undefined;
}

/**
 * Dispatch a single SSE frame.
 * Returns the SyncStats if it was a "done" event, undefined otherwise.
 */
function processFrame(
  frame: string,
  onProgress?: (p: SyncProgress) => void,
): SyncStats | undefined {
  let event = "";
  const dataLines: string[] = [];
  for (const line of frame.split("\n")) {
    if (line.startsWith("event: ")) {
      event = line.slice(7);
    } else if (line.startsWith("data: ")) {
      dataLines.push(line.slice(6));
    } else if (line.startsWith("data:")) {
      dataLines.push(line.slice(5));
    }
  }
  const data = dataLines.join("\n");
  if (!data) return undefined;

  if (event === "progress") {
    onProgress?.(JSON.parse(data) as SyncProgress);
  } else if (event === "done") {
    return JSON.parse(data) as SyncStats;
  }
  return undefined;
}

/** Event payload for /api/v1/events data_changed frames. */
export interface DataChangedEvent {
  scope: "messages" | "sessions" | "sync";
}

/** Watch a session for live updates via SSE.
 *
 * SECURITY NOTE: The native EventSource API does not support custom
 * headers, so the auth token is passed as a query parameter for
 * remote connections. This means the token may appear in browser
 * history and proxy/server access logs. This is an accepted
 * limitation of SSE — switching to a fetch-based streaming
 * approach would avoid this but adds significant complexity.
 */
/** Number of consecutive onerror firings without a successful
 * connection or event delivery before watchSession gives up. Guards
 * against the browser hammering `/watch` forever when the session
 * id is unknown (server returns 404 per the Session API contract)
 * or the server is permanently refusing the stream. */
export const WATCH_SESSION_MAX_CONSECUTIVE_ERRORS = 5;

export function watchSession(
  sessionId: string,
  onUpdate: () => void,
  onTiming?: (t: SessionTiming) => void,
): EventSource {
  const url = `${getBase()}/sessions/${sessionId}/watch`;
  const token = getAuthToken();
  // EventSource does not support custom headers, so pass the
  // auth token as a query parameter for remote connections.
  const fullUrl = token ? `${url}?token=${encodeURIComponent(token)}` : url;
  const es = new EventSource(fullUrl);

  // Circuit breaker: mirrors watchEvents. A 404 (unknown session)
  // or other permanent failure would otherwise have EventSource
  // reconnect in a loop. Counter resets on `open` or a delivered
  // event so a healthy-but-quiet stream isn't tripped.
  let consecutiveErrors = 0;

  es.addEventListener("open", () => {
    consecutiveErrors = 0;
  });

  es.addEventListener("session_updated", () => {
    consecutiveErrors = 0;
    onUpdate();
  });

  if (onTiming) {
    es.addEventListener("session.timing", (ev: MessageEvent) => {
      try {
        onTiming(JSON.parse(ev.data) as SessionTiming);
      } catch (err) {
        console.warn("session.timing parse failed", err);
      }
    });
  }

  es.onerror = () => {
    consecutiveErrors += 1;
    if (consecutiveErrors >= WATCH_SESSION_MAX_CONSECUTIVE_ERRORS) {
      es.close();
    }
  };

  return es;
}

/** Watch the global sync event stream via SSE.
 *
 * Returns the underlying EventSource so callers can close() it
 * when done. The browser's native EventSource auto-reconnects
 * on transient errors; in PG serve mode the endpoint returns
 * 503 and the browser will retry at its default interval.
 *
 * SECURITY NOTE: Same as watchSession — EventSource cannot set
 * headers, so the auth token is passed as a query parameter
 * for remote connections. This may leak the token into browser
 * history / access logs; accepted per the project threat model.
 */
/** Number of consecutive onerror firings without any successful
 * event delivery before watchEvents gives up and closes the
 * underlying EventSource. This protects PG serve mode — where
 * /api/v1/events returns 503 permanently — from turning into a
 * forever retry loop in the browser.
 */
export const WATCH_EVENTS_MAX_CONSECUTIVE_ERRORS = 5;

export interface WatchEventsOptions {
  /** Called once when the circuit breaker trips WITHOUT the
   * EventSource ever having reached the OPEN state. That pattern
   * indicates the endpoint is permanently unreachable for this
   * client (PG serve mode returning 503, incompatible server
   * build, wrong URL, etc.), so callers should stop retrying.
   * Transient failures — where `open` fired at least once before
   * the breaker tripped — do not call this, letting callers
   * recover on their own.
   */
  onPermanentFailure?: () => void;
}

export function watchEvents(
  onEvent: (e: DataChangedEvent) => void,
  opts: WatchEventsOptions = {},
): EventSource {
  const url = `${getBase()}/events`;
  const token = getAuthToken();
  const fullUrl = token
    ? `${url}?token=${encodeURIComponent(token)}`
    : url;
  const es = new EventSource(fullUrl);

  // Circuit breaker: on N consecutive onerror firings without any
  // successful connection or event delivery, close the stream.
  // The counter resets on both `open` (a successful (re)connect)
  // and a delivered `data_changed` event, so a quiet but healthy
  // stream isn't tripped by transient network blips.
  //
  // `hasOpened` distinguishes "never worked" (permanent failure,
  // e.g. PG serve 503) from "worked once, then failed" (transient
  // outage). Permanent failures invoke onPermanentFailure so the
  // caller can stop retrying.
  let consecutiveErrors = 0;
  let hasOpened = false;

  es.addEventListener("open", () => {
    hasOpened = true;
    consecutiveErrors = 0;
  });

  es.addEventListener("data_changed", (msg) => {
    // Successful delivery also resets the circuit breaker.
    consecutiveErrors = 0;
    hasOpened = true;
    // Parse and shape-check the payload. Anything that isn't an
    // object with a known scope collapses to a safe refresh signal
    // so subscribers never observe scope === undefined.
    let parsed: unknown;
    try {
      parsed = JSON.parse((msg as MessageEvent).data);
    } catch {
      onEvent({ scope: "sync" });
      return;
    }
    const scope =
      typeof parsed === "object" && parsed !== null
        ? (parsed as { scope?: unknown }).scope
        : undefined;
    if (
      scope === "messages" ||
      scope === "sessions" ||
      scope === "sync"
    ) {
      onEvent({ scope });
    } else {
      onEvent({ scope: "sync" });
    }
  });

  es.onerror = () => {
    consecutiveErrors += 1;
    if (consecutiveErrors >= WATCH_EVENTS_MAX_CONSECUTIVE_ERRORS) {
      es.close();
      if (!hasOpened && opts.onPermanentFailure) {
        opts.onPermanentFailure();
      }
    }
  };

  return es;
}

/** Get the export URL for a session.
 *
 * For authenticated remote connections, triggers a fetch-based
 * download with the Authorization header instead of leaking the
 * token in the URL query string.
 */
export function getExportUrl(sessionId: string): string {
  return `${getBase()}/sessions/${sessionId}/export`;
}

export function getInsightExportUrl(insightId: number): string {
  return `${getBase()}/insights/${insightId}/export`;
}

/** Get markdown export URL for a session, with optional child depth. */
export function getMarkdownExportUrl(
  sessionId: string,
  depth?: 1 | "all",
): string {
  const url = new URL(
    `${getBase()}/sessions/${sessionId}/md`,
    window.location.origin,
  );
  if (depth !== undefined) {
    url.searchParams.set("depth", String(depth));
  }
  if (isRemoteConnection()) {
    return url.toString();
  }
  return `${url.pathname}${url.search}`;
}

export function getInsightMarkdownExportUrl(
  insightId: number,
): string {
  return `${getBase()}/insights/${insightId}/md`;
}

/** Download a session export using fetch with auth headers,
 *  avoiding token leakage in the URL for remote connections. */
export async function downloadExport(sessionId: string): Promise<void> {
  await downloadAuthenticatedExport(
    getExportUrl(sessionId),
    `session-${sessionId}.html`,
  );
}

export async function downloadInsightExport(
  insightId: number,
): Promise<void> {
  await downloadAuthenticatedExport(
    getInsightExportUrl(insightId),
    `insight-${insightId}.html`,
  );
}

async function downloadAuthenticatedExport(
  url: string,
  fallbackFilename: string,
): Promise<void> {
  const token = getAuthToken();
  if (!token) {
    // Local connection — simple navigation is fine.
    window.open(url, "_blank");
    return;
  }
  // Remote connection — use fetch with Authorization header
  // to avoid putting the token in the URL.
  const res = await fetch(url, authHeaders());
  if (!res.ok) {
    throw new ApiError(res.status, `Export failed: ${res.status}`);
  }
  const blob = await res.blob();
  const blobUrl = URL.createObjectURL(blob);
  const a = document.createElement("a");
  a.href = blobUrl;
  // Extract filename from Content-Disposition if available.
  const cd = res.headers.get("Content-Disposition");
  const match = cd?.match(/filename="?([^"]+)"?/);
  a.download = match?.[1] ?? fallbackFilename;
  document.body.appendChild(a);
  a.click();
  document.body.removeChild(a);
  URL.revokeObjectURL(blobUrl);
}

export interface GenerateInsightHandle {
  abort: () => void;
  done: Promise<Insight>;
}

export interface InsightLogEvent {
  stream: "stdout" | "stderr";
  line: string;
}

export function generateInsight(
  req: GenerateInsightRequest,
  onStatus?: (phase: string) => void,
  onLog?: (event: InsightLogEvent) => void,
): GenerateInsightHandle {
  const controller = new AbortController();

  const done = (async () => {
    const res = await fetch(`${getBase()}/insights/generate`, authHeaders({
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(req),
      signal: controller.signal,
    }));

    if (!res.ok) {
      throw new ApiError(res.status, await responseErrorMessage(res));
    }
    if (!res.body) {
      throw new Error("Generate request failed: empty response");
    }

    const reader = res.body.getReader();
    const decoder = new TextDecoder();
    let buf = "";
    let result: Insight | undefined;

    for (;;) {
      const { done: eof, value } = await reader.read();
      if (eof) break;
      buf += decoder.decode(value, { stream: true });
      buf = buf.replaceAll("\r\n", "\n");

      const parsed = processInsightFrames(buf, onStatus, onLog);
      buf = parsed.remaining;
      if (parsed.result) {
        result = parsed.result;
        reader.cancel();
        break;
      }
    }

    // Flush any remaining multibyte bytes from decoder
    buf += decoder.decode();

    if (!result && buf.trim()) {
      result = processInsightFrame(buf, onStatus, onLog);
    }

    if (!result) {
      throw new Error("Generate stream ended without done event");
    }

    return result;
  })();

  return { abort: () => controller.abort(), done };
}

function processInsightFrames(
  buf: string,
  onStatus?: (phase: string) => void,
  onLog?: (event: InsightLogEvent) => void,
): { result?: Insight; remaining: string } {
  let idx: number;
  let start = 0;
  while ((idx = buf.indexOf("\n\n", start)) !== -1) {
    const frame = buf.slice(start, idx);
    start = idx + 2;
    const result = processInsightFrame(frame, onStatus, onLog);
    if (result) {
      return { result, remaining: buf.slice(start) };
    }
  }
  return { remaining: buf.slice(start) };
}

function processInsightFrame(
  frame: string,
  onStatus?: (phase: string) => void,
  onLog?: (event: InsightLogEvent) => void,
): Insight | undefined {
  let event = "";
  const dataLines: string[] = [];
  for (const line of frame.split("\n")) {
    if (line.startsWith("event: ")) {
      event = line.slice(7);
    } else if (line.startsWith("data: ")) {
      dataLines.push(line.slice(6));
    } else if (line.startsWith("data:")) {
      dataLines.push(line.slice(5));
    }
  }
  const data = dataLines.join("\n");
  if (!data) return undefined;

  if (event === "status") {
    const parsed = JSON.parse(data) as { phase: string };
    onStatus?.(parsed.phase);
  } else if (event === "log") {
    const parsed = JSON.parse(data) as InsightLogEvent;
    onLog?.(parsed);
  } else if (event === "done") {
    return JSON.parse(data) as Insight;
  } else if (event === "error") {
    const parsed = JSON.parse(data) as { message: string };
    throw new Error(parsed.message);
  }
  return undefined;
}

/* Import */

export interface ImportStats {
  imported: number;
  updated: number;
  skipped: number;
  errors: number;
}

export interface ImportCallbacks {
  onProgress?: (stats: ImportStats) => void;
  onIndexing?: () => void;
}

async function readImportSSE(
  res: Response,
  cb?: ImportCallbacks,
): Promise<ImportStats> {
  const reader = res.body!.getReader();
  const decoder = new TextDecoder();
  let buf = "";
  let result: ImportStats | null = null;

  for (;;) {
    const { done, value } = await reader.read();
    if (done) break;
    buf += decoder.decode(value, { stream: true });

    // Process complete SSE frames (double newline delimited).
    let idx: number;
    while ((idx = buf.indexOf("\n\n")) !== -1) {
      const frame = buf.slice(0, idx);
      buf = buf.slice(idx + 2);

      let event = "";
      let data = "";
      for (const line of frame.split("\n")) {
        if (line.startsWith("event: ")) event = line.slice(7);
        else if (line.startsWith("data: ")) data = line.slice(6);
      }
      if (!event || !data) continue;

      const parsed = JSON.parse(data);
      switch (event) {
        case "progress":
          cb?.onProgress?.(parsed as ImportStats);
          break;
        case "indexing":
          cb?.onIndexing?.();
          break;
        case "done":
          result = parsed as ImportStats;
          break;
        case "error":
          throw new Error(
            (parsed as { error?: string }).error
            ?? "Import failed",
          );
      }
    }
  }

  if (!result) throw new Error("Import stream ended without result");
  return result;
}

export async function importClaudeAI(
  file: File,
  cb?: ImportCallbacks,
): Promise<ImportStats> {
  const form = new FormData();
  form.append("file", file);
  const init = authHeaders({ method: "POST", body: form });
  const headers = new Headers(init.headers);
  headers.set("Accept", "text/event-stream");
  const res = await fetch(
    `${getBase()}/import/claude-ai`,
    { ...init, headers },
  );
  if (!res.ok) {
    const err = await res.json().catch(() => ({}));
    throw new Error(
      (err as { error?: string }).error
      ?? `Import failed (${res.status})`,
    );
  }
  if (res.headers.get("content-type")?.includes("text/event-stream")) {
    return readImportSSE(res, cb);
  }
  return res.json();
}

export async function importChatGPT(
  file: File,
  cb?: ImportCallbacks,
): Promise<ImportStats> {
  const form = new FormData();
  form.append("file", file);
  const init = authHeaders({ method: "POST", body: form });
  const headers = new Headers(init.headers);
  headers.set("Accept", "text/event-stream");
  const res = await fetch(
    `${getBase()}/import/chatgpt`,
    { ...init, headers },
  );
  if (!res.ok) {
    const err = await res.json().catch(() => ({}));
    throw new Error(
      (err as { error?: string }).error
      ?? `Import failed (${res.status})`,
    );
  }
  if (res.headers.get("content-type")?.includes("text/event-stream")) {
    return readImportSSE(res, cb);
  }
  return res.json();
}
