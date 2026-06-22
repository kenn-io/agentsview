/** Matches Go Progress struct in internal/sync/progress.go */
export interface SyncProgress {
  phase: string;
  detail?: string;
  hint?: string;
  resync?: boolean;
  current_project?: string;
  projects_total: number;
  projects_done: number;
  sessions_total: number;
  sessions_done: number;
  messages_indexed: number;
}

/** Matches Go SyncStats struct */
export interface SyncStats {
  total_sessions: number;
  synced: number;
  skipped: number;
  failed: number;
  orphaned_copied?: number;
  warnings?: string[];
  aborted?: boolean;
}

export interface SyncStatus {
  last_sync: string;
  stats: SyncStats | null;
  progress?: SyncProgress | null;
}
