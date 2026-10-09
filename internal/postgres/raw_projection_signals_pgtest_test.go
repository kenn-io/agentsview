//go:build pgtest

package postgres

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/ingest"
	"go.kenn.io/agentsview/internal/parser"
)

func TestRawProjectionTranscriptsOnlyToolCoverage(t *testing.T) {
	f := newHostedFixture(t, "tenant-tool-coverage")
	options := RawProjectionOptions{Tenant: f.tenant, Content: ingest.ContentOptions{ArchiveContent: config.ArchiveContentTranscripts}}
	sink, err := NewRawProjectionStore(f.runtime, options)
	require.NoError(t, err)
	outcome := projectionOutcome("Check the fixture")
	outcome.Outcome.Results[0].Result.Messages = append(outcome.Outcome.Results[0].Result.Messages, parser.ParsedMessage{
		Ordinal: 2, Role: parser.RoleAssistant, ToolCalls: []parser.ParsedToolCall{{ToolUseID: "call-2", ToolName: "Bash", Category: "Bash", InputJSON: `{"command":"true"}`}},
	})
	candidate, err := ingest.PrepareCandidate(t.Context(), outcome.Outcome.Results[0].Result, options.Content)
	require.NoError(t, err)
	prepared, err := ingest.Finalize(t.Context(), candidate, options.Content)
	require.NoError(t, err)
	require.NotNil(t, prepared.Signals.ToolObservations)
	assert.Empty(t, prepared.Signals.ToolObservations)
	revision, err := rawContentRevision(prepared)
	require.NoError(t, err)
	tx, err := f.runtime.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	defer tx.Rollback()
	require.NoError(t, sink.writePayload(t.Context(), tx, "fixture-session", "fixture-group", revision, prepared))
	prepared.Signals.ToolObservations = []db.ToolObservation{{MessageOrdinal: 1, Outcome: "empty", Repeat: "none"}}
	_, err = tx.ExecContext(t.Context(), `INSERT INTO raw_session_groups(group_id,provider,logical_key,base_alias) VALUES('fixture-group','codex','fixture','fixture')`)
	require.NoError(t, err)
	_, err = tx.ExecContext(t.Context(), `INSERT INTO raw_content_revisions(session_id,group_id,content_revision,payload) VALUES('fixture-session','fixture-group',$1,$2)`, revision, []byte{})
	require.NoError(t, err)
	changed, err := publishRawRecency(t.Context(), tx, "fixture-session", prepared.Signals)
	require.NoError(t, err)
	assert.True(t, changed)
	prepared.Signals.ToolObservations = nil
	changed, err = publishRawRecency(t.Context(), tx, "fixture-session", prepared.Signals)
	require.NoError(t, err)
	assert.False(t, changed, "nil observations preserve the snapshot")
	prepared.Signals.ToolObservations = []db.ToolObservation{}
	changed, err = publishRawRecency(t.Context(), tx, "fixture-session", prepared.Signals)
	require.NoError(t, err)
	assert.True(t, changed, "explicit empty observations clear stored facts")
	changed, err = publishRawRecency(t.Context(), tx, "fixture-session", prepared.Signals)
	require.NoError(t, err)
	assert.False(t, changed)
	require.NoError(t, tx.Commit())
}

