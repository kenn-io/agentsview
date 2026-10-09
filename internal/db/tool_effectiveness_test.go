package db

import (
	"database/sql"
	"encoding/json/v2"
	"errors"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
)

func seedToolRateFixture(t *testing.T, d *DB) {
	t.Helper()
	data, err := os.ReadFile("testdata/tool_effectiveness.json")
	require.NoError(t, err)
	var cases []SessionBatchWrite
	require.NoError(t, json.Unmarshal(data, &cases))
	var messages []Message
	for _, fixture := range cases {
		id := fixture.Session.ID
		insertSession(t, d, id, fixture.Session.Project, func(s *Session) {
			s.StartedAt = fixture.Session.StartedAt
			s.UserMessageCount = fixture.Session.UserMessageCount
		})
		for _, call := range fixture.Messages {
			m := asstMsgAt(id, call.Ordinal, "tool", call.Timestamp)
			m.Model = call.Model
			m.ToolCalls = []ToolCall{{SessionID: id, ToolName: call.ToolCalls[0].ToolName, Category: call.ToolCalls[0].Category}}
			messages = append(messages, m)
		}
	}
	insertMessages(t, d, messages...)
	facts := map[string][]ToolObservation{
		"recovered": {{MessageOrdinal: 2, Outcome: "empty", Repeat: "none", SequenceEnding: new("recovered")}, {MessageOrdinal: 4, Outcome: "empty", Repeat: "identical"}, {MessageOrdinal: 5, Outcome: "content", Repeat: "none"}},
		"abandoned": {{Outcome: "empty", Repeat: "none", SequenceEnding: new("abandoned")}},
		"open":      {{Outcome: "empty", Repeat: "none", SequenceEnding: new("open")}},
		"unknown":   {{Outcome: "empty", Repeat: "none", SequenceEnding: new("unknown")}, {MessageOrdinal: 1, Outcome: "unknown", Repeat: "near_identical"}},
	}
	for id, observations := range facts {
		updateSignals(t, d, id, SessionSignalUpdate{ToolObservations: observations, QualitySignals: QualitySignals{Version: CurrentQualitySignalVersion}})
	}
}

