import type {
  SessionPage,
  Session,
  MessagesResponse,
  MinimapResponse,
  SearchResponse,
  ProjectsResponse,
  MachinesResponse,
  AgentsResponse,
  Stats,
  VersionInfo,
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
  Granularity,
  HeatmapMetric,
  TopSessionsMetric,
  Insight,
  InsightsResponse,
  GenerateInsightRequest,
  PinsResponse,
  TrashResponse,
} from "./types.js";

const BASE = "/api/v1";

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
  const res = await fetch(`${BASE}${path}`, init);
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
  date?: string;
  date_from?: string;
  date_to?: string;
  active_since?: string;
  min_messages?: number;
  max_messages?: number;
  min_user_messages?: number;
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

export interface GetMinimapParams {
  from?: number;
  max?: number;
}

export function getMinimap(
  sessionId: string,
  params: GetMinimapParams = {},
): Promise<MinimapResponse> {
  return fetchJSON(
    `/sessions/${sessionId}/minimap${buildQuery({ ...params })}`,
  );
}

/* Search */

export function search(
  query: string,
  params: {
    project?: string;
    limit?: number;
    cursor?: number;
  } = {},
  init?: RequestInit,
): Promise<SearchResponse> {
  if (!query) {
    throw new Error("search query must not be empty");
  }
  return fetchJSON(`/search${buildQuery({ q: query, ...params })}`, init);
}

/* Metadata */

export function getProjects(): Promise<ProjectsResponse> {
  return fetchJSON("/projects");
}

export function getMachines(): Promise<MachinesResponse> {
  return fetchJSON("/machines");
}

export function getAgents(): Promise<AgentsResponse> {
  return fetchJSON("/agents");
}

export function getStats(): Promise<Stats> {
  return fetchJSON("/stats");
}

export function getVersion(): Promise<VersionInfo> {
  return fetchJSON("/version");
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
    const res = await fetch(`${BASE}${path}`, {
      method: "POST",
      signal: controller.signal,
    });

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

/** Watch a session for live updates via SSE */
export function watchSession(
  sessionId: string,
  onUpdate: () => void,
): EventSource {
  const es = new EventSource(`${BASE}/sessions/${sessionId}/watch`);

  es.addEventListener("session_updated", () => {
    onUpdate();
  });

  es.onerror = () => {
    // Connection will auto-retry via EventSource spec
  };

  return es;
}

/** Get the export URL for a session */
export function getExportUrl(sessionId: string): string {
  return `${BASE}/sessions/${sessionId}/export`;
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
  const res = await fetch(`${BASE}/sessions/${id}/star`, {
    method: "PUT",
  });
  if (!res.ok) {
    const body = await res.text();
    throw new ApiError(res.status, apiErrorMessage(res.status, body));
  }
}

export async function unstarSession(id: string): Promise<void> {
  const res = await fetch(`${BASE}/sessions/${id}/star`, {
    method: "DELETE",
  });
  if (!res.ok) {
    const body = await res.text();
    throw new ApiError(res.status, apiErrorMessage(res.status, body));
  }
}

export async function bulkStarSessions(
  sessionIds: string[],
): Promise<void> {
  const res = await fetch(`${BASE}/starred/bulk`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ session_ids: sessionIds }),
  });
  if (!res.ok) {
    const body = await res.text();
    throw new ApiError(res.status, apiErrorMessage(res.status, body));
  }
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
  active_since?: string;
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
  const res = await fetch(`${BASE}/insights/${id}`, {
    method: "DELETE",
  });
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
    const res = await fetch(`${BASE}/insights/generate`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(req),
      signal: controller.signal,
    });

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
  const res = await fetch(`${BASE}/sessions/${id}`, {
    method: "DELETE",
  });
  if (!res.ok) {
    const body = await res.text();
    throw new ApiError(res.status, apiErrorMessage(res.status, body));
  }
}

export async function restoreSession(id: string): Promise<void> {
  const res = await fetch(`${BASE}/sessions/${id}/restore`, {
    method: "POST",
  });
  if (!res.ok) {
    const body = await res.text();
    throw new ApiError(res.status, apiErrorMessage(res.status, body));
  }
}

export async function permanentDeleteSession(
  id: string,
): Promise<void> {
  const res = await fetch(`${BASE}/sessions/${id}/permanent`, {
    method: "DELETE",
  });
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

/* Pins */

export function listPins(): Promise<PinsResponse> {
  return fetchJSON("/pins");
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
    `${BASE}/sessions/${sessionId}/messages/${messageId}/pin`,
    { method: "DELETE" },
  );
  if (!res.ok) {
    const body = await res.text();
    throw new ApiError(res.status, apiErrorMessage(res.status, body));
  }
}
