package ingest

import "go.kenn.io/agentsview/internal/db"

// ApplySignalFields copies the persisted scalar signal view onto session.
func ApplySignalFields(session *db.Session, update db.SessionSignalUpdate) {
	session.ToolFailureSignalCount = update.ToolFailureSignalCount
	session.ToolRetryCount = update.ToolRetryCount
	session.EditChurnCount = update.EditChurnCount
	session.ConsecutiveFailureMax = update.ConsecutiveFailureMax
	session.Outcome = update.Outcome
	session.OutcomeConfidence = update.OutcomeConfidence
	session.EndedWithRole = update.EndedWithRole
	session.FinalFailureStreak = update.FinalFailureStreak
	session.SignalsPendingSince = cloneSignalPointer(update.SignalsPendingSince)
	session.CompactionCount = update.CompactionCount
	session.MidTaskCompactionCount = update.MidTaskCompactionCount
	session.ContextPressureMax = cloneSignalPointer(update.ContextPressureMax)
	session.HealthScore = cloneSignalPointer(update.HealthScore)
	session.HealthGrade = cloneSignalPointer(update.HealthGrade)
	session.HasToolCalls = update.HasToolCalls
	session.HasContextData = update.HasContextData
	session.SecretLeakCount = update.SecretLeakCount
	session.SecretsRulesVersion = update.SecretsRulesVersion
	session.QualitySignalVersion = update.QualitySignals.Version
	session.ShortPromptCount = update.QualitySignals.ShortPromptCount
	session.UnstructuredStart = update.QualitySignals.UnstructuredStart
	session.MissingSuccessCriteriaCount = update.QualitySignals.MissingSuccessCriteriaCount
	session.MissingVerificationCount = update.QualitySignals.MissingVerificationCount
	session.DuplicatePromptCount = update.QualitySignals.DuplicatePromptCount
	session.NoCodeContextCount = update.QualitySignals.NoCodeContextCount
	session.RunawayToolLoopCount = update.QualitySignals.RunawayToolLoopCount
}

// SignalFields reconstructs the signal update view from persisted session scalars.
func SignalFields(session db.Session) db.SessionSignalUpdate {
	return db.SessionSignalUpdate{
		ToolFailureSignalCount: session.ToolFailureSignalCount,
		ToolRetryCount:         session.ToolRetryCount,
		EditChurnCount:         session.EditChurnCount,
		ConsecutiveFailureMax:  session.ConsecutiveFailureMax,
		Outcome:                session.Outcome,
		OutcomeConfidence:      session.OutcomeConfidence,
		EndedWithRole:          session.EndedWithRole,
		FinalFailureStreak:     session.FinalFailureStreak,
		SignalsPendingSince:    cloneSignalPointer(session.SignalsPendingSince),
		CompactionCount:        session.CompactionCount,
		MidTaskCompactionCount: session.MidTaskCompactionCount,
		ContextPressureMax:     cloneSignalPointer(session.ContextPressureMax),
		HealthScore:            cloneSignalPointer(session.HealthScore),
		HealthGrade:            cloneSignalPointer(session.HealthGrade),
		HasToolCalls:           session.HasToolCalls,
		HasContextData:         session.HasContextData,
		SecretLeakCount:        session.SecretLeakCount,
		SecretsRulesVersion:    session.SecretsRulesVersion,
		QualitySignals: db.QualitySignals{
			Version:                     session.QualitySignalVersion,
			ShortPromptCount:            session.ShortPromptCount,
			UnstructuredStart:           session.UnstructuredStart,
			MissingSuccessCriteriaCount: session.MissingSuccessCriteriaCount,
			MissingVerificationCount:    session.MissingVerificationCount,
			DuplicatePromptCount:        session.DuplicatePromptCount,
			NoCodeContextCount:          session.NoCodeContextCount,
			RunawayToolLoopCount:        session.RunawayToolLoopCount,
		},
	}
}

func cloneSignalPointer[T any](value *T) *T {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}