func TestToolEffectivenessFilteredRatesAndEvidence(t *testing.T) {
	d := testDB(t)
	seedToolRateFixture(t, d)
	for _, tc := range []struct {
		name                                                                         string
		filter                                                                       AnalyticsFilter
		calls, analyzed, known, empty, repeated, recovered, abandoned, open, unknown int
	}{
		{"all", AnalyticsFilter{From: "2025-06-01", To: "2025-06-02", Timezone: "UTC"}, 7, 6, 5, 5, 2, 1, 1, 1, 1},
		{"second_day", AnalyticsFilter{From: "2025-06-02", To: "2025-06-02", Timezone: "UTC"}, 6, 5, 4, 4, 2, 0, 1, 1, 1},
		{"first_model", AnalyticsFilter{From: "2025-06-01", To: "2025-06-02", Timezone: "UTC", Model: "model-a"}, 1, 1, 1, 1, 0, 1, 0, 0, 0},
		{"second_model", AnalyticsFilter{From: "2025-06-01", To: "2025-06-02", Timezone: "UTC", Model: "model-b"}, 6, 5, 4, 4, 2, 0, 1, 1, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			response, err := d.GetAnalyticsTools(t.Context(), tc.filter)
			require.NoError(t, err)
			var tool ToolUsageAnalysis
			for _, row := range response.ByTool {
				if row.ToolName == "Grep" {
					tool = row
				}
			}
			assert.Equal(t, tc.calls, tool.CallCount)
			assert.Equal(t, tc.calls-tc.analyzed, tool.MissingCalls)
			assert.Equal(t, ToolEffectivenessCounts{AnalyzedCalls: tc.analyzed, KnownOutcomeCalls: tc.known, EmptyCalls: tc.empty, RepeatedCalls: tc.repeated, RecoveredSequences: tc.recovered, AbandonedSequences: tc.abandoned, OpenSequences: tc.open, UnknownSequences: tc.unknown}, tool.ToolEffectivenessCounts)
			require.NotNil(t, tool.EmptyRate)
			assert.InDelta(t, float64(tc.empty)/float64(tc.known), *tool.EmptyRate, 0)
			require.NotNil(t, tool.RepeatRate)
			assert.InDelta(t, float64(tc.repeated)/float64(tc.analyzed), *tool.RepeatRate, 0)
			require.NotNil(t, tool.RecoveryRate)
			assert.InDelta(t, float64(tc.recovered)/float64(tc.recovered+tc.abandoned), *tool.RecoveryRate, 0)
		})
	}
	f := AnalyticsFilter{From: "2025-06-01", To: "2025-06-02", Timezone: "UTC", ToolName: "Grep", ToolCategory: "Grep"}
	first, err := d.GetAnalyticsSignalSessions(t.Context(), f, "tool_empty_rate", 1)
	require.NoError(t, err)
	assert.Equal(t, new(4), first.Total)
	require.Len(t, first.Sessions, 1)
	require.NotNil(t, first.NextOffset)
	f.EvidenceOffset = *first.NextOffset
	second, err := d.GetAnalyticsSignalSessions(t.Context(), f, "tool_empty_rate", 1)
	require.NoError(t, err)
	require.Len(t, second.Sessions, 1)
	assert.NotEqual(t, first.Sessions[0].SessionID, second.Sessions[0].SessionID)
	require.NotNil(t, second.NextOffset)
	f.EvidenceOffset = 0
	f.Model = "model-b"
	evidence, err := d.GetAnalyticsSignalSessions(t.Context(), f, "tool_empty_rate", 20)
	require.NoError(t, err)
	require.Len(t, evidence.Sessions, 4)
	f.Model = ""
	evidence, err = d.GetAnalyticsSignalSessions(t.Context(), f, "tool_recovery_rate", 20)
	require.NoError(t, err)
	require.Len(t, evidence.Sessions, 1)
	assert.Equal(t, "recovered", evidence.Sessions[0].SessionID)
	assert.Equal(t, 1, evidence.Sessions[0].SignalTotal)
	f.ToolName = "Read"
	f.ToolCategory = "Read"
	evidence, err = d.GetAnalyticsSignalSessions(t.Context(), f, "tool_recovery_rate", 20)
	require.NoError(t, err)
	assert.Empty(t, evidence.Sessions)
}

func TestToolEffectivenessMissingAndUnknownDenominators(t *testing.T) {
	d := testDB(t)
	insertSession(t, d, "unknown", "project-a")
	m := asstMsgAt("unknown", 0, "tool", "2025-06-02T00:00:00Z")
	m.ToolCalls = []ToolCall{{SessionID: "unknown", ToolName: "Grep", Category: "Grep"}}
	insertMessages(t, d, m)
	f := AnalyticsFilter{From: "2025-06-02", To: "2025-06-02", Timezone: "UTC"}
	response, err := d.GetAnalyticsTools(t.Context(), f)
	require.NoError(t, err)
	require.Len(t, response.ByTool, 1)
	row := response.ByTool[0]
	assert.Equal(t, 1, row.MissingCalls)
	assert.Nil(t, row.EmptyRate)
	assert.Nil(t, row.RepeatRate)
	assert.Nil(t, row.RecoveryRate)
	updateSignals(t, d, "unknown", SessionSignalUpdate{ToolObservations: []ToolObservation{{Outcome: "unknown", Repeat: "none"}}, QualitySignals: QualitySignals{Version: CurrentQualitySignalVersion}})
	response, err = d.GetAnalyticsTools(t.Context(), f)
	require.NoError(t, err)
	row = response.ByTool[0]
	assert.Zero(t, row.MissingCalls)
	assert.Nil(t, row.EmptyRate)
	require.NotNil(t, row.RepeatRate)
	assert.Zero(t, *row.RepeatRate)
	assert.Nil(t, row.RecoveryRate)
	require.NoError(t, d.Update(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(), `UPDATE sessions SET quality_signal_version=0 WHERE id='unknown'`)
		return err
	}))
	response, err = d.GetAnalyticsTools(t.Context(), f)
	require.NoError(t, err)
	assert.Equal(t, 1, response.ByTool[0].MissingCalls)
	evidence, err := d.GetAnalyticsSignalSessions(t.Context(), f, "tool_repeat_rate", 10)
	require.NoError(t, err)
	assert.Empty(t, evidence.Sessions)
}

