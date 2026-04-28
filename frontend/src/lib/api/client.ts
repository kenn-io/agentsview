import type {
  SessionPage,
  Session,
  MessagesResponse,
  SearchResponse,
  ProjectsResponse,
  MachinesResponse,
  AgentsResponse,
  Stats,
  VersionInfo,
  UpdateCheck,
  SyncStatus,
  SyncProgress,
  SyncStats,
  PublishResponse,
  GithubConfig,
  SetGithubConfigResponse,
  AnalyticsSummary,
  ActivityResponse,
  HeatmapResponse,
  ProjectsAnalyticsResponse,
  HourOfWeekResponse,
  SessionShapeResponse,
  VelocityResponse,
  ToolsAnalyticsResponse,
  TopSessionsResponse,
  SignalsAnalyticsResponse,
  Granularity,
  HeatmapMetric,
  TopSessionsMetric,
  TrendsGranularity,
  TrendsTermsResponse,
  Insight,
  InsightsResponse,
  GenerateInsightRequest,
  PinsResponse,
  TrashResponse,
  UsageSummaryResponse,
  TopUsageSessionsResponse,
  UsageParams,
  UsageTopSessionsParams,
} from "./types.js";
import type { SessionActivityResponse } from "./types/session-activity.js";
import type { SessionTiming } from "./types/timing.js";

const SERVER_URL_KEY = "agentsview-server-url";
const AUTH_TOKEN_KEY = "agentsview-auth-token";

export function getBase(): string {
  const server = getServerUrl();
  if (server) return `${server}/api/v1`;
  // Use the <base href> tag injected by --base-path so the app
  // works behind a reverse-proxy subpath (e.g. /agentsview/api/v1).
  // Only derive from baseURI when a real <base> tag exists;
  // otherwise fall back to "/api/v1" so SPA fallback pages on
  // non-root URLs don't produce wrong API paths.
  const baseEl = document.querySelector("base[href]");
  if (baseEl) {
    const base = new URL(document.baseURI).pathname.replace(/\/$/, "");
    return `${base}/api/v1`;
  }
  return "/api/v1";
}

export function getServerUrl(): string {
  return localStorage.getItem(SERVER_URL_KEY) ?? "";
}

export function setServerUrl(url: string): void {
  if (url) {
    localStorage.setItem(SERVER_URL_KEY, url);
  } else {
    localStorage.removeItem(SERVER_URL_KEY);
  }
}

/** Return the localStorage key for the auth token, scoped by server URL. */
function authTokenKey(): string {
  const server = getServerUrl();
  return server ? `${AUTH_TOKEN_KEY}::${server}` : AUTH_TOKEN_KEY;
}

export function getAuthToken(): string {
  return localStorage.getItem(authTokenKey()) ?? "";
}

export function setAuthToken(token: string): void {
  const key = authTokenKey();
  if (token) {
    localStorage.setItem(key, token);
  } else {
    localStorage.removeItem(key);
  }
}

export function isRemoteConnection(): boolean {
  return getServerUrl() !== "";
}

function authHeaders(init?: RequestInit): RequestInit {
  const token = getAuthToken();
  if (!token) return init ?? {};

  const headers = new Headers(init?.headers);
  headers.set("Authorization", `Bearer ${token}`);
  return { ...init, headers };
}

export class ApiError extends Error {
  constructor(
    public readonly status: number,
    message: string,
  ) {
    super(message);
    this.name = "ApiError";
  }
}

function apiErrorMessage(status: number, body: string): string {
  return body.trim() || `API ${status}`;
}

async function fetchJSON<T>(path: string, init?: RequestInit): Promise<T> {
  const res = await fetch(`${getBase()}${path}`, authHeaders(init));
  if (!res.ok) {
    const body = await res.text();
    throw new ApiError(res.status, apiErrorMessage(res.status, body));
  }
  return res.json() as Promise<T>;
}

type QueryValue = string | number | boolean | undefined | null;

function buildQuery(params: Record<string, QueryValue>): string {
  const q = new URLSearchParams();
  for (const [key, value] of Object.entries(params)) {
    if (value !== undefined && value !== null && value !== "") {
      q.set(key, String(value));
    }
  }
  const qs = q.toString();
  return qs ? `?${qs}` : "";
}

/* Sessions */

export interface ListSessionsParams {
  project?: string;
  exclude_project?: string;
  machine?: string;
  agent?: string;
  termination?: string;
  date?: string;
  date_from?: string;
  date_to?: string;
  active_since?: string;
  min_messages?: number;
  max_messages?: number;
  min_user_messages?: number;
  include_one_shot?: boolean;
  include_automated?: boolean;
  include_children?: boolean;
  cursor?: string;
  limit?: number;
}

