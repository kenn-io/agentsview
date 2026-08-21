/* generated using openapi-typescript-codegen -- do not edit */
/* istanbul ignore file */
/* tslint:disable */
/* eslint-disable */
import type { SyncAnomalyStats } from './SyncAnomalyStats';
export type SyncSyncStats = {
  aborted?: boolean;
  anomalies?: SyncAnomalyStats;
  cwd_updated?: number;
  failed: number;
  orphaned_copied?: number;
  rebuild_phases?: any[] | null;
  skipped: number;
  synced: number;
  tombstoned?: number;
  total_sessions: number;
  warnings?: any[] | null;
};

