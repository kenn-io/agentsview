//go:build pgtest

package postgres

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/rawderive"
	"go.kenn.io/agentsview/internal/rawsync"
)

func TestRawProjectionPrefixCopiesRetainSourceHistory(t *testing.T) {
	t.Parallel()
	for _, longerFirst := range []bool{false, true} {
		t.Run(fmt.Sprint(longerFirst), func(t *testing.T) {
			f := newProjectionFixture(t)
			short := projectionOutcome("same prompt")
			short.Outcome.Results[0].Result.Messages = short.Outcome.Results[0].Result.Messages[:1]
			long := projectionOutcome("same prompt")
			var b rawsync.CanonicalManifest
			var accepted rawsync.CommitResult
			if longerFirst {
				b, accepted = f.accept(t, "device-b", "long", "")
				require.NoError(t, f.sink.Project(t.Context(), f.lease(t, b), b, long))
				// Rebuild the selected retained longer copy, not the incoming
				// shorter payload that this next projection inserts.
				retained, err := f.sink.Resolve(t.Context(), "codex:portable")
				require.NoError(t, err)
				_, err = f.runtime.ExecContext(t.Context(), rawSourceProofDetachSQL, retained.SessionID)
				require.NoError(t, err)
				_, err = f.runtime.ExecContext(t.Context(), `DELETE FROM sessions WHERE id=$1`, retained.SessionID)
				require.NoError(t, err)
			}
			a, shortAccepted := f.accept(t, "device-a", "short", "")
			require.NoError(t, f.sink.Project(t.Context(), f.lease(t, a), a, short))
			if !longerFirst {
				b, accepted = f.accept(t, "device-b", "long", "")
				require.NoError(t, f.sink.Project(t.Context(), f.lease(t, b), b, long))
			}
			h, err := NewHostedStore(t.Context(), f.dsn, f.schema, f.tenant, false)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, h.Close()) })
			page, err := h.ListSessions(t.Context(), db.SessionFilter{Limit: 10})
			require.NoError(t, err)
			require.Len(t, page.Sessions, 1)
			assert.Equal(t, "codex:portable", page.Sessions[0].ID)
			messages, err := h.GetMessages(t.Context(), "codex:portable", 0, 10, true)
			require.NoError(t, err)
			require.Len(t, messages, 2)
			_, err = h.StarSession(t.Context(), "codex:portable")
			require.NoError(t, err)
			require.NoError(t, f.sink.SetPin(t.Context(), f.alias(t, a), 0, true, "retained prefix"))
			a, _ = f.accept(t, "device-a", "same-short", shortAccepted.Receipt)
			require.NoError(t, f.sink.Project(t.Context(), f.lease(t, a), a, short))
			var sources, captures int
			resolved, err := f.sink.Resolve(t.Context(), "codex:portable")
			require.NoError(t, err)
			require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT count(*),count(DISTINCT session_id) FROM session_sources WHERE physical_session_id=$1`, resolved.SessionID).Scan(&sources, &captures))
			assert.Equal(t, 2, sources)
			assert.Equal(t, 2, captures)
			// Removing the longer source must reveal the original shorter capture,
			// not leave the other device claiming content it never supplied.
			b, _ = f.accept(t, "device-b", "removed", accepted.Receipt)
			require.NoError(t, f.sink.Project(t.Context(), f.lease(t, b), b, rawderive.ParsedManifest{Outcome: parser.ParseOutcome{ResultSetComplete: true, ForceReplace: true}}))
			messages, err = h.GetMessages(t.Context(), "codex:portable", 0, 10, true)
			require.NoError(t, err)
			require.Len(t, messages, 1)
			assert.Equal(t, "same prompt", messages[0].Content)
			stars, err := h.ListStarredSessionIDs(t.Context())
			require.NoError(t, err)
			assert.Contains(t, stars, "codex:portable")
			pins, err := h.ListPinnedMessages(t.Context(), "codex:portable", "")
			require.NoError(t, err)
			require.Len(t, pins, 1)
			assert.Equal(t, new("retained prefix"), pins[0].Note)
		})
	}
}

func TestRawProjectionPrefixDoesNotChooseDivergentContinuation(t *testing.T) {
	t.Parallel()
	f := newProjectionFixture(t)
	for i := range 3 {
		outcome := projectionOutcome("shared prompt")
		if i == 0 {
			outcome.Outcome.Results[0].Result.Messages = outcome.Outcome.Results[0].Result.Messages[:1]
		} else if i == 2 {
			outcome.Outcome.Results[0].Result.Messages[1].Content = "different continuation"
		}
		m, _ := f.accept(t, fmt.Sprintf("device-%d", i), fmt.Sprintf("capture-%d", i), "")
		require.NoError(t, f.sink.Project(t.Context(), f.lease(t, m), m, outcome))
	}
	resolved, err := f.sink.Resolve(t.Context(), "codex:portable")
	require.NoError(t, err)
	assert.Equal(t, RawIdentityAmbiguous, resolved.State)
	require.Len(t, resolved.Variants, 3)
	for _, alias := range resolved.Variants {
		variant, err := f.sink.Resolve(t.Context(), alias)
		require.NoError(t, err)
		assert.Equal(t, RawIdentityUnique, variant.State)
		var exists bool
		require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT EXISTS(SELECT 1 FROM sessions WHERE id=$1)`, variant.SessionID).Scan(&exists))
		assert.True(t, exists)
	}
}