export function listSessions(
  params: ListSessionsParams = {},
): Promise<SessionPage> {
  return fetchJSON(`/sessions${buildQuery({ ...params })}`);
}

export function getSession(id: string, init?: RequestInit): Promise<Session> {
  return fetchJSON(`/sessions/${id}`, init);
}

export function getChildSessions(
  id: string,
  init?: RequestInit,
): Promise<Session[]> {
  return fetchJSON(`/sessions/${id}/children`, init);
}

export function getSessionActivity(
  sessionId: string,
): Promise<SessionActivityResponse> {
  return fetchJSON(
    `/sessions/${sessionId}/activity`,
  );
}

/* Messages */

export interface GetMessagesParams {
  from?: number;
  limit?: number;
  direction?: "asc" | "desc";
}

export function getMessages(
  sessionId: string,
  params: GetMessagesParams = {},
  init?: RequestInit,
): Promise<MessagesResponse> {
  return fetchJSON(
    `/sessions/${sessionId}/messages${buildQuery({ ...params })}`,
    init,
  );
}

/* Search */

export function search(
  query: string,
  params: {
    project?: string;
    limit?: number;
    cursor?: number;
    sort?: "relevance" | "recency";
  } = {},
  init?: RequestInit,
): Promise<SearchResponse> {
  if (!query) {
    throw new Error("search query must not be empty");
  }
  return fetchJSON(`/search${buildQuery({ q: query, ...params })}`, init);
}

export interface SessionSearchResponse {
  ordinals: number[];
}

export function searchSession(
  sessionId: string,
  query: string,
  init?: RequestInit,
): Promise<SessionSearchResponse> {
  return fetchJSON(
    `/sessions/${sessionId}/search${buildQuery({ q: query })}`,
    init,
  );
}

/* Metadata */

interface MetadataParams {
  include_one_shot?: boolean;
  include_automated?: boolean;
}

export function getProjects(
  params: MetadataParams = {},
): Promise<ProjectsResponse> {
  return fetchJSON(`/projects${buildQuery({ ...params })}`);
}

export function getMachines(
  params: MetadataParams = {},
): Promise<MachinesResponse> {
  return fetchJSON(`/machines${buildQuery({ ...params })}`);
}

export function getAgents(
  params: MetadataParams = {},
): Promise<AgentsResponse> {
  return fetchJSON(`/agents${buildQuery({ ...params })}`);
}

export function getStats(
  params: MetadataParams = {},
): Promise<Stats> {
  return fetchJSON(`/stats${buildQuery({ ...params })}`);
}

export function getVersion(): Promise<VersionInfo> {
  return fetchJSON("/version");
}

export function checkForUpdate(): Promise<UpdateCheck> {
  return fetchJSON("/update/check");
}

/* Sync */

export function getSyncStatus(): Promise<SyncStatus> {
  return fetchJSON("/sync/status");
}

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
  return `${url.pathname}${url.search}`;
}

/** Download a session export using fetch with auth headers,
 *  avoiding token leakage in the URL for remote connections. */
export async function downloadExport(sessionId: string): Promise<void> {
  const url = getExportUrl(sessionId);
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
  a.download = match?.[1] ?? `session-${sessionId}.md`;
  document.body.appendChild(a);
  a.click();
  document.body.removeChild(a);
  URL.revokeObjectURL(blobUrl);
}

/* Resume in terminal */

export interface ResumeRequest {
  skip_permissions?: boolean;
  fork_session?: boolean;
  command_only?: boolean;
  opener_id?: string;
}

export interface ResumeResponse {
  launched: boolean;
  terminal?: string;
  command: string;
  cwd?: string;
  error?: string;
}

export function resumeSession(
  sessionId: string,
  flags: ResumeRequest = {},
): Promise<ResumeResponse> {
  return fetchJSON(`/sessions/${sessionId}/resume`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(flags),
  });
}

/* Publish / GitHub config */

export function publishSession(sessionId: string): Promise<PublishResponse> {
  return fetchJSON(`/sessions/${sessionId}/publish`, {
    method: "POST",
  });
}

export function getGithubConfig(): Promise<GithubConfig> {
  return fetchJSON("/config/github");
}

export function setGithubConfig(
  token: string,
): Promise<SetGithubConfigResponse> {
  return fetchJSON("/config/github", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ token }),
  });
}

/* Starred */

export async function listStarred(): Promise<{ session_ids: string[] }> {
  return fetchJSON("/starred");
}

export async function starSession(id: string): Promise<void> {
  const res = await fetch(`${getBase()}/sessions/${id}/star`, authHeaders({
    method: "PUT",
  }));
  if (!res.ok) {
    const body = await res.text();
    throw new ApiError(res.status, apiErrorMessage(res.status, body));
  }
}

