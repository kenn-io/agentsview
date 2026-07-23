export interface RecallEvidence {
  id: number;
  entry_id: string;
  session_id: string;
  message_start_ordinal: number;
  message_end_ordinal: number;
  snippet?: string;
}

export interface RecallEntry {
  id: string;
  type: string;
  scope: string;
  status: string;
  review_state: string;
  title: string;
  body: string;
  trigger?: string;
  confidence?: number;
  uncertainty?: string;
  project?: string;
  cwd?: string;
  git_branch?: string;
  agent?: string;
  source_session_id: string;
  source_run_id?: string;
  extractor_method?: string;
  model?: string;
  transferable: boolean;
  provenance_ok: boolean;
  created_at: string;
  updated_at: string;
  evidence?: RecallEvidence[];
}

export interface RecallEntriesResponse {
  entries: RecallEntry[];
  trusted_only: boolean;
}

export interface RecallExtractGeneration {
  fingerprint: string;
  state: string;
  model: string;
  segmenter: string;
  created_at: string;
  updated_at: string;
}

export interface RecallExtractProgressStats {
  pending: number;
  partial: number;
  done: number;
  failed: number;
  units_done: number;
  units_total: number;
  entries: number;
}

export interface RecallExtractionStatus {
  configured: boolean;
  fingerprint?: string;
  generations?: RecallExtractGeneration[];
  stats?: RecallExtractProgressStats;
  eligible_backlog?: number;
}

export interface RecallEntryFilters {
  query?: string;
  project?: string;
  type?: string;
  sourceRunId?: string;
  reviewState?: string;
  limit?: number;
}
