package db

// Dashboard input columns are shared by the archive, PostgreSQL journal, and
// DuckDB content digests. Message join keys differ between the backends.
var (
	DashboardSessionInputs         = []string{"id", "message_count", "relationship_type", "deleted_at", "machine", "project", "git_branch", "agent", "user_message_count", "is_automated", "started_at", "ended_at", "created_at", "termination_status"}
	DashboardToolsSessionInputs    = []string{"id", "quality_signal_version"}
	DashboardSignalsSessionInputs  = []string{"id", "first_message", "health_score", "health_grade", "outcome", "outcome_confidence", "tool_failure_signal_count", "tool_retry_count", "edit_churn_count", "compaction_count", "mid_task_compaction_count", "context_pressure_max", "quality_signal_version", "short_prompt_count", "unstructured_start", "missing_success_criteria_count", "missing_verification_count", "duplicate_prompt_count", "no_code_context_count", "runaway_tool_loop_count"}
	DashboardScopeMessageInputs    = []string{"session_id", "model", "timestamp", "ordinal", "role", "is_system"}
	DashboardToolsMessageInputs    = []string{"session_id", "timestamp", "model", "ordinal"}
	DashboardVelocityMessageInputs = []string{"session_id", "ordinal", "role", "timestamp", "content_length", "source_subtype", "model", "is_system"}
	DashboardSignalsMessageInputs  = []string{"session_id", "content", "role", "source_subtype", "is_system"}
	DashboardToolsCallInputs       = []string{"session_id", "category", "tool_name", "observed_outcome", "observed_repeat", "sequence_ending"}
	DashboardVelocityCallInputs    = []string{"session_id"}
)

func DashboardUsesMessageScope(f AnalyticsFilter) bool {
	return f.Model != "" || f.HasTimeFilter() || f.From != "" || f.To != ""
}
