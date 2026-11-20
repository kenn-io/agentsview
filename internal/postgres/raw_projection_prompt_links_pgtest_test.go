//go:build pgtest

package postgres

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/rawderive"
)

func TestRawProjectionRolloutBackfillsPromptLinks(t *testing.T) {
	for _, tc := range []struct {
		name     string
		agent    parser.AgentType
		id       string
		recorded bool
		preserve bool
	}{
		// Only the Claude parser records links the agent itself reported.
		{name: "rollout", agent: parser.AgentClaude, id: "claude:portable", recorded: true},
		{name: "preserved history", agent: parser.AgentRooCode, id: "roocode:portable", preserve: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newProjectionFixture(t)
			outcome := projectionOutcome("Review https://github.com/o/r/pull/2")
			outcome.Outcome.Results[0].Result.Session.Agent = tc.agent
			outcome.Outcome.Results[0].Result.Session.ID = tc.id
			var recorded []db.PRLink
			if tc.recorded {
				link, ok := parser.NewPRLink("https://github.com/o/r/pull/1", "o/r", 1)
				require.True(t, ok)
				outcome.Outcome.Results[0].Result.Session.PRLinks = []parser.PRLink{link}
				recorded = []db.PRLink{link}
			}
			m, receipt := f.accept(t, "device-a", "initial", "", tc.agent)
			require.NoError(t, f.sink.Project(t.Context(), f.lease(t, m), m, outcome))
			before, err := f.sink.Resolve(t.Context(), tc.id)
			require.NoError(t, err)
			// A pre-upgrade session row contains agent-recorded links only.
			_, err = f.runtime.ExecContext(t.Context(), `UPDATE sessions SET pr_links=$2, data_version=183 WHERE id=$1`, before.SessionID, db.EncodePRLinks(recorded))
			require.NoError(t, err)
			if tc.preserve {
				outcome.Outcome.Results[0].Result.Messages = nil
				m, _ = f.accept(t, "device-a", "replay", receipt.Receipt, tc.agent)
				require.NoError(t, f.sink.Project(t.Context(), f.lease(t, m), m, outcome))
			} else {
				rollout, err := f.sink.ScheduleCurrentHeads(t.Context(), "prompt-links", "parser-2", 10)
				require.NoError(t, err)
				require.Equal(t, 1, rollout.Selected)
				leases, err := f.jobs.ClaimRawParseJobs(t.Context(), "prompt-links-worker", 1, time.Minute)
				require.NoError(t, err)
				require.Len(t, leases, 1)
				require.NoError(t, f.sink.Project(t.Context(), leases[0], m, outcome))
			}
			after, err := f.sink.Resolve(t.Context(), tc.id)
			require.NoError(t, err)
			assert.Equal(t, before.SessionID, after.SessionID)
			var links string
			require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT pr_links FROM sessions WHERE id=$1`, after.SessionID).Scan(&links))
			assert.Equal(t, append(recorded, db.PRLink{URL: "https://github.com/o/r/pull/2", Host: "github.com", Repository: "o/r", Number: 2}), db.DecodePRLinks(links))
		})
	}
}

func TestRawProjectionRolloutBackfillsSelectedLongerCapture(t *testing.T) {
	f := newProjectionFixture(t)
	short := projectionOutcome("start")
	short.Outcome.Results[0].Result.Messages = short.Outcome.Results[0].Result.Messages[:1]
	a, _ := f.accept(t, "device-a", "short", "")
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, a), a, short))
	long := projectionOutcome("start")
	long.Outcome.Results[0].Result.Messages = append(long.Outcome.Results[0].Result.Messages,
		parser.ParsedMessage{Ordinal: 2, Role: parser.RoleUser, Content: "Review https://github.com/o/r/pull/2"})
	b, accepted := f.accept(t, "device-b", "long", "")
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, b), b, long))
	resolved, err := f.sink.Resolve(t.Context(), "codex:portable")
	require.NoError(t, err)
	assert.Equal(t, RawIdentityUnique, resolved.State)
	var links string
	require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT pr_links FROM sessions WHERE id=$1`, resolved.SessionID).Scan(&links))
	assert.Equal(t, []db.PRLink{{URL: "https://github.com/o/r/pull/2", Host: "github.com", Repository: "o/r", Number: 2}}, db.DecodePRLinks(links))
	f.tombstone(t, b, "departed", accepted.Receipt)
	before, err := f.sink.Resolve(t.Context(), "codex:portable")
	require.NoError(t, err)
	_, err = f.runtime.ExecContext(t.Context(), `UPDATE sessions SET pr_links='', data_version=183 WHERE id=$1`, before.SessionID)
	require.NoError(t, err)
	var corpusBefore int64
	require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT corpus_revision FROM raw_corpus_state`).Scan(&corpusBefore))
	_, err = f.sink.SelectSourceGeneration(t.Context(), a, "parser-2")
	require.NoError(t, err)
	leases, err := f.jobs.ClaimRawParseJobs(t.Context(), "upgrade-worker", 1, time.Minute)
	require.NoError(t, err)
	require.Len(t, leases, 1)
	require.NoError(t, f.sink.Project(t.Context(), leases[0], a, short))
	after, err := f.sink.Resolve(t.Context(), "codex:portable")
	require.NoError(t, err)
	assert.Equal(t, before.SessionID, after.SessionID)
	require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT pr_links FROM sessions WHERE id=$1`, after.SessionID).Scan(&links))
	assert.Equal(t, []db.PRLink{{URL: "https://github.com/o/r/pull/2", Host: "github.com", Repository: "o/r", Number: 2}}, db.DecodePRLinks(links))
	var corpusAfter int64
	require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT corpus_revision FROM raw_corpus_state`).Scan(&corpusAfter))
	assert.Greater(t, corpusAfter, corpusBefore)
}

func TestRawProjectionFencesAndBackfillsTombstonedCapture(t *testing.T) {
	f := newProjectionFixture(t)
	m, accepted := f.accept(t, "device-a", "initial", "")
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, m), m, projectionOutcome("Review https://github.com/o/r/pull/2")))
	tomb, _ := f.tombstone(t, m, "departed", accepted.Receipt)
	resolved, err := f.sink.Resolve(t.Context(), "codex:portable")
	require.NoError(t, err)
	_, err = f.runtime.ExecContext(t.Context(), `UPDATE sessions SET data_version=183 WHERE id=$1`, resolved.SessionID)
	require.NoError(t, err)
	for _, stage := range []string{"stale", "backfill"} {
		if stage == "backfill" {
			_, err = f.runtime.ExecContext(t.Context(), `UPDATE sessions SET pr_links='',data_version=183 WHERE id=$1`, resolved.SessionID)
			require.NoError(t, err)
		}
		_, err = f.sink.SelectSourceGeneration(t.Context(), tomb, "parser-"+stage)
		require.NoError(t, err)
		leases, err := f.jobs.ClaimRawParseJobs(t.Context(), "upgrade-worker", 1, time.Minute)
		require.NoError(t, err)
		require.Len(t, leases, 1)
		var corpusBefore int64
		require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT corpus_revision FROM raw_corpus_state`).Scan(&corpusBefore))
		require.NoError(t, f.sink.Project(t.Context(), leases[0], tomb, rawderive.ParsedManifest{Tombstone: true}))
		var corpusAfter int64
		require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT corpus_revision FROM raw_corpus_state`).Scan(&corpusAfter))
		if stage == "backfill" {
			assert.Greater(t, corpusAfter, corpusBefore)
			var links string
			require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT pr_links FROM sessions WHERE id=$1`, resolved.SessionID).Scan(&links))
			assert.Equal(t, []db.PRLink{{URL: "https://github.com/o/r/pull/2", Host: "github.com", Repository: "o/r", Number: 2}}, db.DecodePRLinks(links))
		} else {
			assert.Equal(t, corpusBefore, corpusAfter, stage)
		}
		var version int
		require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT data_version FROM sessions WHERE id=$1`, resolved.SessionID).Scan(&version))
		assert.Equal(t, db.CurrentDataVersion(), version)
	}
}

func TestRawProjectionRefreshesLinksWhenDialogueChanges(t *testing.T) {
	f := newProjectionFixture(t)
	m, _ := f.accept(t, "device-a", "initial", "")
	outcome := projectionOutcome("Review https://github.com/o/r/pull/1")
	result := &outcome.Outcome.Results[0].Result
	result.Messages = append(result.Messages, parser.ParsedMessage{Ordinal: 2, Role: parser.RoleUser, Content: "Also https://github.com/o/r/pull/2"})
	for i, msg := range result.Messages {
		if msg.Role == parser.RoleUser {
			result.Messages[i].DialogueText = new(msg.Content)
		}
	}
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, m), m, outcome))
	resolved, err := f.sink.Resolve(t.Context(), "codex:portable")
	require.NoError(t, err)
	var links string
	require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT pr_links FROM sessions WHERE id=$1`, resolved.SessionID).Scan(&links))
	require.Equal(t, []db.PRLink{
		{URL: "https://github.com/o/r/pull/1", Host: "github.com", Repository: "o/r", Number: 1},
		{URL: "https://github.com/o/r/pull/2", Host: "github.com", Repository: "o/r", Number: 2},
	}, db.DecodePRLinks(links))
	// A newer parser keeps the message but no longer counts it as dialogue.
	result.Messages[2].DialogueText = new("")
	_, err = f.sink.SelectSourceGeneration(t.Context(), m, "parser-2")
	require.NoError(t, err)
	leases, err := f.jobs.ClaimRawParseJobs(t.Context(), "upgrade-worker", 1, time.Minute)
	require.NoError(t, err)
	require.Len(t, leases, 1)
	require.NoError(t, f.sink.Project(t.Context(), leases[0], m, outcome))
	require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT pr_links FROM sessions WHERE id=$1`, resolved.SessionID).Scan(&links))
	assert.Equal(t, []db.PRLink{{URL: "https://github.com/o/r/pull/1", Host: "github.com", Repository: "o/r", Number: 1}}, db.DecodePRLinks(links))
}
