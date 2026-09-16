import type { Report, SessionRow } from "./types/activity.js";
import { ActivityService, type ActivityReportSessionsResponse } from "./generated/index";
import { callGenerated } from "./runtime.js";
import { consumeEvents } from "./client.js";

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

export async function fetchActivityReport(
  query: ActivityReportQuery,
  signal?: AbortSignal,
  onProgress?: (progress: ActivityReportProgress) => void,
): Promise<Report> {
  const headers = new Headers();
  headers.set("Accept", "text/event-stream, application/json");
  const { gitBranch, ...params } = query;
  const res = await ActivityService.getApiV1ActivityReport(
    { ...params, git_branch: gitBranch },
    { headers, signal },
  );
  if (res.headers.get("Content-Type")?.includes("text/event-stream")) {
    return consumeEvents<Report>(
      res,
      ({ event, data }) => {
        if (event === "progress") onProgress?.(JSON.parse(data));
        if (event === "report") return JSON.parse(data);
        if (event === "error") throw new Error(JSON.parse(data).error ?? "Activity report failed");
      },
      "Activity report stream ended without a report event",
    );
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
