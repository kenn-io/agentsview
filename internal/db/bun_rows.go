package db

import (
	"encoding/json"
	"fmt"
	"time"

	"go.kenn.io/agentsview/internal/db/bunmodel"
)

func timestampToBunRow(value string) (*bunmodel.Timestamp, error) {
	if value == "" {
		return nil, nil
	}
	timestamp, err := bunmodel.ParseTimestamp(value)
	if err != nil {
		return nil, err
	}
	return &timestamp, nil
}

func timestampPtrToBunRow(value *string) (*bunmodel.Timestamp, error) {
	if value == nil {
		return nil, nil
	}
	return timestampToBunRow(*value)
}

func timestampFromBunRow(value *bunmodel.Timestamp) *string {
	if value == nil || value.IsZero() {
		return nil
	}
	formatted := value.Time.UTC().Format(time.RFC3339Nano)
	return &formatted
}

func requiredTimestampToBunRow(value string) (bunmodel.Timestamp, error) {
	if value == "" {
		return bunmodel.Timestamp{}, fmt.Errorf("timestamp is empty")
	}
	return bunmodel.ParseTimestamp(value)
}

func requiredTimestampFromBunRow(value bunmodel.Timestamp) string {
	if value.IsZero() {
		return ""
	}
	return value.Time.UTC().Format(time.RFC3339Nano)
}

