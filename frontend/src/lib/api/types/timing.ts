/** HTTP and SSE timing payload, with durations in milliseconds. */
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
  activity: TurnActivity[];
  activity_totals: ActivityTotals;
  running: boolean;
}

export interface ActivityTotals {
  thinking_ms: number;
  generation_ms: number;
  tool_ms: number;
  unattributed_ms: number;
}

export interface TurnActivity extends ActivityTotals {
  message_id: number;
  ordinal: number;
  started_at: string;
  duration_ms: number;
  precision: "message_only";
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
  /** Unknown until a closed execution interval is recorded. */
  duration_ms: number | null;
  is_parallel: boolean;
  input_preview: string;
}
