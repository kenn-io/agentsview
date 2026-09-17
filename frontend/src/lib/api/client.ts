import { EventSource } from "eventsource";
import { createParser, type EventSourceMessage } from "eventsource-parser";
import type {
  SyncProgress,
  SyncSyncStats as SyncStats,
  DbInsight as Insight,
  GenerateInsightRequest,
} from "./generated/index.js";
import {
  SyncService,
  SessionsService,
  InsightsService,
  ImportService,
  type DbSessionTiming as SessionTiming,
  type ImporterImportStats as ImportStats,
} from "./generated/index.js";
import { ApiError, getAuthToken, getGeneratedBase, isRemoteConnection } from "./runtime.js";

export interface SyncHandle {
  abort: () => void;
  done: Promise<SyncStats>;
}

export async function consumeEvents<T>(
  response: Response,
  dispatch: (event: EventSourceMessage) => T | undefined,
  missingResult: string,
): Promise<T> {
  if (!response.body) throw new Error(missingResult);
  const reader = response.body.getReader();
  const decoder = new TextDecoder();
  let result: T | undefined;
  const parser = createParser({
    onEvent: (event) => {
      if (result === undefined) result = dispatch(event);
    },
  });
  try {
    while (result === undefined) {
      const { done, value } = await reader.read();
      if (done) {
        // The daemon may close immediately after its terminal event.
        parser.feed(decoder.decode() + "\n\n");
        break;
      }
      parser.feed(decoder.decode(value, { stream: true }));
    }
    if (result === undefined) throw new Error(missingResult);
    return result;
  } finally {
    await reader.cancel();
    reader.releaseLock();
  }
}

function streamSyncSSE(
  request: (signal: AbortSignal) => Promise<Response>,
  onProgress?: (p: SyncProgress) => void,
): SyncHandle {
  const controller = new AbortController();
  const done = request(controller.signal).then((response) =>
    consumeEvents<SyncStats>(
      response,
      ({ event, data }) => {
        if (event === "progress") onProgress?.(JSON.parse(data));
        if (event === "done") return JSON.parse(data);
        if (event === "error") throw new Error(JSON.parse(data).error ?? "Sync failed");
      },
      "Sync stream ended without done event",
    ),
  );
  return { abort: () => controller.abort(), done };
}

export function triggerSync(onProgress?: (p: SyncProgress) => void): SyncHandle {
  return streamSyncSSE((signal) => SyncService.postApiV1Sync(undefined, { signal }), onProgress);
}

export function triggerResync(onProgress?: (p: SyncProgress) => void): SyncHandle {
  return streamSyncSSE((signal) => SyncService.postApiV1Resync({ signal }), onProgress);
}

/** Event payload for /api/v1/events data_changed frames. */
export interface DataChangedEvent {
  scope: "messages" | "sessions" | "sync";
}

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
  const url = new URL(
    `${getGeneratedBase()}${SessionsService.getGetApiV1SessionsByIdWatchUrl({ id: sessionId })}`,
    window.location.origin,
  );
  const es = new EventSource(url, {
    fetch: (_url, options) => SessionsService.getApiV1SessionsByIdWatch({ id: sessionId }, options),
  });

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
  const url = new URL(
    `${getGeneratedBase()}${SessionsService.getGetApiV1EventsUrl()}`,
    window.location.origin,
  );
  const es = new EventSource(url, {
    fetch: (_url, options) => SessionsService.getApiV1Events(options),
  });

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
    if (scope === "messages" || scope === "sessions" || scope === "sync") {
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
  return `${getGeneratedBase()}${SessionsService.getGetApiV1SessionsByIdExportUrl({ id: sessionId })}`;
}

/** Get markdown export URL for a session, with optional child depth. */
export function getMarkdownExportUrl(sessionId: string, depth?: 1 | "all"): string {
  const url = new URL(
    `${getGeneratedBase()}${SessionsService.getGetApiV1SessionsByIdMdUrl({ id: sessionId }, { depth: depth === 1 ? "1" : depth })}`,
    window.location.origin,
  );
  if (isRemoteConnection()) {
    return url.toString();
  }
  return `${url.pathname}${url.search}`;
}

/** Download a session export using fetch with auth headers,
 *  avoiding token leakage in the URL for remote connections. */
export async function downloadExport(sessionId: string): Promise<void> {
  await downloadAuthenticatedExport(
    SessionsService.getGetApiV1SessionsByIdExportUrl({ id: sessionId }),
    () => SessionsService.getApiV1SessionsByIdExport({ id: sessionId }),
    `session-${sessionId}.html`,
  );
}

export async function downloadInsightExport(insightId: number): Promise<void> {
  await downloadAuthenticatedExport(
    InsightsService.getGetApiV1InsightsByIdExportUrl({ id: insightId }),
    () => InsightsService.getApiV1InsightsByIdExport({ id: insightId }),
    `insight-${insightId}.html`,
  );
}

async function downloadAuthenticatedExport(
  url: string,
  request: () => Promise<Response>,
  fallbackFilename: string,
): Promise<void> {
  const token = getAuthToken();
  if (!token) {
    // Local connection — simple navigation is fine.
    window.open(`${getGeneratedBase()}${url}`, "_blank");
    return;
  }
  // Remote connection — use fetch with Authorization header
  // to avoid putting the token in the URL.
  const res = await request();
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

  const done = InsightsService.postApiV1InsightsGenerate(req, { signal: controller.signal }).then(
    (response) =>
      consumeEvents<Insight>(
        response,
        ({ event, data }) => {
          if (event === "status") onStatus?.(JSON.parse(data).phase);
          if (event === "log") onLog?.(JSON.parse(data));
          if (event === "done") return JSON.parse(data);
          if (event === "error") throw new Error(JSON.parse(data).message);
        },
        "Generate stream ended without done event",
      ),
  );
  return { abort: () => controller.abort(), done };
}

/* Import */

export interface ImportCallbacks {
  onProgress?: (stats: ImportStats) => void;
  onIndexing?: () => void;
}

async function readImportResponse(response: Response, cb?: ImportCallbacks): Promise<ImportStats> {
  if (!response.headers.get("content-type")?.includes("text/event-stream")) return response.json();
  return consumeEvents<ImportStats>(
    response,
    ({ event, data }) => {
      if (event === "progress") cb?.onProgress?.(JSON.parse(data));
      if (event === "indexing") cb?.onIndexing?.();
      if (event === "done") return JSON.parse(data);
      if (event === "error") throw new Error(JSON.parse(data).error ?? "Import failed");
    },
    "Import stream ended without result",
  );
}

export async function importClaudeAI(file: File, cb?: ImportCallbacks): Promise<ImportStats> {
  return readImportResponse(
    await ImportService.postApiV1ImportClaudeAi(
      { file },
      {
        headers: { Accept: "text/event-stream" },
      },
    ),
    cb,
  );
}

export async function importChatGPT(file: File, cb?: ImportCallbacks): Promise<ImportStats> {
  return readImportResponse(
    await ImportService.postApiV1ImportChatgpt(
      { file },
      {
        headers: { Accept: "text/event-stream" },
      },
    ),
    cb,
  );
}
