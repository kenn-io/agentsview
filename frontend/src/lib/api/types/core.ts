/** Matches Go VersionInfo struct in internal/server/server.go */
export interface VersionInfo {
  version: string;
  commit: string;
  build_date: string;
  insight_generation_available?: boolean;
  read_only?: boolean;
}

/** Matches Go Session struct in internal/db/sessions.go */
export interface QualitySignals {
  version: number;
  short_prompt_count: number;
  unstructured_start: boolean;
  missing_success_criteria_count: number;
  missing_verification_count: number;
  duplicate_prompt_count: number;
  no_code_context_count: number;
  runaway_tool_loop_count: number;
}

export interface Session {
  id: string;
  project: string;
  machine: string;
  cwd?: string;
  agent: string;
  agent_label?: string;
  entrypoint?: string;
  first_message: string | null;
  display_name?: string | null;
  started_at: string | null;
  ended_at: string | null;
  message_count: number;
  user_message_count: number;
  parent_session_id?: string;
  relationship_type?: string;
  deleted_at?: string | null;
  termination_status?: string | null;
  transcript_revision?: string;
  file_path?: string;
  file_size?: number;
  file_mtime?: number;
  total_output_tokens: number;
  peak_context_tokens: number;
  has_total_output_tokens?: boolean;
  has_peak_context_tokens?: boolean;
  is_automated: boolean;
  is_teammate?: boolean;
  /** True when populated from the skinny sidebar index and not yet hydrated. */
  is_index_only?: boolean;
  // Session signals (from backend computation)
  health_score?: number | null;
  health_grade?: string | null;
  outcome?: string;
  outcome_confidence?: string;
  ended_with_role?: string;
  tool_failure_signal_count?: number;
  tool_retry_count?: number;
  edit_churn_count?: number;
  consecutive_failure_max?: number;
  final_failure_streak?: number;
  compaction_count?: number;
  mid_task_compaction_count?: number;
  context_pressure_max?: number | null;
  quality_signals?: QualitySignals | null;
  // Detail-only fields (from enriched detail response)
  health_score_basis?: string[] | null;
  health_penalties?: Record<string, number> | null;
  transcript_fidelity?: string;
  parser_malformed_lines?: number;
  /**
   * Antigravity decode confidence, derived on read from agent +
   * source_version (see internal/service SessionDetail.MarshalJSON).
   * "low" means an unrecognized (newer) schema; "high" a known range;
   * absent for other agents. Only present on the detail response.
   */
  decode_confidence?: string;
  created_at: string;
}

/** Matches Go SessionPage struct */
export interface SessionPage {
  sessions: Session[];
  next_cursor?: string;
  total: number;
}

/** Skinny sidebar index row from GET /api/v1/sessions/sidebar-index. */
export interface SidebarSessionIndexRow {
  id: string;
  parent_session_id?: string | null;
  relationship_type?: string | null;
  project: string;
  machine: string;
  agent: string;
  agent_label?: string | null;
  entrypoint?: string | null;
  display_name?: string | null;
  started_at: string | null;
  ended_at: string | null;
  created_at: string;
  termination_status?: string | null;
  message_count: number;
  user_message_count: number;
  transcript_revision?: string;
  is_automated: boolean;
  is_teammate?: boolean;
}

/** Matches Go SidebarSessionIndex struct. */
export interface SidebarSessionIndexResponse {
  sessions: SidebarSessionIndexRow[];
  next_cursor?: string | null;
  total: number;
}

/** Matches Go ProjectInfo struct */
export interface ProjectInfo {
  name: string;
  session_count: number;
}

/** Matches Go ToolResultEvent struct in internal/db/messages.go */
export interface ToolResultEvent {
  tool_use_id?: string;
  agent_id?: string;
  subagent_session_id?: string;
  source: string;
  status: string;
  content: string;
  content_length: number;
  timestamp?: string;
  event_index: number;
}

/** Matches Go ToolCall struct in internal/db/messages.go */
export interface ToolCall {
  tool_name: string;
  category?: string;
  tool_use_id?: string;
  input_json?: string;
  skill_name?: string;
  result_content_length?: number;
  result_content?: string;
  subagent_session_id?: string;
  result_events?: ToolResultEvent[];
}

/** Matches Go Message struct in internal/db/messages.go */
export interface Message {
  id: number;
  session_id: string;
  ordinal: number;
  role: string;
  content: string;
  timestamp: string;
  has_thinking: boolean;
  thinking_text: string;
  has_tool_use: boolean;
  content_length: number;
  model: string;
  token_usage?: Record<string, number | boolean> | null;
  context_tokens: number;
  output_tokens: number;
  has_context_tokens?: boolean;
  has_output_tokens?: boolean;
  tool_calls?: ToolCall[];
  is_system: boolean;
  is_compact_boundary?: boolean;
  source_subtype?: string;
}

/** Matches Go SearchResult struct in internal/db/search.go */
export interface SearchResult {
  session_id: string;
  project: string;
  agent: string;
  name: string;
  ordinal: number;
  session_ended_at: string;
  snippet: string;
  rank: number;
}

/** Matches Go Stats struct in internal/db/stats.go */
export interface Stats {
  session_count: number;
  message_count: number;
  project_count: number;
  machine_count: number;
  earliest_session: string | null;
}

export interface MessagesResponse {
  messages: Message[];
  count: number;
}

export interface SearchResponse {
  query: string;
  results: SearchResult[];
  count: number;
  next: number;
}

export interface ProjectsResponse {
  projects: ProjectInfo[];
}

export interface MachinesResponse {
  machines: string[];
}

/** Matches Go BranchInfo struct in internal/db/sessions.go */
export interface BranchInfo {
  project: string;
  branch: string;
  token: string;
}

export interface BranchesResponse {
  branches: BranchInfo[];
}

/** Matches Go AgentInfo struct */
export interface AgentInfo {
  name: string;
  session_count: number;
}

export interface AgentsResponse {
  agents: AgentInfo[];
}

/** Matches Go PinnedMessage struct in internal/db/pins.go */
export interface PinnedMessage {
  id: number;
  session_id: string;
  message_id: number;
  ordinal: number;
  note?: string;
  content?: string | null;
  role?: string | null;
  created_at: string;
  // Session metadata — populated for the "all pins" query.
  session_project?: string | null;
  session_agent?: string | null;
  session_display_name?: string | null;
  session_first_message?: string | null;
}

export interface PinsResponse {
  pins: PinnedMessage[];
}

export interface TrashResponse {
  sessions: Session[];
}

/** Matches Go updateCheckResponse in internal/server/update.go */
export interface UpdateCheck {
  update_available: boolean;
  current_version: string;
  latest_version?: string;
  is_dev_build?: boolean;
}
