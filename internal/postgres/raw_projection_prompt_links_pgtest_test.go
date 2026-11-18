//go:build pgtest

package postgres

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
)

func TestRawProjectionPreservedHistoryFindsPromptLinks(t *testing.T) {
	f := newProjectionFixture(t)
	outcome := projectionOutcome("https://github.com/o/r/pull/2")
	outcome.Outcome.Results[0].Result.Session.Agent = parser.AgentRooCode
	structured, ok := parser.NewPRLink("https://github.com/o/r/pull/1", "o/r", 1)
	require.True(t, ok)
	outcome.Outcome.Results[0].Result.Session.PRLinks = []parser.PRLink{structured}
	a, receipt := f.accept(t, "device-a", "initial", "", parser.AgentRooCode)
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, a), a, outcome))
	outcome.Outcome.Results[0].Result.Messages = nil
	a, _ = f.accept(t, "device-a", "replay", receipt.Receipt, parser.AgentRooCode)
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, a), a, outcome))
	resolved, err := f.sink.Resolve(t.Context(), "codex:portable")
	require.NoError(t, err)
	var links string
	require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT pr_links FROM sessions WHERE id=$1`, resolved.SessionID).Scan(&links))
	assert.Equal(t, []db.PRLink{structured, {URL: "https://github.com/o/r/pull/2", Host: "github.com", Repository: "o/r", Number: 2}}, db.DecodePRLinks(links))
}

func TestRawProjectionPromptLinkInAppendedDeviceCapture(t *testing.T) {
	f := newProjectionFixture(t)
	short := projectionOutcome("start")
	a, _ := f.accept(t, "device-a", "short", "")
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, a), a, short))
	long := projectionOutcome("start")
	long.Outcome.Results[0].Result.Messages = append(long.Outcome.Results[0].Result.Messages,
		parser.ParsedMessage{Ordinal: 2, Role: parser.RoleUser, Content: "https://github.com/o/r/pull/2"})
	b, _ := f.accept(t, "device-b", "long", "")
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, b), b, long))
	resolved, err := f.sink.Resolve(t.Context(), "codex:portable")
	require.NoError(t, err)
	assert.Equal(t, RawIdentityUnique, resolved.State)
	var links string
	require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT pr_links FROM sessions WHERE id=$1`, resolved.SessionID).Scan(&links))
	assert.Equal(t, []db.PRLink{{URL: "https://github.com/o/r/pull/2", Host: "github.com", Repository: "o/r", Number: 2}}, db.DecodePRLinks(links))
}

func TestRawProjectionRolloutBackfillsPromptLinks(t *testing.T) {
	f := newProjectionFixture(t)
	outcome := projectionOutcome("Review https://github.com/o/r/pull/2")
	link, ok := parser.NewPRLink("https://github.com/o/r/pull/1", "o/r", 1)
	require.True(t, ok)
	outcome.Outcome.Results[0].Result.Session.PRLinks = []parser.PRLink{link}
	m, _ := f.accept(t, "device-a", "initial", "")
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, m), m, outcome))
	before, err := f.sink.Resolve(t.Context(), "codex:portable")
	require.NoError(t, err)
	// A pre-upgrade session row contains the agent-recorded link only.
	_, err = f.runtime.ExecContext(t.Context(), `UPDATE sessions SET pr_links=$2 WHERE id=$1`, before.SessionID, db.EncodePRLinks([]db.PRLink{link}))
	require.NoError(t, err)
	rollout, err := f.sink.ScheduleCurrentHeads(t.Context(), "prompt-links", "parser-2", 10)
	require.NoError(t, err)
	require.Equal(t, 1, rollout.Selected)
	leases, err := f.jobs.ClaimRawParseJobs(t.Context(), "prompt-links-worker", 1, time.Minute)
	require.NoError(t, err)
	require.Len(t, leases, 1)
	require.NoError(t, f.sink.Project(t.Context(), leases[0], m, outcome))
	after, err := f.sink.Resolve(t.Context(), "codex:portable")
	require.NoError(t, err)
	assert.Equal(t, before.SessionID, after.SessionID)
	var links string
	require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT pr_links FROM sessions WHERE id=$1`, after.SessionID).Scan(&links))
	assert.Equal(t, []db.PRLink{link, {URL: "https://github.com/o/r/pull/2", Host: "github.com", Repository: "o/r", Number: 2}}, db.DecodePRLinks(links))
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
	f.tombstone(t, b, "departed", accepted.Receipt)
	before, err := f.sink.Resolve(t.Context(), "codex:portable")
	require.NoError(t, err)
	_, err = f.runtime.ExecContext(t.Context(), `UPDATE sessions SET pr_links='[]' WHERE id=$1`, before.SessionID)
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
	pr, err := db.ParsePRFilter("o/r#2")
	require.NoError(t, err)
	page, err := f.hosted(t).ListSessions(t.Context(), db.SessionFilter{PR: pr, Limit: 10})
	require.NoError(t, err)
	require.Len(t, page.Sessions, 1)
	assert.Equal(t, "codex:portable", page.Sessions[0].ID)
	var corpusAfter int64
	require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT corpus_revision FROM raw_corpus_state`).Scan(&corpusAfter))
	assert.Greater(t, corpusAfter, corpusBefore)
}
