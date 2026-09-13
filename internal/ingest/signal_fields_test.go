package ingest

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"go.kenn.io/agentsview/internal/db"
)

func TestSignalFieldsRoundTripEveryPersistedScalar(t *testing.T) {
	pressure := 0.75
	score := 82
	grade := "B"
	pending := "2026-01-02T12:00:00Z"
	session := db.Session{
		ToolFailureSignalCount: 1, ToolRetryCount: 2,
		EditChurnCount: 3, ConsecutiveFailureMax: 4,
		Outcome: "completed", OutcomeConfidence: "high",
		EndedWithRole: "assistant", FinalFailureStreak: 5,
		SignalsPendingSince: &pending,
		CompactionCount:     6, MidTaskCompactionCount: 7,
		ContextPressureMax: &pressure, HealthScore: &score, HealthGrade: &grade,
		HasToolCalls: true, HasContextData: true,
		SecretLeakCount: 8, SecretsRulesVersion: "rules-v2",
		QualitySignalVersion: 9, ShortPromptCount: 10,
		UnstructuredStart: true, MissingSuccessCriteriaCount: 11,
		MissingVerificationCount: 12, DuplicatePromptCount: 13,
		NoCodeContextCount: 14, RunawayToolLoopCount: 15,
	}

	got := SignalFields(session)
	assert.Equal(t, db.SessionSignalUpdate{
		ToolFailureSignalCount: 1, ToolRetryCount: 2,
		EditChurnCount: 3, ConsecutiveFailureMax: 4,
		Outcome: "completed", OutcomeConfidence: "high",
		EndedWithRole: "assistant", FinalFailureStreak: 5,
		SignalsPendingSince: &pending,
		CompactionCount:     6, MidTaskCompactionCount: 7,
		ContextPressureMax: &pressure, HealthScore: &score, HealthGrade: &grade,
		HasToolCalls: true, HasContextData: true,
		SecretLeakCount: 8, SecretsRulesVersion: "rules-v2",
		QualitySignals: db.QualitySignals{
			Version: 9, ShortPromptCount: 10, UnstructuredStart: true,
			MissingSuccessCriteriaCount: 11, MissingVerificationCount: 12,
			DuplicatePromptCount: 13, NoCodeContextCount: 14,
			RunawayToolLoopCount: 15,
		},
	}, got)

	var roundTrip db.Session
	ApplySignalFields(&roundTrip, got)
	assert.Equal(t, session, roundTrip)
}

func TestSignalFieldsCopiesNullableValues(t *testing.T) {
	pressure := 0.5
	score := 60
	grade := "C"
	pending := "pending"
	update := db.SessionSignalUpdate{
		SignalsPendingSince: &pending, ContextPressureMax: &pressure,
		HealthScore: &score, HealthGrade: &grade,
	}
	var session db.Session
	ApplySignalFields(&session, update)
	*update.SignalsPendingSince = "changed"
	*update.ContextPressureMax = 1
	*update.HealthScore = 1
	*update.HealthGrade = "F"
	assert.Equal(t, "pending", *session.SignalsPendingSince)
	assert.Equal(t, 0.5, *session.ContextPressureMax)
	assert.Equal(t, 60, *session.HealthScore)
	assert.Equal(t, "C", *session.HealthGrade)

	got := SignalFields(session)
	*session.SignalsPendingSince = "again"
	assert.Equal(t, "pending", *got.SignalsPendingSince)
}
