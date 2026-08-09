/* generated using openapi-typescript-codegen -- do not edit */
/* istanbul ignore file */
/* tslint:disable */
/* eslint-disable */
import type { DbIssueReviewFacets } from './DbIssueReviewFacets';
import type { DbIssueReviewFinding } from './DbIssueReviewFinding';
export type DbIssueReviewResponse = {
  analyzed_messages: number;
  analyzed_tool_calls: number;
  duplicate_messages: number;
  duplicate_tool_calls: number;
  facets: DbIssueReviewFacets;
  findings: Array<DbIssueReviewFinding>;
  generated_at: string;
  scanned_messages: number;
  scanned_sessions: number;
  scanned_telemetry: number;
  scanned_tool_calls: number;
  telemetry_status: string;
  total_findings: number;
  truncated: boolean;
};
