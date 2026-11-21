import { type LiveFrame, type LiveRequest } from "./liveHub.js";
import { LiveBroadcast } from "./liveBroadcast.js";
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
  FrictionService,
  type DbSessionTiming as SessionTiming,
  type ImporterImportStats as ImportStats,
} from "./generated/index.js";
import {
  ApiError,
  AUTH_TOKEN_CHANGE_EVENT,
  type ApiRequestOptions,
  getAuthToken,
  getGeneratedBase,
  getServerUrl,
  isRemoteConnection,
} from "./runtime.js";
import { reportTelemetry } from "../utils/telemetry.js";
import type { BrowserHost } from "./browserHost.js";

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
        if (event === "error") {
          // The stream already returned 200, so the failure carries only its code.
          const failure = JSON.parse(data);
          throw Object.assign(new Error(failure.error ?? "Sync failed"), { code: failure.code });
        }
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

export interface LiveSource extends EventTarget {
  readyState: number;
  close(): void;
}

const broadcasts = new Map<string, LiveBroadcast>();

function joinBroadcast(base: string, receive: (frame: LiveFrame) => void) {
  let broadcast = broadcasts.get(base);
  if (!broadcast) {
    broadcast = new LiveBroadcast(base);
    broadcasts.set(base, broadcast);
  }
  return broadcast.connect(receive);
}

function connectLive(session?: string): LiveSource {
  const base = new URL(getGeneratedBase() || "/", window.location.origin).href.replace(/\/$/, "");
  const server = getServerUrl();
  const request: LiveRequest = {
    base,
    token: getAuthToken(server),
    credentialTime: performance.timeOrigin + performance.now(),
    session,
  };
  const target = new EventTarget() as LiveSource;
  target.readyState = 0;
  let disposed = false;
  let send: (value: LiveRequest | "leave" | "close") => void;
  const receive = (frame: LiveFrame) => {
    if (disposed) return;
    if (frame.type === "events_ready" || frame.type === "session_ready") target.readyState = 1;
    if (frame.type === "closed" || frame.type === "permanent_failure") target.readyState = 2;
    target.dispatchEvent(new MessageEvent(frame.type, { data: frame.data }));
  };
  if (typeof SharedWorker !== "undefined") {
    const worker = new SharedWorker(new URL("./live.worker.ts", import.meta.url), {
      type: "module",
      name: base,
    });
    worker.onerror = () => {
      if (disposed) return;
      send = joinBroadcast(base, receive);
      send({ ...request });
    };
    worker.port.onmessage = (event: MessageEvent<LiveFrame>) => receive(event.data);
    send = (value) => worker.port.postMessage(value === "close" ? "leave" : value);
    worker.port.start();
  } else send = joinBroadcast(base, receive);
  const onPageHide = () => send("leave");
  const refreshCredentials = () => {
    const token = getAuthToken(server);
    if (request.token === token) return;
    request.token = token;
    request.credentialTime = performance.timeOrigin + performance.now();
    send({ ...request });
  };
  const onPageShow = () => {
    request.token = getAuthToken(server);
    request.credentialTime = performance.timeOrigin + performance.now();
    send({ ...request });
  };
  window.addEventListener("storage", refreshCredentials);
  window.addEventListener(AUTH_TOKEN_CHANGE_EVENT, refreshCredentials);
  window.addEventListener("pagehide", onPageHide);
  window.addEventListener("pageshow", onPageShow);
  target.close = () => {
    disposed = true;
    send("close");
    target.readyState = 2;
    window.removeEventListener("storage", refreshCredentials);
    window.removeEventListener(AUTH_TOKEN_CHANGE_EVENT, refreshCredentials);
    window.removeEventListener("pagehide", onPageHide);
    window.removeEventListener("pageshow", onPageShow);
  };
  send({ ...request });
  return target;
}

export function watchSession(
  sessionId: string,
  onUpdate: () => void,
  onTiming?: (t: SessionTiming) => void,
): LiveSource {
  const source = connectLive(sessionId);
  source.addEventListener("session_updated", onUpdate);
  if (onTiming)
    source.addEventListener("session.timing", (event) => {
      try {
        onTiming(JSON.parse((event as MessageEvent).data) as SessionTiming);
      } catch (err) {
        console.warn("session.timing parse failed", err);
      }
    });
  return source;
}

export interface WatchEventsOptions {
  onPermanentFailure?: () => void;
}
export function watchEvents(
  onEvent: (e: DataChangedEvent) => void,
  opts: WatchEventsOptions = {},
): LiveSource {
  const source = connectLive();
  source.addEventListener("data_changed", (event) => {
    let scope: unknown;
    try {
      scope = (JSON.parse((event as MessageEvent).data) as { scope?: unknown }).scope;
    } catch {
      /* refresh all */
    }
    onEvent({ scope: scope === "messages" || scope === "sessions" ? scope : "sync" });
  });
  source.addEventListener("permanent_failure", () => opts.onPermanentFailure?.());
  return source;
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
    "html",
  );
}

export async function downloadInsightExport(insightId: number): Promise<void> {
  await downloadAuthenticatedExport(
    InsightsService.getGetApiV1InsightsByIdExportUrl({ id: insightId }),
    () => InsightsService.getApiV1InsightsByIdExport({ id: insightId }),
    `insight-${insightId}.html`,
    "insight_html",
  );
}

