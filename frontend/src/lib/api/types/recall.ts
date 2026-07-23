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
