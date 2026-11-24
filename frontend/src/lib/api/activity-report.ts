import type { Report } from "./types/activity.js";
import {
  ActivityService,
  type ActivityReportSessionsResponse,
  type GetApiV1ActivityReportParams,
} from "./generated/index";
import { consumeEvents } from "./client.js";
import { m } from "../i18n/index.js";

const ACTIVITY_REPORT_SCHEMA_VERSION = 9;

function validateActivityReport(report: Report): Report {
  if (report?.schema_version !== ACTIVITY_REPORT_SCHEMA_VERSION) {
    throw new Error(m.activity_report_load_failed());
  }
  return report;
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

export async function fetchActivityReport(
  query: GetApiV1ActivityReportParams,
  signal?: AbortSignal,
  onProgress?: (progress: ActivityReportProgress) => void,
): Promise<Report> {
  const headers = new Headers();
  headers.set("Accept", "text/event-stream, application/json");
  const res = await ActivityService.getApiV1ActivityReport(query, { headers, signal });
  if (res.headers.get("Content-Type")?.includes("text/event-stream")) {
    const report = await consumeEvents<Report>(
      res,
      ({ event, data }) => {
        if (event === "progress") onProgress?.(JSON.parse(data));
        if (event === "report") return JSON.parse(data);
        if (event === "error") throw new Error(JSON.parse(data).error ?? "Activity report failed");
      },
      "Activity report stream ended without a report event",
    );
    return validateActivityReport(report);
  }
  return validateActivityReport((await res.json()) as Report);
}

export async function fetchActivitySessions(
  reportID: string,
  options: ActivitySessionPageOptions,
  signal?: AbortSignal,
): Promise<ActivityReportSessionsResponse> {
  const page = await ActivityService.getApiV1ActivityReportByReportIdSessions(
    { reportId: reportID },
    {
      limit: options.limit,
      cursor: options.cursor,
      sort: options.sort,
      direction: options.direction,
      bucket_start: options.bucketRange?.start,
      bucket_end: options.bucketRange?.end,
    },
    { signal },
  );
  if (page.report) validateActivityReport(page.report);
  return page;
}
