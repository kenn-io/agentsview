/* generated using openapi-typescript-codegen -- do not edit */
/* istanbul ignore file */
/* tslint:disable */
/* eslint-disable */
import type { ConfigDuckDBConfig } from './ConfigDuckDBConfig';
import type { ConfigPGConfig } from './ConfigPGConfig';
export type DaemonPushRequest = {
  duckdb?: ConfigDuckDBConfig;
  exclude_projects?: any[] | null;
  full: boolean;
  migrate_legacy_sync_state?: boolean;
  no_vectors?: boolean;
  pg?: ConfigPGConfig;
  projects?: any[] | null;
  sync_state_target?: string;
};

