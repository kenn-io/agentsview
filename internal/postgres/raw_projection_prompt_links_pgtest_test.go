//go:build pgtest

package postgres

import (
	"os"
	"path/filepath"
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
				outcome.Outcome.Results[0].Result.Session.PRLinks = []parser.PRLink{link, {URL: "http://github.com/o/r/pull/1", Host: "github.com", Repository: "o/r", Number: 1}}
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
	root := t.TempDir()
	project := filepath.Join(root, "-home-user-project")
	require.NoError(t, os.MkdirAll(project, 0o700))
	path := filepath.Join(project, "portable.jsonl")
	input := `{"type":"user","sessionId":"portable","uuid":"u1","timestamp":"2026-01-01T00:00:00Z","message":{"role":"user","content":[{"type":"text","text":"Review https://github.com/o/r/pull/1"},{"type":"thinking","thinking":"https://github.com/o/r/pull/2"}]}}` + "\n"
	require.NoError(t, os.WriteFile(path, []byte(input), 0o600))
	provider, ok := parser.NewProvider(parser.AgentClaude, parser.ProviderConfig{Roots: []string{root}})
	require.True(t, ok)
	sources, err := provider.Discover(t.Context())
	require.NoError(t, err)
	require.Len(t, sources, 1)
	parsed, err := provider.Parse(t.Context(), parser.ParseRequest{Source: sources[0]})
	require.NoError(t, err)
	require.Len(t, parsed.Results, 1)
	result := &parsed.Results[0].Result
	require.Len(t, result.Messages, 1)
	require.Contains(t, result.Messages[0].Content, "pull/2")
	require.NotContains(t, *result.Messages[0].DialogueText, "pull/2")
	result.Session.ID = "claude:portable"
	outcome := rawderive.ParsedManifest{Outcome: parsed}
	f := newProjectionFixture(t)
	m, _ := f.accept(t, "device-a", "initial", "", parser.AgentClaude)
	dialogue := result.Messages[0].DialogueText
	result.Messages[0].DialogueText = nil
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, m), m, outcome))
	resolved, err := f.sink.Resolve(t.Context(), "claude:portable")
	require.NoError(t, err)
	var links string
	require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT pr_links FROM sessions WHERE id=$1`, resolved.SessionID).Scan(&links))
	require.Len(t, db.DecodePRLinks(links), 2)
	result.Messages[0].DialogueText = dialogue
	_, err = f.sink.SelectSourceGeneration(t.Context(), m, "parser-2")
	require.NoError(t, err)
	leases, err := f.jobs.ClaimRawParseJobs(t.Context(), "upgrade-worker", 1, time.Minute)
	require.NoError(t, err)
	require.Len(t, leases, 1)
	require.NoError(t, f.sink.Project(t.Context(), leases[0], m, outcome))
	require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT pr_links FROM sessions WHERE id=$1`, resolved.SessionID).Scan(&links))
	assert.Equal(t, []db.PRLink{{URL: "https://github.com/o/r/pull/1", Host: "github.com", Repository: "o/r", Number: 1}}, db.DecodePRLinks(links))
}

