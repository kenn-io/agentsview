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
	d := testDB(t)
	insertSession(t, d, "signal-metadata-race", "proj")

	sess, err := d.GetSessionFull(t.Context(), "signal-metadata-race")
	require.NoError(t, err)
	require.NotNil(t, sess)
	expected, err := SignalInputSnapshot(*sess)
	require.NoError(t, err)
	initialOutcome := sess.Outcome

	endedAt := "2026-08-18T12:00:00Z"
	_, err = d.getWriter().Exec(t.Context(), `
		UPDATE sessions
		SET ended_at = ?, is_automated = 1, message_count = 7,
		    peak_context_tokens = 12345, has_peak_context_tokens = 1
		WHERE id = ?`, endedAt, "signal-metadata-race")
	require.NoError(t, err)

	update := SessionSignalUpdate{
		Outcome: "stale-result", OutcomeConfidence: "high",
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
}

func TestReplaceSessionSignalsIfInputsMatchFrictionInputs(t *testing.T) {
	for _, tc := range []struct {
		name        string
		query       string
		wantApplied bool
	}{
		{
			name:  "parent session id",
			query: `UPDATE sessions SET parent_session_id = 'parent' WHERE id = ?`,
		},
		{
			name:  "relationship type",
			query: `UPDATE sessions SET relationship_type = 'fork' WHERE id = ?`,
		},
		{
			name:  "agent",
			query: `UPDATE sessions SET agent = 'codex' WHERE id = ?`,
		},
		{
			name:        "machine",
			query:       `UPDATE sessions SET machine = 'remote' WHERE id = ?`,
			wantApplied: true,
		},
		{
			name:        "project",
			query:       `UPDATE sessions SET project = 'other-project' WHERE id = ?`,
			wantApplied: true,
		},
		{
			name:        "cwd",
			query:       `UPDATE sessions SET cwd = '/workspace/other' WHERE id = ?`,
			wantApplied: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := testDB(t)
			id := "signal-friction-race"
			insertSession(t, d, id, "proj")

			sess, err := d.GetSessionFull(t.Context(), id)
			require.NoError(t, err)
			expected, err := SignalInputSnapshot(*sess)
			require.NoError(t, err)

			baseline := []FrictionFinding{{
				SessionID: id, Kind: "error", Detector: "error",
				Text: "current findings", Title: "current findings",
				Fingerprint: "current-findings",
			}}
			require.NoError(t, d.replaceSessionFriction(t.Context(), id,
				baseline, nil, "friction-v1",
				FrictionHash(baseline, nil, "friction-v1"),
			))

			_, err = d.getWriter().Exec(t.Context(), tc.query, id)
			require.NoError(t, err)
			computed := []FrictionFinding{{
				SessionID: id, Kind: "correction", Detector: "correction.coding",
				Text: "computed finding", Title: "computed finding",
				Fingerprint: "computed-finding",
			}}
			update := SessionSignalUpdate{
				Outcome: "computed-result",
				Friction: &SessionFrictionUpdate{
					Findings: computed, RulesVersion: "friction-v1",
					Hash: FrictionHash(computed, nil, "friction-v1"),
				},
			}
			applied, err := d.ReplaceSessionSignalsIfInputsMatch(
				t.Context(), id, expected, nil, update,
				SessionSignalState{},
			)
			require.NoError(t, err)
			require.Equal(t, tc.wantApplied, applied)

			after, err := d.GetSessionFull(t.Context(), id)
			require.NoError(t, err)
			findings, err := d.SessionFrictionFindings(t.Context(), id)
			require.NoError(t, err)
			require.Len(t, findings, 1)
			if tc.wantApplied {
				require.Equal(t, "computed-result", after.Outcome)
				require.Equal(t, "computed-finding", findings[0].Fingerprint)
				return
			}
			require.NotEqual(t, "computed-result", after.Outcome)
			require.Equal(t, "current-findings", findings[0].Fingerprint)
		})
	}
}
