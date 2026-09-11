package postgres

// schemaColumnMigrations is shared by legacy setup and atomic hosted provisioning.
func schemaColumnMigrations() []columnMigration {
	return []columnMigration{
		{"sessions", "provenance_kind", `provenance_kind TEXT NOT NULL DEFAULT 'legacy'`, "adding sessions.provenance_kind"},
		{"sessions", "raw_group_id", `raw_group_id TEXT NOT NULL DEFAULT ''`, "adding sessions.raw_group_id"},
		{"sessions", "raw_content_revision", `raw_content_revision TEXT NOT NULL DEFAULT ''`, "adding sessions.raw_content_revision"},
		{
			"model_pricing", "cache_creation_1h_microdollars_per_mtok",
			`cache_creation_1h_microdollars_per_mtok BIGINT NOT NULL DEFAULT 0`,
			"adding model_pricing.cache_creation_1h_microdollars_per_mtok",
		},
		{
			"model_pricing_bands", "cache_creation_1h_microdollars_per_mtok",
			`cache_creation_1h_microdollars_per_mtok BIGINT NOT NULL DEFAULT 0`,
			"adding model_pricing_bands.cache_creation_1h_microdollars_per_mtok",
		},
		{
			"sessions", "transcript_revision",
			`transcript_revision TEXT NOT NULL DEFAULT '0'`,
			"adding sessions.transcript_revision",
		},
		{
			"sessions", "owner_marker",
			`owner_marker TEXT NOT NULL DEFAULT ''`,
			"adding sessions.owner_marker",
		},
		{
			"sessions", "deleted_at",
			`deleted_at TIMESTAMPTZ`,
			"adding sessions.deleted_at",
		},
		{
			"sessions", "created_at",
			`created_at TIMESTAMPTZ`,
			"adding sessions.created_at",
		},
		{
			"sessions", "source_display_name",
			`source_display_name TEXT`,
			"adding sessions.source_display_name",
		},
		{
			"sessions", "source_deleted_at",
			`source_deleted_at TIMESTAMPTZ`,
			"adding sessions.source_deleted_at",
		},
		{
			"sessions", "deletion_cause",
			`deletion_cause TEXT`,
			"adding sessions.deletion_cause",
		},
		{
			"sessions", "parser_parent_session_id",
			`parser_parent_session_id TEXT`,
			"adding sessions.parser_parent_session_id",
		},
		{
			"sessions", "total_output_tokens",
			`total_output_tokens INT NOT NULL DEFAULT 0`,
			"adding sessions.total_output_tokens",
		},
		{
			"sessions", "peak_context_tokens",
			`peak_context_tokens INT NOT NULL DEFAULT 0`,
			"adding sessions.peak_context_tokens",
		},
		{
			"sessions", "has_total_output_tokens",
			`has_total_output_tokens BOOLEAN NOT NULL DEFAULT FALSE`,
			"adding sessions.has_total_output_tokens",
		},
		{
			"sessions", "has_peak_context_tokens",
			`has_peak_context_tokens BOOLEAN NOT NULL DEFAULT FALSE`,
			"adding sessions.has_peak_context_tokens",
		},
		{
			"messages", "model",
			`model TEXT NOT NULL DEFAULT ''`,
			"adding messages.model",
		},
		{
			"messages", "reasoning_effort",
			`reasoning_effort TEXT NOT NULL DEFAULT ''`,
			"adding messages.reasoning_effort",
		},
		{
			"messages", "token_usage",
			`token_usage TEXT NOT NULL DEFAULT ''`,
			"adding messages.token_usage",
		},
		{
			"messages", "context_tokens",
			`context_tokens INT NOT NULL DEFAULT 0`,
			"adding messages.context_tokens",
		},
		{
			"messages", "output_tokens",
			`output_tokens INT NOT NULL DEFAULT 0`,
			"adding messages.output_tokens",
		},
		{
			"messages", "provider_id",
			`provider_id TEXT NOT NULL DEFAULT ''`,
			"adding messages.provider_id",
		},
		{
			"usage_events", "provider_id",
			`provider_id TEXT NOT NULL DEFAULT ''`,
			"adding usage_events.provider_id",
		},
		{
			"messages", "has_context_tokens",
			`has_context_tokens BOOLEAN NOT NULL DEFAULT FALSE`,
			"adding messages.has_context_tokens",
		},
		{
			"messages", "has_output_tokens",
			`has_output_tokens BOOLEAN NOT NULL DEFAULT FALSE`,
			"adding messages.has_output_tokens",
		},
		{
			"messages", "claude_message_id",
			`claude_message_id TEXT NOT NULL DEFAULT ''`,
			"adding messages.claude_message_id",
		},
		{
			"messages", "claude_request_id",
			`claude_request_id TEXT NOT NULL DEFAULT ''`,
			"adding messages.claude_request_id",
		},
		{
			"tool_calls", "call_index",
			`call_index INT NOT NULL DEFAULT 0`,
			"adding tool_calls.call_index",
		},
		{
			"tool_calls", "file_path",
			`file_path TEXT`,
			"adding tool_calls.file_path",
		},
		{
			"sessions", "prompt_evidence_discarded",
			`prompt_evidence_discarded BOOLEAN NOT NULL DEFAULT FALSE`,
			"adding sessions.prompt_evidence_discarded",
		},
		{
			"sessions", "is_automated",
			`is_automated BOOLEAN NOT NULL DEFAULT FALSE`,
			"adding sessions.is_automated",
		},
		{
			"sessions", "tool_failure_signal_count",
			`tool_failure_signal_count INT NOT NULL DEFAULT 0`,
			"adding sessions.tool_failure_signal_count",
		},
		{
			"sessions", "tool_retry_count",
			`tool_retry_count INT NOT NULL DEFAULT 0`,
			"adding sessions.tool_retry_count",
		},
		{
			"sessions", "edit_churn_count",
			`edit_churn_count INT NOT NULL DEFAULT 0`,
			"adding sessions.edit_churn_count",
		},
		{
			"sessions", "consecutive_failure_max",
			`consecutive_failure_max INT NOT NULL DEFAULT 0`,
			"adding sessions.consecutive_failure_max",
		},
		{
			"sessions", "outcome",
			`outcome TEXT NOT NULL DEFAULT 'unknown'`,
			"adding sessions.outcome",
		},
		{
			"sessions", "outcome_confidence",
			`outcome_confidence TEXT NOT NULL DEFAULT 'low'`,
			"adding sessions.outcome_confidence",
		},
		{
			"sessions", "ended_with_role",
			`ended_with_role TEXT NOT NULL DEFAULT ''`,
			"adding sessions.ended_with_role",
		},
		{
			"sessions", "final_failure_streak",
			`final_failure_streak INT NOT NULL DEFAULT 0`,
			"adding sessions.final_failure_streak",
		},
		{
			"sessions", "signals_pending_since",
			`signals_pending_since TEXT`,
			"adding sessions.signals_pending_since",
		},
		{
			"sessions", "compaction_count",
			`compaction_count INT NOT NULL DEFAULT 0`,
			"adding sessions.compaction_count",
		},
		{
			"sessions", "mid_task_compaction_count",
			`mid_task_compaction_count INT NOT NULL DEFAULT 0`,
			"adding sessions.mid_task_compaction_count",
		},
		{
			"sessions", "context_pressure_max",
			`context_pressure_max DOUBLE PRECISION`,
			"adding sessions.context_pressure_max",
		},
		{
			"sessions", "health_score",
			`health_score INT`,
			"adding sessions.health_score",
		},
		{
			"sessions", "health_grade",
			`health_grade TEXT`,
			"adding sessions.health_grade",
		},
		{
			"sessions", "has_tool_calls",
			`has_tool_calls BOOLEAN NOT NULL DEFAULT FALSE`,
			"adding sessions.has_tool_calls",
		},
		{
			"sessions", "has_context_data",
			`has_context_data BOOLEAN NOT NULL DEFAULT FALSE`,
			"adding sessions.has_context_data",
		},
		{
			"sessions", "quality_signal_version",
			`quality_signal_version INT NOT NULL DEFAULT 0`,
			"adding sessions.quality_signal_version",
		},
		{
			"sessions", "short_prompt_count",
			`short_prompt_count INT NOT NULL DEFAULT 0`,
			"adding sessions.short_prompt_count",
		},
		{
			"sessions", "unstructured_start",
			`unstructured_start BOOLEAN NOT NULL DEFAULT FALSE`,
			"adding sessions.unstructured_start",
		},
		{
			"sessions", "missing_success_criteria_count",
			`missing_success_criteria_count INT NOT NULL DEFAULT 0`,
			"adding sessions.missing_success_criteria_count",
		},
		{
			"sessions", "missing_verification_count",
			`missing_verification_count INT NOT NULL DEFAULT 0`,
			"adding sessions.missing_verification_count",
		},
		{
			"sessions", "duplicate_prompt_count",
			`duplicate_prompt_count INT NOT NULL DEFAULT 0`,
			"adding sessions.duplicate_prompt_count",
		},
		{
			"sessions", "no_code_context_count",
			`no_code_context_count INT NOT NULL DEFAULT 0`,
			"adding sessions.no_code_context_count",
		},
		{
			"sessions", "runaway_tool_loop_count",
			`runaway_tool_loop_count INT NOT NULL DEFAULT 0`,
			"adding sessions.runaway_tool_loop_count",
		},
		{
			"sessions", "data_version",
			`data_version INT NOT NULL DEFAULT 0`,
			"adding sessions.data_version",
		},
		{
			"sessions", "cwd",
			`cwd TEXT NOT NULL DEFAULT ''`,
			"adding sessions.cwd",
		},
		{
			"sessions", "git_branch",
			`git_branch TEXT NOT NULL DEFAULT ''`,
			"adding sessions.git_branch",
		},
		{
			"sessions", "source_session_id",
			`source_session_id TEXT NOT NULL DEFAULT ''`,
			"adding sessions.source_session_id",
		},
		{
			"sessions", "source_version",
			`source_version TEXT NOT NULL DEFAULT ''`,
			"adding sessions.source_version",
		},
		{
			"sessions", "transcript_fidelity",
			`transcript_fidelity TEXT NOT NULL DEFAULT ''`,
			"adding sessions.transcript_fidelity",
		},
		{
			"sessions", "parser_malformed_lines",
			`parser_malformed_lines INT NOT NULL DEFAULT 0`,
			"adding sessions.parser_malformed_lines",
		},
		{
			"sessions", "is_truncated",
			`is_truncated BOOLEAN NOT NULL DEFAULT FALSE`,
			"adding sessions.is_truncated",
		},
		{
			"messages", "source_type",
			`source_type TEXT NOT NULL DEFAULT ''`,
			"adding messages.source_type",
		},
		{
			"messages", "source_subtype",
			`source_subtype TEXT NOT NULL DEFAULT ''`,
			"adding messages.source_subtype",
		},
		{
			"messages", "prompt_source",
			`prompt_source TEXT NOT NULL DEFAULT ''`,
			"adding messages.prompt_source",
		},
		{
			"messages", "source_uuid",
			`source_uuid TEXT NOT NULL DEFAULT ''`,
			"adding messages.source_uuid",
		},
		{
			"messages", "source_parent_uuid",
			`source_parent_uuid TEXT NOT NULL DEFAULT ''`,
			"adding messages.source_parent_uuid",
		},
		{
			"messages", "is_sidechain",
			`is_sidechain BOOLEAN NOT NULL DEFAULT FALSE`,
			"adding messages.is_sidechain",
		},
		{
			"messages", "is_compact_boundary",
			`is_compact_boundary BOOLEAN NOT NULL DEFAULT FALSE`,
			"adding messages.is_compact_boundary",
		},
		{
			"messages", "thinking_text",
			`thinking_text TEXT NOT NULL DEFAULT ''`,
			"adding messages.thinking_text",
		},
		{
			"sessions", "termination_status",
			`termination_status TEXT`,
			"adding sessions.termination_status",
		},
		{
			"sessions", "secret_leak_count",
			`secret_leak_count INTEGER NOT NULL DEFAULT 0`,
			"adding sessions.secret_leak_count",
		},
		{
			"sessions", "secrets_rules_version",
			`secrets_rules_version TEXT NOT NULL DEFAULT ''`,
			"adding sessions.secrets_rules_version",
		},
		{
			"sessions", "session_name",
			`session_name TEXT`,
			"adding sessions.session_name",
		},
		{
			"sessions", "agent_label",
			`agent_label TEXT NOT NULL DEFAULT ''`,
			"adding sessions.agent_label",
		},
		{
			"sessions", "entrypoint",
			`entrypoint TEXT NOT NULL DEFAULT ''`,
			"adding sessions.entrypoint",
		},
		{
			"sessions", "session_kind",
			`session_kind TEXT NOT NULL DEFAULT ''`,
			"adding sessions.session_kind",
		},
		{
			"source_project_identity_observations", "source_archive_id",
			`source_archive_id TEXT NOT NULL DEFAULT ''`,
			"adding source_project_identity_observations.source_archive_id",
		},
		{
			"source_project_identity_observations", "source_archive_salt",
			`source_archive_salt TEXT NOT NULL DEFAULT ''`,
			"adding source_project_identity_observations.source_archive_salt",
		},
		{
			"source_project_identity_observations", "repository_path",
			`repository_path TEXT NOT NULL DEFAULT ''`,
			"adding source_project_identity_observations.repository_path",
		},
		{
			"source_project_identity_observations", "worktree_relationship",
			`worktree_relationship TEXT NOT NULL DEFAULT 'unknown'`,
			"adding source_project_identity_observations.worktree_relationship",
		},
		{
			"source_project_identity_observations", "checkout_state",
			`checkout_state TEXT NOT NULL DEFAULT 'unknown'`,
			"adding source_project_identity_observations.checkout_state",
		},
		{
			"source_project_identity_observations", "git_branch",
			`git_branch TEXT NOT NULL DEFAULT ''`,
			"adding source_project_identity_observations.git_branch",
		},
		{
			"source_project_identity_observations", "remote_resolution",
			`remote_resolution TEXT NOT NULL DEFAULT 'unknown'`,
			"adding source_project_identity_observations.remote_resolution",
		},
		{
			"source_project_identity_observations", "remote_candidate_count",
			`remote_candidate_count INT NOT NULL DEFAULT 0`,
			"adding source_project_identity_observations.remote_candidate_count",
		},
		{
			"sessions", "source_archive_id",
			`source_archive_id TEXT NOT NULL DEFAULT ''`,
			"adding sessions.source_archive_id",
		},
		{
			"sessions", "source_database_generation",
			`source_database_generation TEXT NOT NULL DEFAULT ''`,
			"adding sessions.source_database_generation",
		},
		{
			"sessions", "file_path",
			`file_path TEXT`,
			"adding sessions.file_path",
		},
	}
}