func TestRawProjectionEqualContentSettlesSignalsWithoutNewIdentity(t *testing.T) {
	for _, agent := range []parser.AgentType{parser.AgentCodex, parser.AgentRooCode} {
		t.Run(string(agent), func(t *testing.T) {
			f := newProjectionFixture(t)
			ended := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
			now := ended.Add(time.Minute)
			f.sink.options.Now = func() time.Time { return now }
			outcome := projectionOutcome("Implement the requested change")
			outcome.Outcome.Results[0].Result.Session.EndedAt = ended
			outcome.Outcome.Results[0].Result.Session.Agent = agent
			outcome.Outcome.Results[0].Result.Messages[1].ToolCalls[0].ToolName = "Grep"
			outcome.Outcome.Results[0].Result.Messages[1].ToolCalls[0].Category = "Grep"
			outcome.Outcome.Results[0].Result.Messages[1].ToolCalls[0].ResultEvents = []parser.ParsedToolResultEvent{{Status: "completed", Content: "No matches found"}}
			outcome.Outcome.Results[0].Result.Messages = append(outcome.Outcome.Results[0].Result.Messages, parser.ParsedMessage{Ordinal: 2, Role: parser.RoleUser, Content: "Continue implementing this change"})
			a, ar := f.accept(t, "device-a", "recent-a", "", agent)
			require.NoError(t, f.sink.Project(t.Context(), f.lease(t, a), a, outcome))
			first, err := f.sink.Resolve(t.Context(), "codex:portable")
			require.NoError(t, err)
			var pending *string
			var outcomeName string
			require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT outcome,signals_pending_since::text FROM sessions WHERE id=$1`, first.SessionID).Scan(&outcomeName, &pending))
			assert.Equal(t, "unknown", outcomeName)
			require.NotNil(t, pending)
			firstPending := *pending
			var payload []byte
			require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT payload FROM raw_content_revisions WHERE session_id=$1`, first.SessionID).Scan(&payload))
			legacy, err := decodeRawPayload(payload)
			require.NoError(t, err)
			legacy.Signals.ToolObservations = nil
			db.ApplyToolObservations(legacy.Messages, []db.ToolObservation{})
			payload, err = encodeRawPayload(legacy)
			require.NoError(t, err)
			_, err = f.runtime.ExecContext(t.Context(), `UPDATE raw_content_revisions SET payload=$2,recency_state=recency_state-'ToolObservations' WHERE session_id=$1`, first.SessionID, payload)
			require.NoError(t, err)
			_, err = f.runtime.ExecContext(t.Context(), `UPDATE tool_calls SET observed_outcome=NULL,observed_repeat=NULL,sequence_ending=NULL WHERE session_id=$1`, first.SessionID)
			require.NoError(t, err)
			replay := outcome
			if agent == parser.AgentRooCode {
				replay.Outcome.Results = append([]parser.ParseResultOutcome(nil), outcome.Outcome.Results...)
				replay.Outcome.Results[0].Result.Messages = nil
			}
			a, ar = f.accept(t, "device-a", "legacy-replay", ar.Receipt, agent)
			require.NoError(t, f.sink.Project(t.Context(), f.lease(t, a), a, replay))
			repaired, err := f.sink.Resolve(t.Context(), "codex:portable")
			require.NoError(t, err)
			assert.Equal(t, first.SessionID, repaired.SessionID)
			assert.Equal(t, first.IdentityRevision, repaired.IdentityRevision)
			assert.Equal(t, first.CorpusRevision+1, repaired.CorpusRevision)
			assertCoverage := func() {
				t.Helper()
				var outcome, repeat string
				var ending *string
				require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT observed_outcome,observed_repeat,sequence_ending FROM tool_calls WHERE session_id=$1`, first.SessionID).Scan(&outcome, &repeat, &ending))
				assert.Equal(t, "empty", outcome)
				assert.Equal(t, "none", repeat)
				assert.NotNil(t, ending)
				var currentPayload []byte
				require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT payload FROM raw_content_revisions WHERE session_id=$1`, first.SessionID).Scan(&currentPayload))
				assert.Equal(t, payload, currentPayload)
			}
			assertCoverage()
			first = repaired
			now = now.Add(time.Minute)
			a, ar = f.accept(t, "device-a", "recent-a-again", ar.Receipt, agent)
			require.NoError(t, f.sink.Project(t.Context(), f.lease(t, a), a, outcome))
			recent, err := f.sink.Resolve(t.Context(), "codex:portable")
			require.NoError(t, err)
			assert.Equal(t, first.SessionID, recent.SessionID)
			assert.Equal(t, first.CorpusRevision, recent.CorpusRevision, "recent no-op receipt")
			require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT signals_pending_since::text FROM sessions WHERE id=$1`, first.SessionID).Scan(&pending))
			require.NotNil(t, pending)
			assert.Equal(t, firstPending, *pending)
			b, br := f.accept(t, "device-b", "equal-recent-b", "", agent)
			require.NoError(t, f.sink.Project(t.Context(), f.lease(t, b), b, outcome))
			shared, err := f.sink.Resolve(t.Context(), "codex:portable")
			require.NoError(t, err)
			assert.Equal(t, RawIdentityUnique, shared.State)
			var outbox int
			require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT count(*) FROM raw_embedding_outbox`).Scan(&outbox))
			now = ended.Add(11 * time.Minute)
			changed, err := f.sink.SettlePendingSignals(t.Context(), 10)
			require.NoError(t, err)
			assert.Equal(t, 1, changed)
			assertCoverage()
			a, ar = f.accept(t, "device-a", "settled-a", ar.Receipt, agent)
			require.NoError(t, f.sink.Project(t.Context(), f.lease(t, a), a, outcome))
			settled, err := f.sink.Resolve(t.Context(), "codex:portable")
			require.NoError(t, err)
			assert.Equal(t, shared.SessionID, settled.SessionID)
			assert.Equal(t, shared.IdentityRevision, settled.IdentityRevision)
			assert.Equal(t, shared.CorpusRevision+1, settled.CorpusRevision)
			var score *int
			require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT outcome,signals_pending_since::text,health_score FROM sessions WHERE id=$1`, settled.SessionID).Scan(&outcomeName, &pending, &score))
			assert.Equal(t, "abandoned", outcomeName)
			assert.Nil(t, pending)
			require.NotNil(t, score)
			assert.Less(t, *score, 100)
			var afterOutbox int
			require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT count(*) FROM raw_embedding_outbox`).Scan(&afterOutbox))
			assert.Equal(t, outbox, afterOutbox)
			// Remove all source proof, then recreate from the durable shared revision.
			empty := outcome
			empty.Outcome.Results = nil
			a, ar = f.accept(t, "device-a", "gone-a", ar.Receipt, agent)
			require.NoError(t, f.sink.Project(t.Context(), f.lease(t, a), a, empty))
			b, _ = f.accept(t, "device-b", "gone-b", br.Receipt, agent)
			require.NoError(t, f.sink.Project(t.Context(), f.lease(t, b), b, empty))
			a, ar = f.accept(t, "device-a", "return-a", ar.Receipt, agent)
			require.NoError(t, f.sink.Project(t.Context(), f.lease(t, a), a, outcome))
			require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT outcome,signals_pending_since::text FROM sessions WHERE id=$1`, settled.SessionID).Scan(&outcomeName, &pending))
			assert.Equal(t, "abandoned", outcomeName)
			assert.Nil(t, pending)
			assertCoverage()
			restored, err := f.sink.Resolve(t.Context(), "codex:portable")
			require.NoError(t, err)
			a, _ = f.accept(t, "device-a", "return-again", ar.Receipt, agent)
			require.NoError(t, f.sink.Project(t.Context(), f.lease(t, a), a, outcome))
			identical, err := f.sink.Resolve(t.Context(), "codex:portable")
			require.NoError(t, err)
			assert.Equal(t, restored.CorpusRevision, identical.CorpusRevision)
		})
	}
}