func TestRawProjectionRecordedRemoteSurvivesRematerialization(t *testing.T) {
	t.Parallel()
	f := newProjectionFixture(t)
	old := projectionOutcome("same prompt")
	old.Outcome.Results[0].Result.Messages[1].ToolCalls[0].InputJSON = `{"command":"gh pr view 42"}`
	old.Outcome.Results[0].Result.Session.GitRemote = "https://github.com/example/repo.git"
	a, _ := f.accept(t, "device-a", "old", "")
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, a), a, old))
	updated := projectionOutcome("same prompt")
	updated.Outcome.Results[0].Result.Messages[1].ToolCalls[0].InputJSON = `{"command":"gh pr view 42"}`
	updated.Outcome.Results[0].Result.Session.GitRemote = "https://github.com/example/repo.git"
	b, _ := f.accept(t, "device-b", "new", "")
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, b), b, updated))
	current, err := f.sink.Resolve(t.Context(), "codex:portable")
	require.NoError(t, err)
	var encoded string
	require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT pr_links FROM sessions WHERE id=$1`, current.SessionID).Scan(&encoded))
	require.Len(t, db.DecodePRLinks(encoded), 1)
	assert.Equal(t, "https://github.com/example/repo/pull/42", db.DecodePRLinks(encoded)[0].URL)
	_, err = f.runtime.ExecContext(t.Context(), rawSourceProofDetachSQL, current.SessionID)
	require.NoError(t, err)
	_, err = f.runtime.ExecContext(t.Context(), `DELETE FROM sessions WHERE id=$1`, current.SessionID)
	require.NoError(t, err)
	c, _ := f.accept(t, "device-c", "rebuild", "")
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, c), c, updated))
	require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT pr_links FROM sessions WHERE id=$1`, current.SessionID).Scan(&encoded))
	require.Len(t, db.DecodePRLinks(encoded), 1)
	assert.Equal(t, "https://github.com/example/repo/pull/42", db.DecodePRLinks(encoded)[0].URL)
}

