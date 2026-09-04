import type { Report, SessionRow } from "./types/activity.js";
import { ActivityService, type ActivityReportSessionsResponse } from "./generated/index";
import { ApiError, authHeaders, callGenerated, getBase, responseErrorMessage } from "./runtime.js";

export interface ActivityReportQuery {
  preset?: "day" | "week" | "month" | "custom";
  date?: string;
  from?: string;
  to?: string;
  timezone?: string;
  bucket?: "5m" | "15m" | "1h" | "1d" | "1w";
  project?: string;
  gitBranch?: string;
  agent?: string;
  machine?: string;
  automation?: string;
}

export interface ActivityReportProgress {
  phase: "loading_sessions" | "loading_usage" | "scanning_activity" | "finalizing" | "done";
  sessions_total?: number;
  sessions_processed?: number;
  rows_processed?: number;
}

type ActivitySessionRequest = NonNullable<
  Parameters<typeof ActivityService.getApiV1ActivityReportByReportIdSessions>[1]
>;

export type ActivitySessionSort = NonNullable<ActivitySessionRequest["sort"]>;

export interface ActivityBucketRange {
  start: number;
  end: number;
}

export interface ActivitySessionPageOptions {
  limit?: number;
  cursor?: string;
  sort?: ActivitySessionSort;
  direction?: "asc" | "desc";
  bucketRange?: ActivityBucketRange | null;
}

export type ActivitySessionPage = Omit<ActivityReportSessionsResponse, "sessions" | "report"> & {
  sessions: SessionRow[];
  report?: Report;
};

function appendQuery(params: URLSearchParams, key: string, value: unknown): void {
  if (value === undefined || value === null || value === "") return;
  params.set(key, String(value));
}

function reportQuery(query: ActivityReportQuery): URLSearchParams {
  const params = new URLSearchParams();
  appendQuery(params, "preset", query.preset);
  appendQuery(params, "date", query.date);
  appendQuery(params, "from", query.from);
  appendQuery(params, "to", query.to);
  appendQuery(params, "timezone", query.timezone);
  appendQuery(params, "bucket", query.bucket);
  appendQuery(params, "project", query.project);
  appendQuery(params, "git_branch", query.gitBranch);
  appendQuery(params, "agent", query.agent);
  appendQuery(params, "machine", query.machine);
  appendQuery(params, "automation", query.automation);
  return params;
}

interface SSEFrame {
  event: string;
  data: string;
}

function parseFrame(frame: string): SSEFrame {
  let event = "message";
  const data: string[] = [];
  for (const line of frame.split("\n")) {
    if (line.startsWith("event:")) event = line.slice(6).trimStart();
    if (line.startsWith("data:")) data.push(line.slice(5).trimStart());
  }
  return { event, data: data.join("\n") };
}

function handleFrame(
  frame: string,
  onProgress?: (progress: ActivityReportProgress) => void,
): Report | undefined {
  const parsed = parseFrame(frame);
  if (!parsed.data) return undefined;
  if (parsed.event === "progress") {
    onProgress?.(JSON.parse(parsed.data) as ActivityReportProgress);
    return undefined;
  }
  if (parsed.event === "report") return JSON.parse(parsed.data) as Report;
  if (parsed.event === "error") {
    const payload = JSON.parse(parsed.data) as { error?: string };
    throw new Error(payload.error ?? "Activity report failed");
  }
  return undefined;
}

async function readReportStream(
  body: ReadableStream<Uint8Array>,
  onProgress?: (progress: ActivityReportProgress) => void,
): Promise<Report> {
  const reader = body.getReader();
  const decoder = new TextDecoder();
  let buffer = "";
  for (;;) {
    const { done, value } = await reader.read();
    buffer += decoder.decode(value, { stream: !done }).replaceAll("\r\n", "\n");
    let boundary: number;
    while ((boundary = buffer.indexOf("\n\n")) !== -1) {
      const report = handleFrame(buffer.slice(0, boundary), onProgress);
      buffer = buffer.slice(boundary + 2);
      if (report) {
        await reader.cancel();
        return report;
      }
    }
    if (done) break;
  }
  if (buffer.trim()) {
    const report = handleFrame(buffer, onProgress);
    if (report) return report;
  }
  throw new Error("Activity report stream ended without a report event");
}

export async function fetchActivityReport(
  query: ActivityReportQuery,
  signal?: AbortSignal,
  onProgress?: (progress: ActivityReportProgress) => void,
): Promise<Report> {
  const headers = new Headers();
  headers.set("Accept", "text/event-stream, application/json");
  const res = await fetch(
    `${getBase()}/activity/report?${reportQuery(query)}`,
    authHeaders({ method: "GET", headers, signal }),
  );
  if (!res.ok) {
    throw new ApiError(res.status, await responseErrorMessage(res));
  }
  if (res.headers.get("Content-Type")?.includes("text/event-stream")) {
    if (!res.body) throw new Error("Activity report response has no body");
    return readReportStream(res.body, onProgress);
  }
  return (await res.json()) as Report;
}

export async function fetchActivitySessions(
  reportID: string,
  options: ActivitySessionPageOptions,
  signal?: AbortSignal,
): Promise<ActivitySessionPage> {
  const page = await callGenerated(
    (requestOptions) =>
      ActivityService.getApiV1ActivityReportByReportIdSessions(
        { reportId: reportID },
        {
          limit: options.limit,
          cursor: options.cursor,
          sort: options.sort,
          direction: options.direction,
          bucket_start: options.bucketRange?.start,
          bucket_end: options.bucketRange?.end,
        },
        requestOptions,
      ),
    signal,
  );
  return {
    ...page,
    sessions: page.sessions ?? [],
  };
}
