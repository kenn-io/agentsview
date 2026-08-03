package bunmodel

import "github.com/uptrace/bun"

// Session is the common durable session row. The source provenance fields are
// required; adapters stamp them before writes reach the shared store.
type Session struct {
	bun.BaseModel `bun:"table:sessions"`

	ID                    string     `bun:"id,pk"`
	Project               string     `bun:"project,notnull"`
	Machine               string     `bun:"machine,notnull"`
	Agent                 string     `bun:"agent,notnull"`
	AgentLabel            string     `bun:"agent_label,notnull,default:''"`
	Entrypoint            string     `bun:"entrypoint,notnull,default:''"`
	SessionKind           string     `bun:"session_kind,notnull,default:''"`
	FirstMessage          *string    `bun:"first_message,nullzero"`
	DisplayName           *string    `bun:"display_name,nullzero"`
	SessionName           *string    `bun:"session_name,nullzero"`
	StartedAt             *Timestamp `bun:"started_at,type:TIMESTAMPTZ,nullzero"`
	EndedAt               *Timestamp `bun:"ended_at,type:TIMESTAMPTZ,nullzero"`
	MessageCount          int        `bun:"message_count,notnull,default:0"`
	UserMessageCount      int        `bun:"user_message_count,notnull,default:0"`
	ParentSessionID       *string    `bun:"parent_session_id,nullzero"`
	ParserParentSessionID *string    `bun:"parser_parent_session_id,nullzero"`
	RelationshipType      string     `bun:"relationship_type,notnull,default:''"`
	TotalOutputTokens     int        `bun:"total_output_tokens,notnull,default:0"`
	PeakContextTokens     int        `bun:"peak_context_tokens,notnull,default:0"`
	HasTotalOutputTokens  bool       `bun:"has_total_output_tokens,notnull,default:false"`
	HasPeakContextTokens  bool       `bun:"has_peak_context_tokens,notnull,default:false"`
	IsAutomated           bool       `bun:"is_automated,notnull,default:false"`

	ToolFailureSignalCount      int        `bun:"tool_failure_signal_count,notnull,default:0"`
	ToolRetryCount              int        `bun:"tool_retry_count,notnull,default:0"`
	EditChurnCount              int        `bun:"edit_churn_count,notnull,default:0"`
	ConsecutiveFailureMax       int        `bun:"consecutive_failure_max,notnull,default:0"`
	Outcome                     string     `bun:"outcome,notnull,default:'unknown'"`
	OutcomeConfidence           string     `bun:"outcome_confidence,notnull,default:'low'"`
	EndedWithRole               string     `bun:"ended_with_role,notnull,default:''"`
	FinalFailureStreak          int        `bun:"final_failure_streak,notnull,default:0"`
	SignalsPendingSince         *Timestamp `bun:"signals_pending_since,type:TIMESTAMPTZ,nullzero"`
	CompactionCount             int        `bun:"compaction_count,notnull,default:0"`
	MidTaskCompactionCount      int        `bun:"mid_task_compaction_count,notnull,default:0"`
	ContextPressureMax          *float64   `bun:"context_pressure_max,nullzero"`
	HealthScore                 *int       `bun:"health_score,nullzero"`
	HealthGrade                 *string    `bun:"health_grade,nullzero"`
	HasToolCalls                bool       `bun:"has_tool_calls,notnull,default:false"`
	HasContextData              bool       `bun:"has_context_data,notnull,default:false"`
	SecretLeakCount             int        `bun:"secret_leak_count,notnull,default:0"`
	SecretsRulesVersion         string     `bun:"secrets_rules_version,notnull,default:''"`
	QualitySignalVersion        int        `bun:"quality_signal_version,notnull,default:0"`
	ShortPromptCount            int        `bun:"short_prompt_count,notnull,default:0"`
	UnstructuredStart           bool       `bun:"unstructured_start,notnull,default:false"`
	MissingSuccessCriteriaCount int        `bun:"missing_success_criteria_count,notnull,default:0"`
	MissingVerificationCount    int        `bun:"missing_verification_count,notnull,default:0"`
	DuplicatePromptCount        int        `bun:"duplicate_prompt_count,notnull,default:0"`
	NoCodeContextCount          int        `bun:"no_code_context_count,notnull,default:0"`
	RunawayToolLoopCount        int        `bun:"runaway_tool_loop_count,notnull,default:0"`
	DataVersion                 int        `bun:"data_version,notnull,default:0"`
	Cwd                         string     `bun:"cwd,notnull,default:''"`
	GitBranch                   string     `bun:"git_branch,notnull,default:''"`
	SourceSessionID             string     `bun:"source_session_id,notnull,default:''"`
	SourceVersion               string     `bun:"source_version,notnull,default:''"`
	TranscriptFidelity          string     `bun:"transcript_fidelity,notnull,default:''"`
	ParserMalformedLines        int        `bun:"parser_malformed_lines,notnull,default:0"`
	IsTruncated                 bool       `bun:"is_truncated,notnull,default:false"`

	DeletedAt          *Timestamp `bun:"deleted_at,type:TIMESTAMPTZ,nullzero"`
	DeletionCause      *string    `bun:"deletion_cause,nullzero"`
	TerminationStatus  *string    `bun:"termination_status,nullzero"`
	FilePath           *string    `bun:"file_path,nullzero"`
	FileSize           *int64     `bun:"file_size,nullzero"`
	FileMtime          *int64     `bun:"file_mtime,nullzero"`
	FileInode          *int64     `bun:"file_inode,nullzero"`
	FileDevice         *int64     `bun:"file_device,nullzero"`
	FileHash           *string    `bun:"file_hash,nullzero"`
	LocalModifiedAt    *Timestamp `bun:"local_modified_at,type:TIMESTAMPTZ,nullzero"`
	TranscriptRevision *string    `bun:"transcript_revision,nullzero"`
	CreatedAt          Timestamp  `bun:"created_at,type:TIMESTAMPTZ,notnull"`

	SourceArchiveID          string `bun:"source_archive_id,notnull"`
	SourceDatabaseGeneration string `bun:"source_database_generation,notnull"`
}