export async function unstarSession(id: string): Promise<void> {
  const res = await fetch(`${getBase()}/sessions/${id}/star`, authHeaders({
    method: "DELETE",
  }));
  if (!res.ok) {
    const body = await res.text();
    throw new ApiError(res.status, apiErrorMessage(res.status, body));
  }
}

export async function bulkStarSessions(
  sessionIds: string[],
): Promise<void> {
  const res = await fetch(`${getBase()}/starred/bulk`, authHeaders({
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ session_ids: sessionIds }),
  }));
  if (!res.ok) {
    const body = await res.text();
    throw new ApiError(res.status, apiErrorMessage(res.status, body));
  }
}

/* Session directory */

export function getSessionDirectory(
  sessionId: string,
): Promise<{ path: string }> {
  return fetchJSON(`/sessions/${sessionId}/directory`);
}

/* Openers — Conductor-style "Open in" */

export interface Opener {
  id: string;
  name: string;
  kind: "editor" | "terminal" | "files" | "action";
  bin: string;
}

export interface OpenersResponse {
  openers: Opener[];
}

export function listOpeners(): Promise<OpenersResponse> {
  return fetchJSON("/openers");
}

export interface OpenResponse {
  launched: boolean;
  opener: string;
  path: string;
}

export function openSession(
  sessionId: string,
  openerId: string,
): Promise<OpenResponse> {
  return fetchJSON(`/sessions/${sessionId}/open`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ opener_id: openerId }),
  });
}

/* Terminal config */

export interface TerminalConfig {
  mode: "auto" | "custom" | "clipboard";
  custom_bin?: string;
  custom_args?: string;
}

export function getTerminalConfig(): Promise<TerminalConfig> {
  return fetchJSON("/config/terminal");
}

export function setTerminalConfig(
  cfg: TerminalConfig,
): Promise<TerminalConfig> {
  return fetchJSON("/config/terminal", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(cfg),
  });
}

/* Settings */

export interface AppSettings {
  agent_dirs: Record<string, string[]>;
  terminal: TerminalConfig;
  github_configured: boolean;
  host: string;
  port: number;
  auth_token?: string;
  require_auth?: boolean;
}

export function getSettings(): Promise<AppSettings> {
  return fetchJSON("/settings");
}

export function updateSettings(
  patch: Partial<AppSettings>,
): Promise<AppSettings> {
  return fetchJSON("/settings", {
    method: "PUT",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(patch),
  });
}

/* Analytics */

export interface AnalyticsParams {
  from?: string;
  to?: string;
  timezone?: string;
  machine?: string;
  project?: string;
  agent?: string;
  dow?: number;
  hour?: number;
  min_user_messages?: number;
  include_one_shot?: boolean;
  include_automated?: boolean;
  active_since?: string;
  termination?: string;
}

export function getAnalyticsSummary(
  params: AnalyticsParams,
): Promise<AnalyticsSummary> {
  return fetchJSON(`/analytics/summary${buildQuery({ ...params })}`);
}

export function getAnalyticsActivity(
  params: AnalyticsParams & {
    granularity?: Granularity;
  },
): Promise<ActivityResponse> {
  return fetchJSON(`/analytics/activity${buildQuery({ ...params })}`);
}

export function getAnalyticsHeatmap(
  params: AnalyticsParams & {
    metric?: HeatmapMetric;
  },
): Promise<HeatmapResponse> {
  return fetchJSON(`/analytics/heatmap${buildQuery({ ...params })}`);
}

export function getAnalyticsProjects(
  params: AnalyticsParams,
): Promise<ProjectsAnalyticsResponse> {
  return fetchJSON(`/analytics/projects${buildQuery({ ...params })}`);
}

export function getAnalyticsHourOfWeek(
  params: AnalyticsParams,
): Promise<HourOfWeekResponse> {
  return fetchJSON(`/analytics/hour-of-week${buildQuery({ ...params })}`);
}

export function getAnalyticsSessionShape(
  params: AnalyticsParams,
): Promise<SessionShapeResponse> {
  return fetchJSON(`/analytics/sessions${buildQuery({ ...params })}`);
}

export function getAnalyticsVelocity(
  params: AnalyticsParams,
): Promise<VelocityResponse> {
  return fetchJSON(`/analytics/velocity${buildQuery({ ...params })}`);
}

export function getAnalyticsTools(
  params: AnalyticsParams,
): Promise<ToolsAnalyticsResponse> {
  return fetchJSON(`/analytics/tools${buildQuery({ ...params })}`);
}

export function getAnalyticsTopSessions(
  params: AnalyticsParams & {
    metric?: TopSessionsMetric;
  },
): Promise<TopSessionsResponse> {
  return fetchJSON(`/analytics/top-sessions${buildQuery({ ...params })}`);
}