func sessionToBunRow(session Session) (bunmodel.Session, error) {
	session.Project = SanitizeUTF8(session.Project)
	session.Machine = SanitizeUTF8(session.Machine)
	session.Agent = SanitizeUTF8(session.Agent)
	session.AgentLabel = SanitizeUTF8(session.AgentLabel)
	session.Entrypoint = SanitizeUTF8(session.Entrypoint)
	session.SessionKind = SanitizeUTF8(session.SessionKind)
	session.FirstMessage = sanitizeCanonicalSessionStringPtr(session.FirstMessage)
	session.DisplayName = sanitizeCanonicalSessionStringPtr(session.DisplayName)
	session.SessionName = sanitizeCanonicalSessionStringPtr(session.SessionName)
	session.ParentSessionID = sanitizeCanonicalSessionStringPtr(session.ParentSessionID)
	session.ParserParentSessionID = sanitizeCanonicalSessionStringPtr(session.ParserParentSessionID)
	session.RelationshipType = SanitizeUTF8(session.RelationshipType)
	session.Outcome = SanitizeUTF8(session.Outcome)
	session.OutcomeConfidence = SanitizeUTF8(session.OutcomeConfidence)
	session.EndedWithRole = SanitizeUTF8(session.EndedWithRole)
	session.HealthGrade = sanitizeCanonicalSessionStringPtr(session.HealthGrade)
	session.SecretsRulesVersion = SanitizeUTF8(session.SecretsRulesVersion)
	session.Cwd = SanitizeUTF8(session.Cwd)
	session.GitBranch = SanitizeUTF8(session.GitBranch)
	session.SourceSessionID = SanitizeUTF8(session.SourceSessionID)
	session.SourceVersion = SanitizeUTF8(session.SourceVersion)
	session.TranscriptFidelity = SanitizeUTF8(session.TranscriptFidelity)
	session.DeletionCause = sanitizeCanonicalSessionStringPtr(session.DeletionCause)
	session.TerminationStatus = sanitizeCanonicalSessionStringPtr(session.TerminationStatus)
	session.FilePath = sanitizeCanonicalSessionStringPtr(session.FilePath)
	session.FileHash = sanitizeCanonicalSessionStringPtr(session.FileHash)
	if session.TranscriptRevision != nil {
		clean := SanitizeUTF8(*session.TranscriptRevision)
		if clean == "" {
			session.TranscriptRevision = nil
		} else if clean != *session.TranscriptRevision {
			session.TranscriptRevision = &clean
		}
	}
	outcome := session.Outcome
	if outcome == "" {
		outcome = "unknown"
	}
	outcomeConfidence := session.OutcomeConfidence
	if outcomeConfidence == "" {
		outcomeConfidence = "low"
	}
	row := bunmodel.Session{
		ID:                    session.ID,
		Project:               session.Project,
		Machine:               session.Machine,
		Agent:                 session.Agent,
		AgentLabel:            session.AgentLabel,
		Entrypoint:            session.Entrypoint,
		SessionKind:           session.SessionKind,
		FirstMessage:          session.FirstMessage,
		DisplayName:           session.DisplayName,
		SessionName:           session.SessionName,
		MessageCount:          session.MessageCount,
		UserMessageCount:      session.UserMessageCount,
		ParentSessionID:       session.ParentSessionID,
		ParserParentSessionID: session.ParserParentSessionID,
		RelationshipType:      session.RelationshipType,
		TotalOutputTokens:     session.TotalOutputTokens,
		PeakContextTokens:     session.PeakContextTokens,
		HasTotalOutputTokens:  session.HasTotalOutputTokens,
		HasPeakContextTokens:  session.HasPeakContextTokens,
		IsAutomated:           session.IsAutomated,

		ToolFailureSignalCount:      session.ToolFailureSignalCount,
		ToolRetryCount:              session.ToolRetryCount,
		EditChurnCount:              session.EditChurnCount,
		ConsecutiveFailureMax:       session.ConsecutiveFailureMax,
		Outcome:                     outcome,
		OutcomeConfidence:           outcomeConfidence,
		EndedWithRole:               session.EndedWithRole,
		FinalFailureStreak:          session.FinalFailureStreak,
		CompactionCount:             session.CompactionCount,
		MidTaskCompactionCount:      session.MidTaskCompactionCount,
		ContextPressureMax:          session.ContextPressureMax,
		HealthScore:                 session.HealthScore,
		HealthGrade:                 session.HealthGrade,
		HasToolCalls:                session.HasToolCalls,
		HasContextData:              session.HasContextData,
		SecretLeakCount:             session.SecretLeakCount,
		SecretsRulesVersion:         session.SecretsRulesVersion,
		QualitySignalVersion:        session.QualitySignalVersion,
		ShortPromptCount:            session.ShortPromptCount,
		UnstructuredStart:           session.UnstructuredStart,
		MissingSuccessCriteriaCount: session.MissingSuccessCriteriaCount,
		MissingVerificationCount:    session.MissingVerificationCount,
		DuplicatePromptCount:        session.DuplicatePromptCount,
		NoCodeContextCount:          session.NoCodeContextCount,
		RunawayToolLoopCount:        session.RunawayToolLoopCount,
		DataVersion:                 session.DataVersion,
		Cwd:                         session.Cwd,
		GitBranch:                   session.GitBranch,
		SourceSessionID:             session.SourceSessionID,
		SourceVersion:               session.SourceVersion,
		TranscriptFidelity:          session.TranscriptFidelity,
		ParserMalformedLines:        session.ParserMalformedLines,
		IsTruncated:                 session.IsTruncated,

		DeletionCause:      session.DeletionCause,
		TerminationStatus:  session.TerminationStatus,
		FilePath:           session.FilePath,
		FileSize:           session.FileSize,
		FileMtime:          session.FileMtime,
		FileInode:          session.FileInode,
		FileDevice:         session.FileDevice,
		FileHash:           session.FileHash,
		TranscriptRevision: "0",

		SourceArchiveID:          session.SourceArchiveID,
		SourceDatabaseGeneration: session.SourceDatabaseGeneration,
	}
	if session.TranscriptRevision != nil {
		row.TranscriptRevision = *session.TranscriptRevision
	}

	optionalTimestamps := [...]struct {
		name  string
		value *string
		dest  **bunmodel.Timestamp
	}{
		{"started_at", session.StartedAt, &row.StartedAt},
		{"ended_at", session.EndedAt, &row.EndedAt},
		{"signals_pending_since", session.SignalsPendingSince, &row.SignalsPendingSince},
		{"deleted_at", session.DeletedAt, &row.DeletedAt},
		{"local_modified_at", session.LocalModifiedAt, &row.LocalModifiedAt},
	}
	for _, field := range optionalTimestamps {
		value, err := timestampPtrToBunRow(field.value)
		if err != nil {
			return bunmodel.Session{}, fmt.Errorf(
				"session %q %s: %w", session.ID, field.name, err,
			)
		}
		*field.dest = value
	}
	createdAt, err := requiredTimestampToBunRow(session.CreatedAt)
	if err != nil {
		return bunmodel.Session{}, fmt.Errorf(
			"session %q created_at: %w", session.ID, err,
		)
	}
	row.CreatedAt = createdAt
	return row, nil
}