func TestToolObservationsReplacementFingerprintAndOrphanCopy(t *testing.T) {
	d := testDB(t)
	seedToolRateFixture(t, d)
	before, err := d.ToolCallFingerprint(t.Context(), "recovered")
	require.NoError(t, err)
	parseBefore, err := d.ToolCallParseDiffFingerprint(t.Context(), "recovered")
	require.NoError(t, err)
	signalsWith := func(observations []ToolObservation) SessionSignalUpdate {
		return SessionSignalUpdate{ToolObservations: observations, QualitySignals: QualitySignals{Version: CurrentQualitySignalVersion}}
	}
	_, err = d.getWriter().Exec(t.Context(), `CREATE TRIGGER reject_unchanged_observations BEFORE UPDATE OF observed_outcome ON tool_calls WHEN NEW.session_id='recovered' BEGIN SELECT RAISE(ABORT, 'unexpected observation write'); END`)
	require.NoError(t, err)
	identical := []ToolObservation{{MessageOrdinal: 5, Outcome: "content", Repeat: "none"}, {MessageOrdinal: 4, Outcome: "empty", Repeat: "identical"}, {MessageOrdinal: 2, Outcome: "empty", Repeat: "none", SequenceEnding: new("recovered")}}
	// A recompute that leaves every call's facts unchanged must not rewrite tool-call rows.
	for _, observations := range [][]ToolObservation{nil, identical, append(identical, ToolObservation{MessageOrdinal: 99, Outcome: "unknown", Repeat: "none"})} {
		require.NoError(t, d.UpdateSessionSignals(t.Context(), "recovered", signalsWith(observations)))
	}
	changed := identical[:2]
	require.ErrorContains(t, d.UpdateSessionSignals(t.Context(), "recovered", signalsWith(changed)), "unexpected observation write")
	unchanged, err := d.ToolCallFingerprint(t.Context(), "recovered")
	require.NoError(t, err)
	assert.Equal(t, before, unchanged)
	_, err = d.getWriter().Exec(t.Context(), "DROP TRIGGER reject_unchanged_observations")
	require.NoError(t, err)
	require.NoError(t, d.UpdateSessionSignals(t.Context(), "recovered", signalsWith(changed)))
	messages, err := d.GetAllMessages(t.Context(), "recovered")
	require.NoError(t, err)
	assert.Nil(t, messages[0].ToolCalls[0].ObservedOutcome)
	assert.Equal(t, new("identical"), messages[1].ToolCalls[0].ObservedRepeat)
	ending := append([]ToolObservation(nil), identical...)
	ending[0].SequenceEnding = new("")
	require.NoError(t, d.UpdateSessionSignals(t.Context(), "recovered", signalsWith(ending)))
	messages, err = d.GetAllMessages(t.Context(), "recovered")
	require.NoError(t, err)
	assert.Equal(t, new(""), messages[2].ToolCalls[0].SequenceEnding)
	require.NoError(t, d.UpdateSessionSignals(t.Context(), "recovered", signalsWith(identical)))
	messages, err = d.GetAllMessages(t.Context(), "recovered")
	require.NoError(t, err)
	assert.Nil(t, messages[2].ToolCalls[0].SequenceEnding)
	after, err := d.ToolCallFingerprint(t.Context(), "recovered")
	require.NoError(t, err)
	assert.Equal(t, before, after)
	replacement := testDB(t)
	_, err = replacement.CopyOrphanedDataFromExcluding(d.Path(), nil)
	require.NoError(t, err)
	copied, err := replacement.ToolCallFingerprint(t.Context(), "recovered")
	require.NoError(t, err)
	assert.Equal(t, before, copied)
	require.NoError(t, d.UpdateSessionSignals(t.Context(), "recovered", signalsWith([]ToolObservation{})))
	after, err = d.ToolCallFingerprint(t.Context(), "recovered")
	require.NoError(t, err)
	assert.NotEqual(t, before, after)
	batch, err := d.ToolCallFingerprints(t.Context(), []string{"recovered"})
	require.NoError(t, err)
	assert.Equal(t, after, batch["recovered"])
	parseAfter, err := d.ToolCallParseDiffFingerprint(t.Context(), "recovered")
	require.NoError(t, err)
	assert.Equal(t, parseBefore, parseAfter)
}

