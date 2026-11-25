//go:build pgtest

package postgres

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/ingest"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/rawderive"
	"go.kenn.io/agentsview/internal/rawsync"
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
				var metadataBytes []byte
				require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT prior_payload FROM raw_session_branches WHERE session_id=$1`, before.SessionID).Scan(&metadataBytes))
				metadata, err := decodeRawPayload(metadataBytes)
				require.NoError(t, err)
				metadata.Session.PRLinks = nil
				metadata.Session.DataVersion = 0
				metadataBytes, err = encodeRawPayload(metadata)
				require.NoError(t, err)
				_, err = f.runtime.ExecContext(t.Context(), `UPDATE raw_session_branches SET prior_payload=$2 WHERE session_id=$1`, before.SessionID, metadataBytes)
				require.NoError(t, err)
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
	b, _ := f.accept(t, "device-b", "long", "")
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, b), b, long))
	resolved, err := f.sink.Resolve(t.Context(), "codex:portable")
	require.NoError(t, err)
	assert.Equal(t, RawIdentityUnique, resolved.State)
	var links string
	require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT pr_links FROM sessions WHERE id=$1`, resolved.SessionID).Scan(&links))
	assert.Equal(t, []db.PRLink{{URL: "https://github.com/o/r/pull/2", Host: "github.com", Repository: "o/r", Number: 2}}, db.DecodePRLinks(links))
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

	f := newProjectionFixture(t)
	m, _ := f.accept(t, "device-a", "initial", "", parser.AgentClaude)
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, m), m, rawderive.ParsedManifest{Outcome: parsed}))

	h, err := newHostedAdapter(t.Context(), f.runtime, f.tenant)
	require.NoError(t, err)
	pr, err := db.ParsePRFilter("o/r#7")
	require.NoError(t, err)
	page, err := h.ListSessions(t.Context(), db.SessionFilter{PR: pr, Limit: 10})
	require.NoError(t, err)
	require.Len(t, page.Sessions, 1)
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
	message.DialogueText = new("https://github.com/o/r/pull/1")
	_, err = f.sink.SelectSourceGeneration(t.Context(), m, "parser-future")
	require.NoError(t, err)
	leases, err := f.jobs.ClaimRawParseJobs(t.Context(), "future-worker", 1, time.Minute)
	require.NoError(t, err)
	require.Len(t, leases, 1)
	err = f.sink.Project(t.Context(), leases[0], m, outcome)
	var tooNew *db.DataVersionTooNewError
	require.ErrorAs(t, err, &tooNew)
	var links string
	var version int
	require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT pr_links,data_version FROM sessions WHERE id=$1`, resolved.SessionID).Scan(&links, &version))
	assert.Equal(t, future, version)
	assert.Equal(t, []db.PRLink{{URL: "https://github.com/o/r/pull/99", Host: "github.com", Repository: "o/r", Number: 99}}, db.DecodePRLinks(links))
}

func TestRawProjectionCommandPRLinks(t *testing.T) {
	for _, policy := range []config.ArchiveContent{config.ArchiveContentFull, config.ArchiveContentTranscripts, config.ArchiveContentUsage} {
		t.Run(string(policy), func(t *testing.T) {
			f := newProjectionFixture(t)
			var err error
			f.sink, err = NewRawProjectionStore(t.Context(), f.runtime, RawProjectionOptions{Tenant: f.tenant, Content: ingest.ContentOptions{ArchiveContent: policy}})
			require.NoError(t, err)
			outcome := projectionOutcome("start")
			outcome.Outcome.Results[0].Result.Messages[1].ToolCalls[0].InputJSON = `{"command":"gh pr view 43 -R example/repo"}`
			m, _ := f.accept(t, "device-a", "initial", "")
			require.NoError(t, f.sink.Project(t.Context(), f.lease(t, m), m, outcome))
			resolved, err := f.sink.Resolve(t.Context(), "codex:portable")
			require.NoError(t, err)
			var links string
			require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT pr_links FROM sessions WHERE id=$1`, resolved.SessionID).Scan(&links))
			assert.Equal(t, []db.PRLink{{URL: "https://github.com/example/repo/pull/43", Host: "github.com", Repository: "example/repo", Number: 43}}, db.DecodePRLinks(links))
			if policy == config.ArchiveContentUsage {
				outcome.Outcome.Results[0].Result.Messages[1].ToolCalls[0].InputJSON = `{"command":"echo hello"}`
				m, receipt := f.accept(t, "device-b", "same-content", "")
				require.NoError(t, f.sink.Project(t.Context(), f.lease(t, m), m, outcome))
				var captured string
				require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT captured_session_id FROM raw_session_branches WHERE manifest_id=$1`, m.ManifestID).Scan(&captured))
				assert.Equal(t, resolved.SessionID, captured)
				require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT pr_links FROM sessions WHERE id=$1`, resolved.SessionID).Scan(&links))
				assert.Equal(t, []db.PRLink{{URL: "https://github.com/example/repo/pull/43", Host: "github.com", Repository: "example/repo", Number: 43}}, db.DecodePRLinks(links))
				outcome.Outcome.Results[0].Result.Messages[1].ToolCalls[0].InputJSON = `{"command":"gh pr view 44 -R example/repo"}`
				m, _ = f.accept(t, "device-b", "new-link", receipt.Receipt)
				require.NoError(t, f.sink.Project(t.Context(), f.lease(t, m), m, outcome))
				require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT pr_links FROM sessions WHERE id=$1`, resolved.SessionID).Scan(&links))
				want := []db.PRLink{
					{URL: "https://github.com/example/repo/pull/43", Host: "github.com", Repository: "example/repo", Number: 43},
					{URL: "https://github.com/example/repo/pull/44", Host: "github.com", Repository: "example/repo", Number: 44},
				}
				assert.ElementsMatch(t, want, db.DecodePRLinks(links))
				_, err = f.runtime.ExecContext(t.Context(), rawSourceProofDetachSQL, resolved.SessionID)
				require.NoError(t, err)
				_, err = f.runtime.ExecContext(t.Context(), `DELETE FROM sessions WHERE id=$1`, resolved.SessionID)
				require.NoError(t, err)
				outcome.Outcome.Results[0].Result.Messages[1].ToolCalls[0].InputJSON = `{"command":"echo hello"}`
				m, _ = f.accept(t, "device-c", "restore", "")
				require.NoError(t, f.sink.Project(t.Context(), f.lease(t, m), m, outcome))
				restored, err := f.sink.Resolve(t.Context(), "codex:portable")
				require.NoError(t, err)
				require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT pr_links FROM sessions WHERE id=$1`, restored.SessionID).Scan(&links))
				assert.ElementsMatch(t, want, db.DecodePRLinks(links))
			}
		})
	}
}

