/* generated using openapi-typescript-codegen -- do not edit */
/* istanbul ignore file */
/* tslint:disable */
/* eslint-disable */
import type { SyncSanitizeStats } from './SyncSanitizeStats';
export type SyncAnomalyStats = {
  gen_metadata_without_usage_by_agent?: Record<string, number>;
  gen_metadata_without_usage_total?: number;
  malformed_lines_by_agent?: Record<string, number>;
  malformed_lines_total?: number;
  sanitize?: SyncSanitizeStats;
  unknown_schema_sessions_by_agent?: Record<string, number>;
  unknown_schema_sessions_total?: number;
};