func TestToolObservationsFullReplaceWritesWithCalls(t *testing.T) {
	for _, route := range []string{"single", "batch", "batch-transcripts", "observations-transcripts"} {
		t.Run(route, func(t *testing.T) {
			d := testDB(t)
			insertSession(t, d, "s1", "project-a")
			require.NoError(t, d.Update(t.Context(), func(tx *sql.Tx) error {
				_, err := tx.ExecContext(t.Context(), `CREATE TRIGGER reject_observation_update BEFORE UPDATE OF observed_outcome ON tool_calls BEGIN SELECT RAISE(ABORT, 'observations must accompany inserts'); END`)
				return err
			}))
			messages := []Message{{SessionID: "s1", Ordinal: 3, Role: "assistant", ToolCalls: []ToolCall{{ToolName: "Grep", Category: "Grep", ObservedOutcome: new("content")}}}}
			update := SessionSignalUpdate{ToolObservations: []ToolObservation{{MessageOrdinal: 3, Outcome: "empty", Repeat: "none", SequenceEnding: new("abandoned")}}}
			switch route {
			case "observations-transcripts":
				_, err := d.getWriter().Exec(t.Context(), "DROP TRIGGER reject_observation_update")
				require.NoError(t, err)
				require.NoError(t, d.ReplaceSessionContent(t.Context(), "s1", messages, update, nil))
				d.SetArchiveContent(config.ArchiveContentTranscripts)
				require.NoError(t, d.UpdateSessionSignals(t.Context(), "s1", SessionSignalUpdate{}))
			case "single":
				require.NoError(t, d.ReplaceSessionContent(t.Context(), "s1", messages, update, nil))
			default:
				if route == "batch-transcripts" {
					d.SetArchiveContent(config.ArchiveContentTranscripts)
				}
				session, err := d.GetSessionFull(t.Context(), "s1")
				require.NoError(t, err)
				result, err := d.WriteSessionBatchContext(t.Context(), []SessionBatchWrite{{Session: *session, Messages: messages, Signals: update, ReplaceMessages: true}})
				require.NoError(t, err)
				require.Empty(t, result.Errors)
				require.Equal(t, 1, result.WrittenSessions)
			}
			stored, err := d.GetAllMessages(t.Context(), "s1")
			require.NoError(t, err)
			require.Len(t, stored, 1)
			if route == "batch-transcripts" || route == "observations-transcripts" {
				assert.Nil(t, stored[0].ToolCalls[0].ObservedOutcome)
			} else {
				assert.Equal(t, new("empty"), stored[0].ToolCalls[0].ObservedOutcome)
				assert.Equal(t, new("abandoned"), stored[0].ToolCalls[0].SequenceEnding)
			}
		})
	}
}

func TestToolObservationDeltaPointUpdatesAndRollback(t *testing.T) {
	d := testDB(t)
	seedToolRateFixture(t, d)
	before, err := d.GetAllMessages(t.Context(), "recovered")
	require.NoError(t, err)
	delta := SignalDelta{Update: SessionSignalUpdate{QualitySignals: QualitySignals{Version: CurrentQualitySignalVersion}}, ToolObservations: []ToolObservationDelta{{MessageOrdinal: 2, SequenceEnding: new("open")}, {MessageOrdinal: 4, Outcome: new("content"), Repeat: new("none")}}}
	require.Error(t, d.Update(t.Context(), func(tx *sql.Tx) error {
		require.NoError(t, applySignalDeltaTx(t.Context(), tx, "recovered", delta))
		return errors.New("rollback fixture")
	}))
	after, err := d.GetAllMessages(t.Context(), "recovered")
	require.NoError(t, err)
	assert.Equal(t, before, after)
	require.NoError(t, d.Update(t.Context(), func(tx *sql.Tx) error { return applySignalDeltaTx(t.Context(), tx, "recovered", delta) }))
	after, err = d.GetAllMessages(t.Context(), "recovered")
	require.NoError(t, err)
	assert.Equal(t, "empty", *after[0].ToolCalls[0].ObservedOutcome)
	assert.Equal(t, "open", *after[0].ToolCalls[0].SequenceEnding)
	assert.Equal(t, "content", *after[1].ToolCalls[0].ObservedOutcome)
	assert.Equal(t, before[2], after[2])
}
