/* generated using openapi-typescript-codegen -- do not edit */
/* istanbul ignore file */
/* tslint:disable */
/* eslint-disable */
import type { DbIssueReviewEvidence } from './DbIssueReviewEvidence';
export type DbIssueReviewFinding = {
  confidence: string;
  duration_coverage: number;
  duration_source?: string;
  evidence: Array<DbIssueReviewEvidence>;
  github_reference?: string;
  id: string;
  incomplete_session_count: number;
  last_seen: string;
  occurrences: number;
  p95_duration_ms?: number;
  project_count: number;
  reason_code: string;
  recommendation: string;
  recommendation_type: string;
  review_state: string;
  review_state_expires_at?: string;
  session_count: number;
  severity: string;
  signature: string;
  sources: Array<string>;
  status: string;
  tool: string;
  total_duration_ms: number;
  wasted_duration_ms: number;
};