/** Download a stored Friction Log digest as Markdown with remote auth headers. */
export async function downloadFrictionDigestMarkdown(date: string): Promise<void> {
  await downloadAuthenticatedExport(
    FrictionService.getGetApiV1FrictionDigestsByDateMdUrl({ date }),
    () => FrictionService.getApiV1FrictionDigestsByDateMd({ date }),
    `friction-log-${date}.md`,
    "friction_markdown",
  );
}

async function downloadAuthenticatedExport(
  url: string,
  request: () => Promise<Response>,
  fallbackFilename: string,
  format: "html" | "insight_html" | "friction_markdown",
): Promise<void> {
  reportTelemetry("export_run", { format });
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
          if (event === "done") {
            const insight: Insight = JSON.parse(data);
            // A cache hit returns a stored insight without running the generator.
            if (insight.cache_status !== "hit") {
              reportTelemetry("insight_generated", {
                kind: req.type === "llm_canned" ? (req.kind ?? req.type) : req.type,
              });
            }
            return insight;
          }
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

async function readImportResponse(
  response: Response,
  cb?: ImportCallbacks,
  onFetch?: (id: string, path: string) => void,
): Promise<ImportStats> {
  if (!response.headers.get("content-type")?.includes("text/event-stream")) return response.json();
  return consumeEvents<ImportStats>(
    response,
    ({ event, data }) => {
      if (event === "fetch") {
        const request = JSON.parse(data);
        onFetch?.(request.id, request.path);
      }
      if (event === "progress") cb?.onProgress?.(JSON.parse(data));
      if (event === "indexing") cb?.onIndexing?.();
      if (event === "done") return JSON.parse(data);
      if (event === "error") {
        const failure = JSON.parse(data);
        throw new ApiError(response.status, failure.error ?? "Import failed", failure.code);
      }
    },
    "Import stream ended without result",
  );
}

// Platforms the Import dialog can Sync through the browser. Each name is the
// platform segment of the Sync route.
export type BrowserSyncPlatform = "claude-ai" | "chatgpt";

// The desktop app relays only Claude.ai requests through its own browser
// window. Every other platform syncs through Chrome, even in the desktop app.
export function relayHost(
  platform: BrowserSyncPlatform,
  host: BrowserHost | undefined,
): BrowserHost | undefined {
  return platform === "claude-ai" ? host : undefined;
}

// The browser window is shared, so Sign in and a new Sync wait until the
// previous run has closed it. A run takes its turn before awaiting anything,
// so a stopped run still finishes closing before a retry starts.
let browserSyncOwner: Promise<void> = Promise.resolve();

export async function connectClaudeAI(host: BrowserHost): Promise<void> {
  await browserSyncOwner;
  await host.connect();
}

export async function syncBrowser(
  platform: BrowserSyncPlatform,
  desktopHost: BrowserHost | undefined,
  cb?: ImportCallbacks,
  signal?: AbortSignal,
): Promise<ImportStats> {
  const host = relayHost(platform, desktopHost);
  // Sync outlives the import dialog, so the selected server can change
  // mid-run. Pin every request to the server the run started on, so a late
  // browser response never relays chat contents to the new server.
  const token = getAuthToken();
  const server: ApiRequestOptions = {
    baseUrl: getGeneratedBase(),
    headers: token ? { Authorization: `Bearer ${token}` } : {},
  };
  const previous = browserSyncOwner;
  let release!: () => void;
  browserSyncOwner = new Promise((resolve) => {
    release = resolve;
  });
  const controller = new AbortController();
  const abort = () => controller.abort();
  signal?.addEventListener("abort", abort, { once: true });
  if (signal?.aborted) abort();
  try {
    await previous;
    const response = await ImportService.postApiV1ImportByPlatformSync(
      { platform },
      host ? undefined : { browser: "chrome" },
      { ...server, signal: controller.signal },
    );
    let fail: (error: unknown) => void = () => {};
    const failed = new Promise<never>((_, reject) => {
      fail = reject;
    });
    const result = readImportResponse(
      response,
      cb,
      host
        ? (id, path) => {
            void (async () => {
              let fetched;
              try {
                fetched = await host.fetch(path);
              } catch (error) {
                fetched = { status: 0, body: "", error: String(error) };
              }
              await ImportService.postApiV1ImportClaudeAiSyncResultsById(
                { id },
                {
                  status: fetched.status,
                  body: fetched.body,
                  retry_after: fetched.retryAfter,
                  error: fetched.error,
                },
                { ...server, signal: controller.signal },
              );
            })().catch((error) => {
              fail(error);
              abort();
            });
          }
        : undefined,
    );
    return await Promise.race([result, failed]);
  } finally {
    signal?.removeEventListener("abort", abort);
    abort();
    // Cleanup failure must preserve the sync outcome.
    if (host)
      void host
        .close()
        .catch(() => {})
        .finally(release);
    else release();
  }
}

export async function importClaudeAI(file: File, cb?: ImportCallbacks): Promise<ImportStats> {
  return readImportResponse(
    await ImportService.postApiV1ImportClaudeAi({ file }, undefined, {
      headers: { Accept: "text/event-stream" },
    }),
    cb,
  );
}

export async function importChatGPT(file: File, cb?: ImportCallbacks): Promise<ImportStats> {
  return readImportResponse(
    await ImportService.postApiV1ImportChatgpt({ file }, undefined, {
      headers: { Accept: "text/event-stream" },
    }),
    cb,
  );
}
