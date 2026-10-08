package db

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestReplaceSessionSignalsIfRevisionRejectsStaleSnapshot(t *testing.T) {
	d := testDB(t)
	insertSession(t, d, "signal-race", "proj")

	sess, err := d.GetSessionFull(t.Context(), "signal-race")
	require.NoError(t, err)
	require.NotNil(t, sess)
	require.NotNil(t, sess.TranscriptRevision)
	currentRevision := *sess.TranscriptRevision
	initialOutcome := sess.Outcome

	update := SessionSignalUpdate{
		Outcome:             "completed",
		OutcomeConfidence:   "high",
		SecretLeakCount:     1,
		SecretsRulesVersion: "rules-v1",
		QualitySignals: QualitySignals{
			Version: CurrentQualitySignalVersion,
		},
	}
	finding := SecretFinding{
		RuleName:       "test-secret",
		Confidence:     "definite",
		LocationKind:   "message",
		MessageOrdinal: 0,
		MatchEnd:       4,
		RedactedMatch:  "****",
		RulesVersion:   "rules-v1",
	}
	state := SessionSignalState{
		State:         []byte("stale-state"),
		SignalVersion: CurrentQualitySignalVersion,
	}

	applied, err := d.ReplaceSessionSignalsIfRevision(t.Context(),
		"signal-race", currentRevision+"-stale", []SecretFinding{finding},
		update, state,
	)
	require.NoError(t, err)
	require.False(t, applied)

	afterReject, err := d.GetSessionFull(t.Context(), "signal-race")
	require.NoError(t, err)
	require.Equal(t, initialOutcome, afterReject.Outcome)
	_, ok, err := d.GetSessionSignalState(t.Context(), "signal-race")
	require.NoError(t, err)
	require.False(t, ok)
	findings, err := d.SessionSecretFindings(t.Context(), "signal-race")
	require.NoError(t, err)
	require.Empty(t, findings)

	applied, err = d.ReplaceSessionSignalsIfRevision(t.Context(),
		"signal-race", currentRevision, []SecretFinding{finding}, update, state,
	)
	require.NoError(t, err)
	require.True(t, applied)

	afterApply, err := d.GetSessionFull(t.Context(), "signal-race")
	require.NoError(t, err)
	require.Equal(t, "completed", afterApply.Outcome)
	storedState, ok, err := d.GetSessionSignalState(t.Context(), "signal-race")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, currentRevision, storedState.TranscriptRevision)
	require.Equal(t, []byte("stale-state"), storedState.State)
	findings, err = d.SessionSecretFindings(t.Context(), "signal-race")
	require.NoError(t, err)
	require.Len(t, findings, 1)
}

func TestReplaceSessionSignalsIfInputsMatchRejectsMetadataOnlyRace(t *testing.T) {
	for _, mutation := range []string{
		"ended_at = '2026-08-18T12:00:00Z', is_automated = 1, message_count = 7, peak_context_tokens = 12345, has_peak_context_tokens = 1",
		"termination_status = 'clean'",
	} {
		t.Run(mutation, func(t *testing.T) {
			d := testDB(t)
			insertSession(t, d, "signal-metadata-race", "proj")

			insertMessages(t, d, Message{SessionID: "signal-metadata-race", Ordinal: 0, Role: "assistant", ToolCalls: []ToolCall{{ToolName: "Grep", ObservedOutcome: new("empty")}}})
			sess, err := d.GetSessionFull(t.Context(), "signal-metadata-race")
			require.NoError(t, err)
			require.NotNil(t, sess)
			expected, err := SignalInputSnapshot(*sess)
			require.NoError(t, err)
			initialOutcome := sess.Outcome

			_, err = d.getWriter().Exec(t.Context(), "UPDATE sessions SET "+mutation+" WHERE id = ?", "signal-metadata-race")
			require.NoError(t, err)

			update := SessionSignalUpdate{
				Outcome: "stale-result", OutcomeConfidence: "high",
				ToolObservations:    []ToolObservation{{Outcome: "unknown", Repeat: "none"}},
				SecretsRulesVersion: "rules-v1",
				QualitySignals:      QualitySignals{Version: CurrentQualitySignalVersion},
			}
			state := SessionSignalState{
				State: []byte("stale-state"), SignalVersion: CurrentQualitySignalVersion,
			}
			applied, err := d.ReplaceSessionSignalsIfInputsMatch(t.Context(),
				"signal-metadata-race", expected, nil, update, state,
			)
			require.NoError(t, err)
			require.False(t, applied,
				"metadata-only changes must invalidate a full signal snapshot")

			afterReject, err := d.GetSessionFull(t.Context(), "signal-metadata-race")
			require.NoError(t, err)
			require.Equal(t, initialOutcome, afterReject.Outcome)
			messages, err := d.GetAllMessages(t.Context(), "signal-metadata-race")
			require.NoError(t, err)
			require.Equal(t, new("empty"), messages[0].ToolCalls[0].ObservedOutcome)
			_, ok, err := d.GetSessionSignalState(t.Context(), "signal-metadata-race")
			require.NoError(t, err)
			require.False(t, ok)

			fresh, err := SignalInputSnapshot(*afterReject)
			require.NoError(t, err)
			update.Outcome = "fresh-result"
			state.State = []byte("fresh-state")
			applied, err = d.ReplaceSessionSignalsIfInputsMatch(t.Context(),
				"signal-metadata-race", fresh, nil, update, state,
			)
			require.NoError(t, err)
			require.True(t, applied)

			afterApply, err := d.GetSessionFull(t.Context(), "signal-metadata-race")
			require.NoError(t, err)
			require.Equal(t, "fresh-result", afterApply.Outcome)
			stored, ok, err := d.GetSessionSignalState(t.Context(), "signal-metadata-race")
			require.NoError(t, err)
			require.True(t, ok)
			require.Equal(t, fresh.TranscriptRevision, stored.TranscriptRevision)
			require.Equal(t, []byte("fresh-state"), stored.State)
		})
	}
}