func TestRawProjectionClaudeRecordedLinksKeepSharedSession(t *testing.T) {
	root := t.TempDir()
	project := filepath.Join(root, "-home-user-project")
	require.NoError(t, os.MkdirAll(project, 0o700))
	path := filepath.Join(project, "portable.jsonl")
	transcript := `{"type":"user","sessionId":"portable","uuid":"u1","timestamp":"2026-01-01T00:00:00Z","message":{"role":"user","content":"Review this change"}}
{"type":"pr-link","prUrl":"https://github.com/o/r/pull/7","prRepository":"o/r","prNumber":7}
{"type":"pr-link","prUrl":"http://github.com/o/r/pull/7","prRepository":"o/r","prNumber":7}
`
	require.NoError(t, os.WriteFile(path, []byte(transcript), 0o600))
	provider, ok := parser.NewProvider(parser.AgentClaude, parser.ProviderConfig{Roots: []string{root}})
	require.True(t, ok)
	sources, err := provider.Discover(t.Context())
	require.NoError(t, err)
	require.Len(t, sources, 1)
	parsed, err := provider.Parse(t.Context(), parser.ParseRequest{Source: sources[0]})
	require.NoError(t, err)
	require.Len(t, parsed.Results, 1)
	parsed.Results[0].Result.Session.ID = "claude:portable"
	assert.Equal(t, []parser.PRLink{
		{URL: "https://github.com/o/r/pull/7", Host: "github.com", Repository: "o/r", Number: 7},
		{URL: "http://github.com/o/r/pull/7", Host: "github.com", Repository: "o/r", Number: 7},
	}, parsed.Results[0].Result.Session.PRLinks)

	// The stored capture represents output from the earlier URL-keyed parser.
	// Reprocessing uses the current parser, whose output must hash identically.
	prior := parsed
	prior.Results = append([]parser.ParseResultOutcome(nil), parsed.Results...)
	prior.Results[0].Result.Session.PRLinks = []parser.PRLink{
		{URL: "https://github.com/o/r/pull/7", Host: "github.com", Repository: "o/r", Number: 7},
		{URL: "http://github.com/o/r/pull/7", Host: "github.com", Repository: "o/r", Number: 7},
	}
	f := newProjectionFixture(t)
	a, _ := f.accept(t, "device-a", "initial-a", "", parser.AgentClaude)
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, a), a, rawderive.ParsedManifest{Outcome: prior}))
	b, _ := f.accept(t, "device-b", "initial-b", "", parser.AgentClaude)
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, b), b, rawderive.ParsedManifest{Outcome: prior}))
	aliasA, aliasB := f.alias(t, a), f.alias(t, b)
	before, err := f.sink.Resolve(t.Context(), "claude:portable")
	require.NoError(t, err)
	require.Equal(t, RawIdentityUnique, before.State)

	_, err = f.sink.SelectSourceGeneration(t.Context(), a, "parser-2")
	require.NoError(t, err)
	leases, err := f.jobs.ClaimRawParseJobs(t.Context(), "upgrade-worker", 1, time.Minute)
	require.NoError(t, err)
	require.Len(t, leases, 1)
	require.NoError(t, f.sink.Project(t.Context(), leases[0], a, rawderive.ParsedManifest{Outcome: parsed}))
	for _, alias := range []string{aliasA, aliasB, "claude:portable"} {
		resolved, err := f.sink.Resolve(t.Context(), alias)
		require.NoError(t, err)
		require.Equal(t, RawIdentityUnique, resolved.State)
		assert.Equal(t, before.PublicID, resolved.PublicID)
		assert.Equal(t, before.SessionID, resolved.SessionID)
	}

	h, err := newHostedAdapter(t.Context(), f.runtime, f.tenant)
	require.NoError(t, err)
	pr, err := db.ParsePRFilter("o/r#7")
	require.NoError(t, err)
	page, err := h.ListSessions(t.Context(), db.SessionFilter{PR: pr, Limit: 10})
	require.NoError(t, err)
	require.Len(t, page.Sessions, 1)
	assert.Equal(t, before.PublicID, page.Sessions[0].ID)
	assert.Equal(t, []db.PRLink{{URL: "https://github.com/o/r/pull/7", Host: "github.com", Repository: "o/r", Number: 7}}, page.Sessions[0].PRLinks)
}

func TestRawProjectionFutureVersionDialogueRefresh(t *testing.T) {
	f := newProjectionFixture(t)
	outcome := projectionOutcome("https://github.com/o/r/pull/1\n[Thinking]\nhttps://github.com/o/r/pull/2\n[/Thinking]")
	message := &outcome.Outcome.Results[0].Result.Messages[0]
	message.DialogueText = nil
	m, _ := f.accept(t, "device-a", "future", "")
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, m), m, outcome))
	resolved, err := f.sink.Resolve(t.Context(), "codex:portable")
	require.NoError(t, err)
	future := db.CurrentDataVersion() + 1
	_, err = f.runtime.ExecContext(t.Context(), `UPDATE sessions SET data_version=$2, pr_links=$3 WHERE id=$1`, resolved.SessionID, future, db.EncodePRLinks([]db.PRLink{{URL: "https://github.com/o/r/pull/99", Host: "github.com", Repository: "o/r", Number: 99}}))
	require.NoError(t, err)
	_, err = NewRawProjectionStore(t.Context(), f.runtime, RawProjectionOptions{Tenant: f.tenant})
	var tooNew *db.DataVersionTooNewError
	require.ErrorAs(t, err, &tooNew)
	message.DialogueText = new("https://github.com/o/r/pull/1")
	_, err = f.sink.SelectSourceGeneration(t.Context(), m, "parser-future")
	require.NoError(t, err)
	leases, err := f.jobs.ClaimRawParseJobs(t.Context(), "future-worker", 1, time.Minute)
	require.NoError(t, err)
	require.Len(t, leases, 1)
	err = f.sink.Project(t.Context(), leases[0], m, outcome)
	require.ErrorAs(t, err, &tooNew)
	var links string
	var version int
	require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT pr_links,data_version FROM sessions WHERE id=$1`, resolved.SessionID).Scan(&links, &version))
	assert.Equal(t, future, version)
	assert.Equal(t, []db.PRLink{{URL: "https://github.com/o/r/pull/99", Host: "github.com", Repository: "o/r", Number: 99}}, db.DecodePRLinks(links))
}
