import type {
  ActivityReport,
  ActivityBucket,
  ActivityReportInterval,
  ActivitySessionRow,
  ActivityKeyMinutes,
  ActivityBranchKeyMinutes,
} from "../generated/index";

export type Bucket = ActivityBucket;
export type ReportInterval = ActivityReportInterval;
// models comes across as any[]; the codegen can't type OpenAPI 3.1's
// nullable-array items (see the Report comment below).
export type SessionRow = Omit<ActivitySessionRow, "models"> & {
  models: string[] | null;
};
export type KeyMinutes = ActivityKeyMinutes;
export type BranchKeyMinutes = ActivityBranchKeyMinutes;

// Narrows the generated model's any[] | null collections to their element
// types; openapi-typescript-codegen can't type OpenAPI 3.1's nullable-array
// syntax (type: [T, "null"]) and falls back to any[]. Structurally
// compatible with ActivityReport, so responses need no runtime conversion.
export type Report = Omit<
  ActivityReport,
  | "buckets"
  | "by_agent"
  | "by_branch"
  | "by_model"
  | "by_project"
  | "by_session"
  | "intervals"
> & {
  buckets: Bucket[] | null;
  by_agent: KeyMinutes[] | null;
  by_branch: BranchKeyMinutes[] | null;
  by_model: KeyMinutes[] | null;
  by_project: KeyMinutes[] | null;
  by_session: SessionRow[] | null;
  intervals: ReportInterval[] | null;
};
