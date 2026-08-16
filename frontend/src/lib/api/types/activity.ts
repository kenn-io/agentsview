import type {
  ActivityReport,
  ActivityBucket,
  ActivitySessionRow,
  ActivityKeyMinutes,
} from "../generated/index";

export type Report = ActivityReport & {
  report_id?: string;
  sessions_next_cursor?: string;
  sessions_total?: number;
};
export type Bucket = ActivityBucket;
export type SessionRow = ActivitySessionRow;
export type KeyMinutes = ActivityKeyMinutes;
