/** Mirrors Go SessionTiming struct in internal/db/timing.go.
 *  Payload of GET /api/v1/sessions/{id}/timing and the
 *  session.timing SSE event. All durations are in milliseconds.
 *  Nullable number fields are null when the underlying value is
 *  unknown (e.g. running, missing timestamp, parallel non-sub-agent
 *  call). */
export interface SessionTiming {
  session_id: string;
  total_duration_ms: number;
  tool_duration_ms: number;
  turn_count: number;
  tool_call_count: number;
  subagent_count: number;
  slowest_call: CallTiming | null;
  by_category: CategoryTotal[];
  turns: TurnTiming[];
  running: boolean;
}

export interface CategoryTotal {
  category: string;
  duration_ms: number;
  call_count: number;
}

export interface TurnTiming {
  message_id: number;
  /** Message ordinal, for ui.scrollToOrdinal. */
  ordinal: number;
  started_at: string;
  duration_ms: number | null;
  primary_category: string;
  calls: CallTiming[];
}

export interface CallTiming {
  tool_use_id: string;
  tool_name: string;
  category: string;
  skill_name?: string;
  subagent_session_id?: string;
  duration_ms: number | null;
  is_parallel: boolean;
  input_preview: string;
}
