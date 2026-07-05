/* generated using openapi-typescript-codegen -- do not edit */
/* istanbul ignore file */
/* tslint:disable */
/* eslint-disable */
import type { SyncAnomalyStats } from './SyncAnomalyStats';
export type SyncSyncStats = {
  aborted?: boolean;
  anomalies?: SyncAnomalyStats;
  failed: number;
  orphaned_copied?: number;
  skipped: number;
  synced: number;
  total_sessions: number;
  warnings?: any[] | null;
};

