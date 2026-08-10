/* generated using openapi-typescript-codegen -- do not edit */
/* istanbul ignore file */
/* tslint:disable */
/* eslint-disable */
import type { ConfigDuckDBConfig } from './ConfigDuckDBConfig';
import type { ConfigPGConfig } from './ConfigPGConfig';
import type { SyncWatchBatch } from './SyncWatchBatch';
import type { SyncWatchRecoveryScope } from './SyncWatchRecoveryScope';
export type DaemonPushRequest = {
  automatic?: boolean;
  duckdb?: ConfigDuckDBConfig;
  exclude_projects?: any[] | null;
  full: boolean;
  last_reconciled_vector_generation?: number;
  migrate_legacy_sync_state?: boolean;
  no_vectors?: boolean;
  pg?: ConfigPGConfig;
  projects?: any[] | null;
  scope_vectors_to_changed_sessions?: boolean;
  sync_state_target?: string;
  watch_batch?: SyncWatchBatch;
  watch_recovery?: SyncWatchRecoveryScope;
};