func sanitizeCanonicalSessionStringPtr(value *string) *string {
	if value == nil {
		return nil
	}
	clean := SanitizeUTF8(*value)
	if clean == "" {
		return nil
	}
	if clean == *value {
		return value
	}
	return &clean
}

func sessionFromBunRow(row bunmodel.Session) Session {
	return Session{
		ID:                    row.ID,
		Project:               row.Project,
		Machine:               row.Machine,
		Agent:                 row.Agent,
		AgentLabel:            row.AgentLabel,
		Entrypoint:            row.Entrypoint,
		SessionKind:           row.SessionKind,
		FirstMessage:          row.FirstMessage,
		DisplayName:           row.DisplayName,
		SessionName:           row.SessionName,
		StartedAt:             timestampFromBunRow(row.StartedAt),
		EndedAt:               timestampFromBunRow(row.EndedAt),
		MessageCount:          row.MessageCount,
		UserMessageCount:      row.UserMessageCount,
		ParentSessionID:       row.ParentSessionID,
		ParserParentSessionID: row.ParserParentSessionID,
		RelationshipType:      row.RelationshipType,
		TotalOutputTokens:     row.TotalOutputTokens,
		PeakContextTokens:     row.PeakContextTokens,
		HasTotalOutputTokens:  row.HasTotalOutputTokens,
		HasPeakContextTokens:  row.HasPeakContextTokens,
		IsAutomated:           row.IsAutomated,

		ToolFailureSignalCount:      row.ToolFailureSignalCount,
		ToolRetryCount:              row.ToolRetryCount,
		EditChurnCount:              row.EditChurnCount,
		ConsecutiveFailureMax:       row.ConsecutiveFailureMax,
		Outcome:                     row.Outcome,
		OutcomeConfidence:           row.OutcomeConfidence,
		EndedWithRole:               row.EndedWithRole,
		FinalFailureStreak:          row.FinalFailureStreak,
		SignalsPendingSince:         timestampFromBunRow(row.SignalsPendingSince),
		CompactionCount:             row.CompactionCount,
		MidTaskCompactionCount:      row.MidTaskCompactionCount,
		ContextPressureMax:          row.ContextPressureMax,
		HealthScore:                 row.HealthScore,
		HealthGrade:                 row.HealthGrade,
		HasToolCalls:                row.HasToolCalls,
		HasContextData:              row.HasContextData,
		SecretLeakCount:             row.SecretLeakCount,
		SecretsRulesVersion:         row.SecretsRulesVersion,
		QualitySignalVersion:        row.QualitySignalVersion,
		ShortPromptCount:            row.ShortPromptCount,
		UnstructuredStart:           row.UnstructuredStart,
		MissingSuccessCriteriaCount: row.MissingSuccessCriteriaCount,
		MissingVerificationCount:    row.MissingVerificationCount,
		DuplicatePromptCount:        row.DuplicatePromptCount,
		NoCodeContextCount:          row.NoCodeContextCount,
		RunawayToolLoopCount:        row.RunawayToolLoopCount,
		DataVersion:                 row.DataVersion,
		Cwd:                         row.Cwd,
		GitBranch:                   row.GitBranch,
		SourceSessionID:             row.SourceSessionID,
		SourceVersion:               row.SourceVersion,
		TranscriptFidelity:          row.TranscriptFidelity,
		ParserMalformedLines:        row.ParserMalformedLines,
		IsTruncated:                 row.IsTruncated,

		DeletedAt:          timestampFromBunRow(row.DeletedAt),
		DeletionCause:      row.DeletionCause,
		TerminationStatus:  row.TerminationStatus,
		FilePath:           row.FilePath,
		FileSize:           row.FileSize,
		FileMtime:          row.FileMtime,
		FileInode:          row.FileInode,
		FileDevice:         row.FileDevice,
		FileHash:           row.FileHash,
		LocalModifiedAt:    timestampFromBunRow(row.LocalModifiedAt),
		TranscriptRevision: &row.TranscriptRevision,
		CreatedAt:          requiredTimestampFromBunRow(row.CreatedAt),

		SourceArchiveID:          row.SourceArchiveID,
		SourceDatabaseGeneration: row.SourceDatabaseGeneration,
	}
}

