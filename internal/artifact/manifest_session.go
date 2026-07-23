package artifact

import "go.kenn.io/agentsview/internal/db"

// manifestSession is the manifest wire representation of a session row. It is
// deliberately a separate type from db.Session: manifest bytes feed content
// hashes, so this struct is part of the pinned artifact format and its
// serialized output can never change for an existing value. Keeping it apart
// from the internal DB type means adding a db.Session field cannot silently
// re-hash every exported manifest; extending THIS struct is an explicit wire
// format decision (see TestManifestSessionMatchesDBSessionWireFormat).
type manifestSession struct {
	ID                   string  `json:"id"`
	Project              string  `json:"project"`
	Machine              string  `json:"machine"`
	Agent                string  `json:"agent"`
	AgentLabel           string  `json:"agent_label,omitempty"`
	Entrypoint           string  `json:"entrypoint,omitempty"`
	FirstMessage         *string `json:"first_message"`
	DisplayName          *string `json:"display_name,omitempty"`
	StartedAt            *string `json:"started_at"`
	EndedAt              *string `json:"ended_at"`
	MessageCount         int     `json:"message_count"`
	UserMessageCount     int     `json:"user_message_count"`
	ParentSessionID      *string `json:"parent_session_id,omitempty"`
	RelationshipType     string  `json:"relationship_type,omitempty"`
	TotalOutputTokens    int     `json:"total_output_tokens"`
	PeakContextTokens    int     `json:"peak_context_tokens"`
	HasTotalOutputTokens bool    `json:"has_total_output_tokens"`
	HasPeakContextTokens bool    `json:"has_peak_context_tokens"`
	IsAutomated          bool    `json:"is_automated"`

	ToolFailureSignalCount int                     `json:"tool_failure_signal_count"`
	ToolRetryCount         int                     `json:"tool_retry_count"`
	EditChurnCount         int                     `json:"edit_churn_count"`
	ConsecutiveFailureMax  int                     `json:"consecutive_failure_max"`
	Outcome                string                  `json:"outcome"`
	OutcomeConfidence      string                  `json:"outcome_confidence"`
	EndedWithRole          string                  `json:"ended_with_role"`
	FinalFailureStreak     int                     `json:"final_failure_streak"`
	SignalsPendingSince    *string                 `json:"signals_pending_since,omitempty"`
	CompactionCount        int                     `json:"compaction_count"`
	MidTaskCompactionCount int                     `json:"mid_task_compaction_count"`
	ContextPressureMax     *float64                `json:"context_pressure_max,omitempty"`
	HealthScore            *int                    `json:"health_score,omitempty"`
	HealthGrade            *string                 `json:"health_grade,omitempty"`
	QualitySignals         *manifestQualitySignals `json:"quality_signals,omitempty"`
	SecretLeakCount        int                     `json:"secret_leak_count"`

	Cwd                  string `json:"cwd,omitempty"`
	GitBranch            string `json:"git_branch,omitempty"`
	SourceSessionID      string `json:"source_session_id,omitempty"`
	SourceVersion        string `json:"source_version,omitempty"`
	TranscriptFidelity   string `json:"transcript_fidelity,omitempty"`
	ParserMalformedLines int    `json:"parser_malformed_lines,omitempty"`
	IsTruncated          bool   `json:"is_truncated,omitempty"`

	DeletedAt          *string `json:"deleted_at,omitempty"`
	TerminationStatus  *string `json:"termination_status,omitempty"`
	FilePath           *string `json:"file_path,omitempty"`
	FileSize           *int64  `json:"file_size,omitempty"`
	FileMtime          *int64  `json:"file_mtime,omitempty"`
	FileInode          *int64  `json:"file_inode,omitempty"`
	FileDevice         *int64  `json:"file_device,omitempty"`
	FileHash           *string `json:"file_hash,omitempty"`
	LocalModifiedAt    *string `json:"local_modified_at,omitempty"`
	TranscriptRevision *string `json:"transcript_revision,omitempty"`
	CreatedAt          string  `json:"created_at"`
}

