import type { Session } from "../api/types.js";
import type { SessionGroupInput } from "./sessionGroups.js";

// Maps sidebar index rows onto Session objects and compares them, so the
// store replaces a row only when a field it shows actually changed.

export function sidebarIndexRowToSession(row: SessionGroupInput): Session {
  const skinny: Session = {
    compaction_count: 0,
    consecutive_failure_max: 0,
    edit_churn_count: 0,
    ended_with_role: "",
    final_failure_streak: 0,
    mid_task_compaction_count: 0,
    outcome: "",
    outcome_confidence: "",
    secret_leak_count: 0,
    tool_failure_signal_count: 0,
    tool_retry_count: 0,
    id: row.id,
    ...indexFields(row),
    first_message: row.first_message ?? null,
    total_output_tokens: 0,
    peak_context_tokens: 0,
    has_total_output_tokens: false,
    has_peak_context_tokens: false,
    is_index_only: true,
  };
  return skinny;
}

export function indexFields(row: SessionGroupInput) {
  return {
    project: row.project,
    project_assigned: row.project_assigned ?? false,
    machine: row.machine,
    source_machines: row.source_machines,
    agent: row.agent,
    agent_label: row.agent_label ?? undefined,
    entrypoint: row.entrypoint ?? undefined,
    display_name: row.display_name ?? undefined,
    started_at: row.started_at,
    ended_at: row.ended_at,
    message_count: row.message_count,
    user_message_count: row.user_message_count ?? 0,
    parent_session_id: row.parent_session_id ?? undefined,
    parent_session_ids: row.parent_session_ids,
    relationship_type: row.relationship_type ?? undefined,
    termination_status: row.termination_status ?? undefined,
    transcript_revision: row.transcript_revision,
    is_automated: row.is_automated ?? false,
    is_teammate: row.is_teammate,
    created_at: row.created_at,
  };
}

export function sameSessionFields(a: Session, b: Session): boolean {
  for (const key in a) {
    if (Object.hasOwn(a, key) && !sameField(a[key as keyof Session], b[key as keyof Session]))
      return false;
  }
  for (const key in b) {
    if (
      Object.hasOwn(b, key) &&
      !Object.prototype.propertyIsEnumerable.call(a, key) &&
      !sameField(a[key as keyof Session], b[key as keyof Session])
    )
      return false;
  }
  return true;
}

export function sameField(a: unknown, b: unknown): boolean {
  if (a === b) return true;
  if (!Array.isArray(a) || !Array.isArray(b) || a.length !== b.length) return false;
  for (let i = 0; i < a.length; i++) {
    if (i in a && a[i] !== b[i]) return false;
  }
  return true;
}