export function getAnalyticsSignals(
  params: AnalyticsParams,
): Promise<SignalsAnalyticsResponse> {
  return fetchJSON(
    `/analytics/signals${buildQuery({ ...params })}`,
  );
}

export interface TrendsTermsParams extends AnalyticsParams {
  granularity?: TrendsGranularity;
  terms: string[];
}

function buildTrendsTermsQuery(params: TrendsTermsParams): string {
  const q = new URLSearchParams();
  for (const [key, value] of Object.entries(params)) {
    if (
      key === "terms" ||
      value === undefined ||
      value === null ||
      value === ""
    ) {
      continue;
    }
    // Match buildQuery semantics: 0 and false are valid query values.
    q.set(key, String(value));
  }
  for (const term of params.terms) {
    if (term.trim()) q.append("term", term);
  }
  const qs = q.toString();
  return qs ? `?${qs}` : "";
}

export function getTrendsTerms(
  params: TrendsTermsParams,
): Promise<TrendsTermsResponse> {
  return fetchJSON(`/trends/terms${buildTrendsTermsQuery(params)}`);
}

/* Insights */

export interface ListInsightsParams {
  type?: string;
  project?: string;
}

export function listInsights(
  params: ListInsightsParams = {},
): Promise<InsightsResponse> {
  return fetchJSON(`/insights${buildQuery({ ...params })}`);
}

export function getInsight(id: number): Promise<Insight> {
  return fetchJSON(`/insights/${id}`);
}

export async function deleteInsight(id: number): Promise<void> {
  const res = await fetch(`${getBase()}/insights/${id}`, authHeaders({
    method: "DELETE",
  }));
  if (!res.ok) {
    const body = await res.text();
    throw new ApiError(res.status, apiErrorMessage(res.status, body));
  }
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

    if (!res.ok || !res.body) {
      throw new Error(`Generate request failed: ${res.status}`);
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

/* Session Management */

export function renameSession(
  id: string,
  displayName: string | null,
): Promise<Session> {
  return fetchJSON(`/sessions/${id}/rename`, {
    method: "PATCH",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ display_name: displayName }),
  });
}

export async function deleteSession(id: string): Promise<void> {
  const res = await fetch(`${getBase()}/sessions/${id}`, authHeaders({
    method: "DELETE",
  }));
  if (!res.ok) {
    const body = await res.text();
    throw new ApiError(res.status, apiErrorMessage(res.status, body));
  }
}

export async function restoreSession(id: string): Promise<void> {
  const res = await fetch(`${getBase()}/sessions/${id}/restore`, authHeaders({
    method: "POST",
  }));
  if (!res.ok) {
    const body = await res.text();
    throw new ApiError(res.status, apiErrorMessage(res.status, body));
  }
}

export async function permanentDeleteSession(
  id: string,
): Promise<void> {
  const res = await fetch(`${getBase()}/sessions/${id}/permanent`, authHeaders({
    method: "DELETE",
  }));
  if (!res.ok) {
    const body = await res.text();
    throw new ApiError(res.status, apiErrorMessage(res.status, body));
  }
}

export function listTrash(): Promise<TrashResponse> {
  return fetchJSON("/trash");
}

export async function emptyTrash(): Promise<{ deleted: number }> {
  return fetchJSON("/trash", { method: "DELETE" });
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

/* Pins */

export function listPins(project?: string): Promise<PinsResponse> {
  const url = project ? `/pins?project=${encodeURIComponent(project)}` : "/pins";
  return fetchJSON(url);
}

export function listSessionPins(
  sessionId: string,
): Promise<PinsResponse> {
  return fetchJSON(`/sessions/${sessionId}/pins`);
}

export function pinMessage(
  sessionId: string,
  messageId: number,
  note?: string,
): Promise<{ id: number }> {
  return fetchJSON(
    `/sessions/${sessionId}/messages/${messageId}/pin`,
    {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ note: note ?? null }),
    },
  );
}

export async function unpinMessage(
  sessionId: string,
  messageId: number,
): Promise<void> {
  const res = await fetch(
    `${getBase()}/sessions/${sessionId}/messages/${messageId}/pin`,
    authHeaders({ method: "DELETE" }),
  );
  if (!res.ok) {
    const body = await res.text();
    throw new ApiError(res.status, apiErrorMessage(res.status, body));
  }
}

/* Usage */

export function getUsageSummary(
  params: UsageParams,
): Promise<UsageSummaryResponse> {
  return fetchJSON(`/usage/summary${buildQuery({ ...params })}`);
}

export function getUsageTopSessions(
  params: UsageTopSessionsParams,
): Promise<TopUsageSessionsResponse> {
  return fetchJSON(`/usage/top-sessions${buildQuery({ ...params })}`);
}