func TestRawProjectionWithdrawnDeviceLinks(t *testing.T) {
	for _, tc := range []struct {
		name    string
		outcome parser.ParseOutcome
	}{
		{"exclude", parser.ParseOutcome{ResultSetComplete: true, ExcludedSessionIDs: []string{"codex:portable"}}},
		{"replace", parser.ParseOutcome{ResultSetComplete: true, ForceReplace: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newProjectionFixture(t)
			var err error
			f.sink, err = NewRawProjectionStore(t.Context(), f.runtime, RawProjectionOptions{Tenant: f.tenant, Content: ingest.ContentOptions{ArchiveContent: config.ArchiveContentUsage}})
			require.NoError(t, err)
			outcome := projectionOutcome("start")
			outcome.Outcome.Results[0].Result.Messages[1].ToolCalls[0].InputJSON = `{"command":"gh pr view 41 -R example/repo"}`
			a, receipt := f.accept(t, "device-a", "initial", "")
			require.NoError(t, f.sink.Project(t.Context(), f.lease(t, a), a, outcome))
			outcome.Outcome.Results[0].Result.Messages[1].ToolCalls[0].InputJSON = `{"command":"gh pr view 42 -R example/repo"}`
			b, _ := f.accept(t, "device-b", "initial", "")
			require.NoError(t, f.sink.Project(t.Context(), f.lease(t, b), b, outcome))
			resolved, err := f.sink.Resolve(t.Context(), "codex:portable")
			require.NoError(t, err)
			var links string
			require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT pr_links FROM sessions WHERE id=$1`, resolved.SessionID).Scan(&links))
			require.Len(t, db.DecodePRLinks(links), 2)
			a, _ = f.accept(t, "device-a", "withdrawn", receipt.Receipt)
			require.NoError(t, f.sink.Project(t.Context(), f.lease(t, a), a, rawderive.ParsedManifest{Outcome: tc.outcome}))
			require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT pr_links FROM sessions WHERE id=$1`, resolved.SessionID).Scan(&links))
			assert.Equal(t, []db.PRLink{{URL: "https://github.com/example/repo/pull/42", Host: "github.com", Repository: "example/repo", Number: 42}}, db.DecodePRLinks(links))
		})
	}
}

