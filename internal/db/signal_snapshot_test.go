package db

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestReplaceSessionSignalsIfRevisionRejectsStaleSnapshot(t *testing.T) {
	require := require.New(t)

	d := testDB(t)
	insertSession(t, d, "signal-race", "proj")

	sess, err := d.GetSessionFull(t.Context(), "signal-race")
	require.NoError(err)
	require.NotNil(sess)
	require.NotNil(sess.TranscriptRevision)
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

	applied, err := d.ReplaceSessionSignalsIfRevision(
		"signal-race", currentRevision+"-stale", []SecretFinding{finding},
		update, state,
	)
	require.NoError(err)
	require.False(applied)

	afterReject, err := d.GetSessionFull(t.Context(), "signal-race")
	require.NoError(err)
	require.Equal(initialOutcome, afterReject.Outcome)
	_, ok, err := d.GetSessionSignalState("signal-race")
	require.NoError(err)
	require.False(ok)
	findings, err := d.SessionSecretFindings(t.Context(), "signal-race")
	require.NoError(err)
	require.Empty(findings)

	applied, err = d.ReplaceSessionSignalsIfRevision(
		"signal-race", currentRevision, []SecretFinding{finding}, update, state,
	)
	require.NoError(err)
	require.True(applied)

	afterApply, err := d.GetSessionFull(t.Context(), "signal-race")
	require.NoError(err)
	require.Equal("completed", afterApply.Outcome)
	storedState, ok, err := d.GetSessionSignalState("signal-race")
	require.NoError(err)
	require.True(ok)
	require.Equal(currentRevision, storedState.TranscriptRevision)
	require.Equal([]byte("stale-state"), storedState.State)
	findings, err = d.SessionSecretFindings(t.Context(), "signal-race")
	require.NoError(err)
	require.Len(findings, 1)
}

func TestReplaceSessionSignalsIfInputsMatchRejectsMetadataOnlyRace(t *testing.T) {
	require := require.New(t)

	d := testDB(t)
	insertSession(t, d, "signal-metadata-race", "proj")

	sess, err := d.GetSessionFull(t.Context(), "signal-metadata-race")
	require.NoError(err)
	require.NotNil(sess)
	expected, err := SignalInputSnapshot(*sess)
	require.NoError(err)
	initialOutcome := sess.Outcome

	endedAt := "2026-08-18T12:00:00Z"
	_, err = d.getWriter().Exec(`
		UPDATE sessions
		SET ended_at = ?, is_automated = 1, message_count = 7,
		    peak_context_tokens = 12345, has_peak_context_tokens = 1
		WHERE id = ?`, endedAt, "signal-metadata-race")
	require.NoError(err)

	update := SessionSignalUpdate{
		Outcome: "stale-result", OutcomeConfidence: "high",
		SecretsRulesVersion: "rules-v1",
		QualitySignals:      QualitySignals{Version: CurrentQualitySignalVersion},
	}
	state := SessionSignalState{
		State: []byte("stale-state"), SignalVersion: CurrentQualitySignalVersion,
	}
	applied, err := d.ReplaceSessionSignalsIfInputsMatch(
		"signal-metadata-race", expected, nil, update, state,
	)
	require.NoError(err)
	require.False(applied,
		"metadata-only changes must invalidate a full signal snapshot")

	afterReject, err := d.GetSessionFull(t.Context(), "signal-metadata-race")
	require.NoError(err)
	require.Equal(initialOutcome, afterReject.Outcome)
	_, ok, err := d.GetSessionSignalState("signal-metadata-race")
	require.NoError(err)
	require.False(ok)

	fresh, err := SignalInputSnapshot(*afterReject)
	require.NoError(err)
	update.Outcome = "fresh-result"
	state.State = []byte("fresh-state")
	applied, err = d.ReplaceSessionSignalsIfInputsMatch(
		"signal-metadata-race", fresh, nil, update, state,
	)
	require.NoError(err)
	require.True(applied)

	afterApply, err := d.GetSessionFull(t.Context(), "signal-metadata-race")
	require.NoError(err)
	require.Equal("fresh-result", afterApply.Outcome)
	stored, ok, err := d.GetSessionSignalState("signal-metadata-race")
	require.NoError(err)
	require.True(ok)
	require.Equal(fresh.TranscriptRevision, stored.TranscriptRevision)
	require.Equal([]byte("fresh-state"), stored.State)
}