func messageToBunRow(message Message) (bunmodel.Message, error) {
	var id *int64
	if message.ID != 0 {
		id = &message.ID
	}
	timestamp, err := timestampToBunRow(message.Timestamp)
	if err != nil {
		return bunmodel.Message{}, fmt.Errorf(
			"message %q ordinal %d timestamp: %w",
			message.SessionID, message.Ordinal, err,
		)
	}
	return bunmodel.Message{
		ID:                id,
		SessionID:         message.SessionID,
		Ordinal:           message.Ordinal,
		Role:              message.Role,
		Content:           message.Content,
		ThinkingText:      message.ThinkingText,
		Timestamp:         timestamp,
		HasThinking:       message.HasThinking,
		HasToolUse:        message.HasToolUse,
		ContentLength:     message.ContentLength,
		IsSystem:          message.IsSystem,
		Model:             message.Model,
		TokenUsage:        append(json.RawMessage(nil), message.TokenUsage...),
		ContextTokens:     message.ContextTokens,
		OutputTokens:      message.OutputTokens,
		HasContextTokens:  message.HasContextTokens,
		HasOutputTokens:   message.HasOutputTokens,
		ClaudeMessageID:   message.ClaudeMessageID,
		ClaudeRequestID:   message.ClaudeRequestID,
		SourceType:        message.SourceType,
		SourceSubtype:     message.SourceSubtype,
		PromptSource:      message.PromptSource,
		SourceUUID:        message.SourceUUID,
		SourceParentUUID:  message.SourceParentUUID,
		IsSidechain:       message.IsSidechain,
		IsCompactBoundary: message.IsCompactBoundary,
	}, nil
}

func messageFromBunRow(row bunmodel.Message) Message {
	var id int64
	if row.ID != nil {
		id = *row.ID
	}
	return Message{
		ID:                id,
		SessionID:         row.SessionID,
		Ordinal:           row.Ordinal,
		Role:              row.Role,
		Content:           row.Content,
		ThinkingText:      row.ThinkingText,
		Timestamp:         requiredTimestampFromBunRowPtr(row.Timestamp),
		HasThinking:       row.HasThinking,
		HasToolUse:        row.HasToolUse,
		ContentLength:     row.ContentLength,
		IsSystem:          row.IsSystem,
		Model:             row.Model,
		TokenUsage:        append(json.RawMessage(nil), row.TokenUsage...),
		ContextTokens:     row.ContextTokens,
		OutputTokens:      row.OutputTokens,
		HasContextTokens:  row.HasContextTokens,
		HasOutputTokens:   row.HasOutputTokens,
		ClaudeMessageID:   row.ClaudeMessageID,
		ClaudeRequestID:   row.ClaudeRequestID,
		SourceType:        row.SourceType,
		SourceSubtype:     row.SourceSubtype,
		PromptSource:      row.PromptSource,
		SourceUUID:        row.SourceUUID,
		SourceParentUUID:  row.SourceParentUUID,
		IsSidechain:       row.IsSidechain,
		IsCompactBoundary: row.IsCompactBoundary,
	}
}

func requiredTimestampFromBunRowPtr(value *bunmodel.Timestamp) string {
	formatted := timestampFromBunRow(value)
	if formatted == nil {
		return ""
	}
	return *formatted
}