func prepareRetainedToolLinks(t *testing.T) (projectionFixture, rawderive.ParsedManifest, rawsync.CommitResult, RawIdentity, RawIdentity) {
	t.Helper()
	f := newProjectionFixture(t)
	var err error
	f.sink, err = NewRawProjectionStore(t.Context(), f.runtime, RawProjectionOptions{Tenant: f.tenant, Content: ingest.ContentOptions{ArchiveContent: config.ArchiveContentTranscripts}})
	require.NoError(t, err)
	outcome := projectionOutcome("start")
	result := &outcome.Outcome.Results[0].Result
	result.Session.Agent = parser.AgentRooCode
	result.Session.ID = "roocode:portable"
	result.Messages[1].ToolCalls[0].InputJSON = `{"command":"gh pr view 42 -R example/repo"}`
	m, receipt := f.accept(t, "device-a", "initial", "", parser.AgentRooCode)
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, m), m, outcome))
	before, err := f.sink.Resolve(t.Context(), "roocode:portable")
	require.NoError(t, err)
	result.Messages[1].ToolCalls[0].InputJSON = `{"command":"gh pr view 43 -R example/repo"}`
	m, receipt = f.accept(t, "device-a", "updated", receipt.Receipt, parser.AgentRooCode)
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, m), m, outcome))
	after, err := f.sink.Resolve(t.Context(), "roocode:portable")
	require.NoError(t, err)
	assert.Equal(t, before.SessionID, after.SessionID)
	var links string
	require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT pr_links FROM sessions WHERE id=$1`, after.SessionID).Scan(&links))
	assert.Equal(t, []db.PRLink{{URL: "https://github.com/example/repo/pull/43", Host: "github.com", Repository: "example/repo", Number: 43}}, db.DecodePRLinks(links))
	return f, outcome, receipt, before, after
}

func TestRawProjectionRestoresOtherSourceToolLinks(t *testing.T) {
	f, _, _, _, after := prepareRetainedToolLinks(t)
	var links string
	var err error
	// Recreate the longer capture while a different source publishes its prefix.
	_, err = f.runtime.ExecContext(t.Context(), rawSourceProofDetachSQL, after.SessionID)
	require.NoError(t, err)
	_, err = f.runtime.ExecContext(t.Context(), `DELETE FROM sessions WHERE id=$1`, after.SessionID)
	require.NoError(t, err)
	short := projectionOutcome("start")
	short.Outcome.Results[0].Result.Session.Agent = parser.AgentRooCode
	short.Outcome.Results[0].Result.Session.ID = "roocode:portable"
	short.Outcome.Results[0].Result.Messages = short.Outcome.Results[0].Result.Messages[:1]
	other, _ := f.accept(t, "device-b", "short", "", parser.AgentRooCode)
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, other), other, short))
	restored, err := f.sink.Resolve(t.Context(), "roocode:portable")
	require.NoError(t, err)
	require.Equal(t, after.SessionID, restored.SessionID)
	var shortCapture string
	require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT captured_session_id FROM raw_session_branches WHERE manifest_id=$1`, other.ManifestID).Scan(&shortCapture))
	assert.NotEqual(t, restored.SessionID, shortCapture)
	require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT pr_links FROM sessions WHERE id=$1`, restored.SessionID).Scan(&links))
	assert.Equal(t, []db.PRLink{{URL: "https://github.com/example/repo/pull/43", Host: "github.com", Repository: "example/repo", Number: 43}}, db.DecodePRLinks(links))
}

func TestRawProjectionRefreshesRetainedToolLinks(t *testing.T) {
	f, outcome, receipt, before, after := prepareRetainedToolLinks(t)
	result := &outcome.Outcome.Results[0].Result
	var links string
	var err error
	var m rawsync.CanonicalManifest
	for _, tc := range []struct {
		name    string
		removed parser.ParseOutcome
	}{
		{"replace", parser.ParseOutcome{ResultSetComplete: true, ForceReplace: true}},
		{"exclude", parser.ParseOutcome{ResultSetComplete: true, ExcludedSessionIDs: []string{"roocode:portable"}}},
	} {
		m, receipt = f.accept(t, "device-a", "absent-"+tc.name, receipt.Receipt, parser.AgentRooCode)
		require.NoError(t, f.sink.Project(t.Context(), f.lease(t, m), m, rawderive.ParsedManifest{Outcome: tc.removed}))
		m, receipt = f.accept(t, "device-a", "restored-"+tc.name, receipt.Receipt, parser.AgentRooCode)
		require.NoError(t, f.sink.Project(t.Context(), f.lease(t, m), m, outcome))
		after, err = f.sink.Resolve(t.Context(), "roocode:portable")
		require.NoError(t, err)
		assert.Equal(t, before.SessionID, after.SessionID)
		require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT pr_links FROM sessions WHERE id=$1`, after.SessionID).Scan(&links))
		assert.Equal(t, []db.PRLink{{URL: "https://github.com/example/repo/pull/43", Host: "github.com", Repository: "example/repo", Number: 43}}, db.DecodePRLinks(links))
	}
	_, err = f.runtime.ExecContext(t.Context(), `UPDATE sessions SET data_version=$2 WHERE id=$1`, after.SessionID, db.CurrentDataVersion()-1)
	require.NoError(t, err)
	messages := result.Messages
	result.Messages = nil
	m, receipt = f.accept(t, "device-a", "shortened", receipt.Receipt, parser.AgentRooCode)
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, m), m, outcome))
	retained, err := f.sink.Resolve(t.Context(), "roocode:portable")
	require.NoError(t, err)
	assert.Equal(t, before.SessionID, retained.SessionID)
	require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT pr_links FROM sessions WHERE id=$1`, retained.SessionID).Scan(&links))
	assert.Equal(t, []db.PRLink{{URL: "https://github.com/example/repo/pull/43", Host: "github.com", Repository: "example/repo", Number: 43}}, db.DecodePRLinks(links))
	result.Messages = messages
	result.Messages[1].ToolCalls[0].InputJSON = `{"command":"true"}`
	m, receipt = f.accept(t, "device-a", "removed", receipt.Receipt, parser.AgentRooCode)
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, m), m, outcome))
	_, err = f.runtime.ExecContext(t.Context(), `UPDATE sessions SET data_version=$2 WHERE id=$1`, retained.SessionID, db.CurrentDataVersion()-1)
	require.NoError(t, err)
	result.Messages = nil
	m, _ = f.accept(t, "device-a", "shortened-again", receipt.Receipt, parser.AgentRooCode)
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, m), m, outcome))
	retained, err = f.sink.Resolve(t.Context(), "roocode:portable")
	require.NoError(t, err)
	assert.Equal(t, before.SessionID, retained.SessionID)
	require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT pr_links FROM sessions WHERE id=$1`, retained.SessionID).Scan(&links))
	assert.Empty(t, db.DecodePRLinks(links))
}
