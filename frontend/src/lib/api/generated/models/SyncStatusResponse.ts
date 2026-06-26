/* generated using openapi-typescript-codegen -- do not edit */
/* istanbul ignore file */
/* tslint:disable */
/* eslint-disable */
import type { SyncProgress } from './SyncProgress';
import type { SyncSyncStats } from './SyncSyncStats';
export type SyncStatusResponse = {
  last_sync: string;
  progress?: SyncProgress;
  stats: SyncSyncStats;
};

