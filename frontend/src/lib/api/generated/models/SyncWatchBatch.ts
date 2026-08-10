/* generated using openapi-typescript-codegen -- do not edit */
/* istanbul ignore file */
/* tslint:disable */
/* eslint-disable */
import type { SyncWatchRename } from './SyncWatchRename';
export type SyncWatchBatch = {
  full_sync?: boolean;
  lost_events?: boolean;
  paths?: Array<string> | null;
  reconcile_roots?: Array<string> | null;
  renames?: Array<SyncWatchRename> | null;
};
