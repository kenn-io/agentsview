/* generated using openapi-typescript-codegen -- do not edit */
/* istanbul ignore file */
/* tslint:disable */
/* eslint-disable */
import type { DbQualitySignals } from './DbQualitySignals';
export type ServiceSessionDetail = {
  agent: string;
  compaction_count: number;
  consecutive_failure_max: number;
  context_pressure_max?: number;
  created_at: string;
  cwd?: string;
  decode_confidence?: string;
  deleted_at?: string;
  display_name?: string;
  edit_churn_count: number;
  ended_at: string | null;
  ended_with_role: string;
  file_device?: number;
  file_hash?: string;
  file_inode?: number;
  file_mtime?: number;
  file_path?: string;
  file_size?: number;
  final_failure_streak: number;
  first_message: string | null;
  git_branch?: string;
  has_peak_context_tokens: boolean;
  has_total_output_tokens: boolean;
  health_grade?: string;
  health_penalties?: Record<string, number>;
  health_score?: number;
  health_score_basis?: any[] | null;
  id: string;
  is_automated: boolean;
  is_truncated?: boolean;
  local_modified_at?: string;
  machine: string;
  message_count: number;
  mid_task_compaction_count: number;
  outcome: string;
  outcome_confidence: string;
  parent_session_id?: string;
  parser_malformed_lines?: number;
  peak_context_tokens: number;
  project: string;
  quality_signals?: DbQualitySignals;
  relationship_type?: string;
  secret_leak_count: number;
  signals_pending_since?: string;
  source_session_id?: string;
  source_version?: string;
  started_at: string | null;
  termination_status?: string;
  tool_failure_signal_count: number;
  tool_retry_count: number;
  total_output_tokens: number;
  transcript_fidelity?: string;
  user_message_count: number;
};