// manifestQualitySignals mirrors db.QualitySignals for the same reason
// manifestSession mirrors db.Session: it appears in hashed manifest bytes.
type manifestQualitySignals struct {
	Version                     int  `json:"version"`
	ShortPromptCount            int  `json:"short_prompt_count"`
	UnstructuredStart           bool `json:"unstructured_start"`
	MissingSuccessCriteriaCount int  `json:"missing_success_criteria_count"`
	MissingVerificationCount    int  `json:"missing_verification_count"`
	DuplicatePromptCount        int  `json:"duplicate_prompt_count"`
	NoCodeContextCount          int  `json:"no_code_context_count"`
	RunawayToolLoopCount        int  `json:"runaway_tool_loop_count"`
}

func manifestSessionFromDB(s db.Session) manifestSession {
	return manifestSession{
		ID:                   s.ID,
		Project:              s.Project,
		Machine:              s.Machine,
		Agent:                s.Agent,
		AgentLabel:           s.AgentLabel,
		Entrypoint:           s.Entrypoint,
		FirstMessage:         s.FirstMessage,
		DisplayName:          s.DisplayName,
		StartedAt:            s.StartedAt,
		EndedAt:              s.EndedAt,
		MessageCount:         s.MessageCount,
		UserMessageCount:     s.UserMessageCount,
		ParentSessionID:      s.ParentSessionID,
		RelationshipType:     s.RelationshipType,
		TotalOutputTokens:    s.TotalOutputTokens,
		PeakContextTokens:    s.PeakContextTokens,
		HasTotalOutputTokens: s.HasTotalOutputTokens,
		HasPeakContextTokens: s.HasPeakContextTokens,
		IsAutomated:          s.IsAutomated,

		ToolFailureSignalCount: s.ToolFailureSignalCount,
		ToolRetryCount:         s.ToolRetryCount,
		EditChurnCount:         s.EditChurnCount,
		ConsecutiveFailureMax:  s.ConsecutiveFailureMax,
		Outcome:                s.Outcome,
		OutcomeConfidence:      s.OutcomeConfidence,
		EndedWithRole:          s.EndedWithRole,
		FinalFailureStreak:     s.FinalFailureStreak,
		SignalsPendingSince:    s.SignalsPendingSince,
		CompactionCount:        s.CompactionCount,
		MidTaskCompactionCount: s.MidTaskCompactionCount,
		ContextPressureMax:     s.ContextPressureMax,
		HealthScore:            s.HealthScore,
		HealthGrade:            s.HealthGrade,
		QualitySignals:         manifestQualitySignalsFromDB(s.QualitySignals),
		SecretLeakCount:        s.SecretLeakCount,

		Cwd:                  s.Cwd,
		GitBranch:            s.GitBranch,
		SourceSessionID:      s.SourceSessionID,
		SourceVersion:        s.SourceVersion,
		TranscriptFidelity:   s.TranscriptFidelity,
		ParserMalformedLines: s.ParserMalformedLines,
		IsTruncated:          s.IsTruncated,

		DeletedAt:          s.DeletedAt,
		TerminationStatus:  s.TerminationStatus,
		FilePath:           s.FilePath,
		FileSize:           s.FileSize,
		FileMtime:          s.FileMtime,
		FileInode:          s.FileInode,
		FileDevice:         s.FileDevice,
		FileHash:           s.FileHash,
		LocalModifiedAt:    s.LocalModifiedAt,
		TranscriptRevision: s.TranscriptRevision,
		CreatedAt:          s.CreatedAt,
	}
}