func TestRawProjectionRemoteFollowsActiveSources(t *testing.T) {
	t.Parallel()
	f := newProjectionFixture(t)
	outcome := func(remote string) rawderive.ParsedManifest {
		p := projectionOutcome("same prompt")
		p.Outcome.Results[0].Result.Messages[1].ToolCalls[0].InputJSON = `{"command":"gh pr view 42"}`
		p.Outcome.Results[0].Result.Session.GitRemote = remote
		return p
	}
	a, receipt := f.accept(t, "device-a", "first", "")
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, a), a, outcome("https://github.com/example/repo.git")))
	first, err := f.sink.Resolve(t.Context(), "codex:portable")
	require.NoError(t, err)
	b, bReceipt := f.accept(t, "device-b", "second", "")
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, b), b, outcome("https://github.com/example/other.git")))
	both, err := f.sink.Resolve(t.Context(), "codex:portable")
	require.NoError(t, err)
	assert.Equal(t, first.SessionID, both.SessionID)
	var links string
	require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT pr_links FROM sessions WHERE id=$1`, both.SessionID).Scan(&links))
	assert.Empty(t, db.DecodePRLinks(links))
	removed, _ := f.accept(t, "device-a", "removed", receipt.Receipt)
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, removed), removed, rawderive.ParsedManifest{Outcome: parser.ParseOutcome{ResultSetComplete: true, ForceReplace: true}}))
	remaining, err := f.sink.Resolve(t.Context(), "codex:portable")
	require.NoError(t, err)
	assert.Equal(t, first.SessionID, remaining.SessionID)
	require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT pr_links FROM sessions WHERE id=$1`, remaining.SessionID).Scan(&links))
	require.Len(t, db.DecodePRLinks(links), 1)
	assert.Equal(t, "https://github.com/example/other/pull/42", db.DecodePRLinks(links)[0].URL)
	unknown, unknownReceipt := f.accept(t, "device-c", "missing-remote", "")
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, unknown), unknown, outcome("")))
	require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT pr_links FROM sessions WHERE id=$1`, remaining.SessionID).Scan(&links))
	assert.Empty(t, db.DecodePRLinks(links))
	withdrawn, _ := f.accept(t, "device-c", "missing-remote-removed", unknownReceipt.Receipt)
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, withdrawn), withdrawn, rawderive.ParsedManifest{Outcome: parser.ParseOutcome{ResultSetComplete: true, ForceReplace: true}}))
	require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT pr_links FROM sessions WHERE id=$1`, remaining.SessionID).Scan(&links))
	require.Len(t, db.DecodePRLinks(links), 1)
	assert.Equal(t, "https://github.com/example/other/pull/42", db.DecodePRLinks(links)[0].URL)
	updated, _ := f.accept(t, "device-b", "updated-remote", bReceipt.Receipt)
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, updated), updated, outcome("https://github.com/example/repo.git")))
	require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT pr_links FROM sessions WHERE id=$1`, remaining.SessionID).Scan(&links))
	require.Len(t, db.DecodePRLinks(links), 1)
	assert.Equal(t, "https://github.com/example/repo/pull/42", db.DecodePRLinks(links)[0].URL)
}

func TestRawProjectionPRCommandContext(t *testing.T) {
	t.Parallel()
	f := newProjectionFixture(t)
	outcome := projectionOutcome("same prompt")
	result := &outcome.Outcome.Results[0].Result
	result.Session.Cwd = "/work/repo"
	result.Session.GitRemote = "https://github.com/example/repo.git"
	call := result.Messages[1].ToolCalls[0]
	call.InputJSON = `{"command":"gh pr edit --add-label 99 42"}`
	second := call
	second.ToolUseID = "call-2"
	second.InputJSON = `{"command":"gh pr view 43 -R github.com/example/repo"}`
	third := call
	third.ToolUseID = "call-3"
	third.InputJSON = `{"command":"gh pr view 44","workdir":"/work/other"}`
	result.Messages[1].ToolCalls = []parser.ParsedToolCall{call, second, third}
	receipt, _ := f.accept(t, "device-a", "commands", "")
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, receipt), receipt, outcome))
	session, err := f.sink.Resolve(t.Context(), "codex:portable")
	require.NoError(t, err)
	var encoded string
	require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT pr_links FROM sessions WHERE id=$1`, session.SessionID).Scan(&encoded))
	links := db.DecodePRLinks(encoded)
	require.Len(t, links, 2)
	assert.Equal(t, 42, links[0].Number)
	assert.Equal(t, 43, links[1].Number)
}