func (m manifestSession) dbSession() db.Session {
	return db.Session{
		ID:                   m.ID,
		Project:              m.Project,
		Machine:              m.Machine,
		Agent:                m.Agent,
		AgentLabel:           m.AgentLabel,
		Entrypoint:           m.Entrypoint,
		FirstMessage:         m.FirstMessage,
		DisplayName:          m.DisplayName,
		StartedAt:            m.StartedAt,
		EndedAt:              m.EndedAt,
		MessageCount:         m.MessageCount,
		UserMessageCount:     m.UserMessageCount,
		ParentSessionID:      m.ParentSessionID,
		RelationshipType:     m.RelationshipType,
		TotalOutputTokens:    m.TotalOutputTokens,
		PeakContextTokens:    m.PeakContextTokens,
		HasTotalOutputTokens: m.HasTotalOutputTokens,
		HasPeakContextTokens: m.HasPeakContextTokens,
		IsAutomated:          m.IsAutomated,

		ToolFailureSignalCount: m.ToolFailureSignalCount,
		ToolRetryCount:         m.ToolRetryCount,
		EditChurnCount:         m.EditChurnCount,
		ConsecutiveFailureMax:  m.ConsecutiveFailureMax,
		Outcome:                m.Outcome,
		OutcomeConfidence:      m.OutcomeConfidence,
		EndedWithRole:          m.EndedWithRole,
		FinalFailureStreak:     m.FinalFailureStreak,
		SignalsPendingSince:    m.SignalsPendingSince,
		CompactionCount:        m.CompactionCount,
		MidTaskCompactionCount: m.MidTaskCompactionCount,
		ContextPressureMax:     m.ContextPressureMax,
		HealthScore:            m.HealthScore,
		HealthGrade:            m.HealthGrade,
		QualitySignals:         m.QualitySignals.dbQualitySignals(),
		SecretLeakCount:        m.SecretLeakCount,

		Cwd:                  m.Cwd,
		GitBranch:            m.GitBranch,
		SourceSessionID:      m.SourceSessionID,
		SourceVersion:        m.SourceVersion,
		TranscriptFidelity:   m.TranscriptFidelity,
		ParserMalformedLines: m.ParserMalformedLines,
		IsTruncated:          m.IsTruncated,

		DeletedAt:          m.DeletedAt,
		TerminationStatus:  m.TerminationStatus,
		FilePath:           m.FilePath,
		FileSize:           m.FileSize,
		FileMtime:          m.FileMtime,
		FileInode:          m.FileInode,
		FileDevice:         m.FileDevice,
		FileHash:           m.FileHash,
		LocalModifiedAt:    m.LocalModifiedAt,
		TranscriptRevision: m.TranscriptRevision,
		CreatedAt:          m.CreatedAt,
	}
}

func manifestQualitySignalsFromDB(qs *db.QualitySignals) *manifestQualitySignals {
	if qs == nil {
		return nil
	}
	return &manifestQualitySignals{
		Version:                     qs.Version,
		ShortPromptCount:            qs.ShortPromptCount,
		UnstructuredStart:           qs.UnstructuredStart,
		MissingSuccessCriteriaCount: qs.MissingSuccessCriteriaCount,
		MissingVerificationCount:    qs.MissingVerificationCount,
		DuplicatePromptCount:        qs.DuplicatePromptCount,
		NoCodeContextCount:          qs.NoCodeContextCount,
		RunawayToolLoopCount:        qs.RunawayToolLoopCount,
	}
}

func (m *manifestQualitySignals) dbQualitySignals() *db.QualitySignals {
	if m == nil {
		return nil
	}
	return &db.QualitySignals{
		Version:                     m.Version,
		ShortPromptCount:            m.ShortPromptCount,
		UnstructuredStart:           m.UnstructuredStart,
		MissingSuccessCriteriaCount: m.MissingSuccessCriteriaCount,
		MissingVerificationCount:    m.MissingVerificationCount,
		DuplicatePromptCount:        m.DuplicatePromptCount,
		NoCodeContextCount:          m.NoCodeContextCount,
		RunawayToolLoopCount:        m.RunawayToolLoopCount,
	}
}
