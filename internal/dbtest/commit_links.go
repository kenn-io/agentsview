package dbtest

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	gitlog "go.kenn.io/agentsview/internal/db/git"
)

func SeedCommitLinks(t *testing.T, d *db.DB) {
	t.Helper()
	for i, id := range []string{"commit-live", "commit-trash", "commit-delete", "commit-clash"} {
		SeedSession(t, d, id, "commit-project")
		sha := "abc1234567890123456789012345678901234567"
		if i == 3 {
			sha = "abc1234ff7890123456789012345678901234567"
		}
		result := "[main " + sha + "] Save the change"
		require.NoError(t, d.ReplaceSessionMessages(t.Context(), id, []db.Message{
			{SessionID: id, Ordinal: 0, Role: "user", Content: "Record the parser fix"},
			{SessionID: id, Ordinal: 1, Role: "assistant", HasToolUse: true, ToolCalls: []db.ToolCall{{ToolName: "Bash", Category: "Bash", ToolUseID: "commit-call", InputJSON: `{"command":"git commit -m change"}`, ResultContent: result, ResultEvents: []db.ToolResultEvent{{Source: "tool_result", Content: result}}}}},
		}))
	}
	SeedSession(t, d, "commit-trailer", "commit-project")
	require.NoError(t, d.ReplaceSessionMessages(t.Context(), "commit-trailer", []db.Message{{SessionID: "commit-trailer", Ordinal: 0, Role: "user", Content: "Save the editor change", Timestamp: "2026-01-01T00:00:00Z"}}))
	require.NoError(t, d.StoreTrailerCommitLinks(t.Context(), "trailer-repo", []gitlog.TrailerCommit{{SHA: strings.Repeat("b", 40), Sessions: []string{"claude:commit-trailer"}, Subject: "Editor change", CommittedAt: "2026-01-01T00:01:00Z"}}))
	require.NoError(t, d.SoftDeleteSession(t.Context(), "commit-trash"))
	SeedSession(t, d, "commit-order", "commit-project")
	require.NoError(t, d.StoreTrailerCommitLinks(t.Context(), "order-repo", []gitlog.TrailerCommit{
		{SHA: strings.Repeat("e", 40), Sessions: []string{"claude:commit-order"}, Subject: "First", CommittedAt: "2026-01-01T00:01:00Z"},
		{SHA: strings.Repeat("d", 40), Sessions: []string{"claude:commit-order"}, Subject: "Second", CommittedAt: "2026-01-01T00:02:00Z"},
	}))
}

func AssertCommitLinkParity(t *testing.T, local *db.DB, store db.CommitLinkReader) {
	t.Helper()
	queries := []string{"ABC1234567890123456789012345678901234567", "abc1234", "abc123456", "deadbee", strings.Repeat("b", 40), strings.Repeat("c", 40), strings.Repeat("a", 64)}
	want, err := local.CommitSessionLinks(t.Context(), queries)
	require.NoError(t, err)
	got, err := store.CommitSessionLinks(t.Context(), queries)
	require.NoError(t, err)
	assert.Equal(t, want, got)
	require.NotEmpty(t, got.Commits[0].Links)
	assert.Equal(t, "Save the change", got.Commits[0].Links[0].Subject)
	for _, id := range []string{"commit-live", "commit-trash", "commit-delete", "commit-trailer", "commit-order", "unknown"} {
		want, err := local.SessionCommitLinks(t.Context(), id)
		require.NoError(t, err)
		got, err := store.SessionCommitLinks(t.Context(), id)
		require.NoError(t, err)
		assert.Equal(t, want, got)
		if id == "commit-order" {
			require.Len(t, want.Commits, 2)
			require.Len(t, got.Commits, 2)
			assert.Equal(t, "First", want.Commits[0].Subject)
			assert.Equal(t, "Second", want.Commits[1].Subject)
			assert.Equal(t, "First", got.Commits[0].Subject)
			assert.Equal(t, "Second", got.Commits[1].Subject)
		}
		if id == "commit-live" {
			require.Len(t, got.Commits, 1)
			require.NotNil(t, got.Commits[0].Turn)
			assert.Equal(t, "Record the parser fix", got.Commits[0].Turn.Text)
		}
	}
	// Exercise the advertised batch limit with SHA-256 queries.
	batch := make([]string, 200)
	for i := range batch {
		batch[i] = strings.Repeat("a", 64)
	}
	result, err := store.CommitSessionLinks(t.Context(), batch)
	require.NoError(t, err)
	require.Len(t, result.Commits, 200)
	assert.Empty(t, result.Commits[199].Links)
}

// AssertRetainedCommitLinkScope checks that an archive can reproduce filtered
// retained links even when the replica has no previous project evidence.
func AssertRetainedCommitLinkScope(t *testing.T, local *db.DB, reader func() db.CommitLinkReader, push func(projects, exclude []string)) {
	t.Helper()
	require.NoError(t, local.DeleteSession(t.Context(), "commit-delete"))
	for _, step := range []struct {
		name              string
		projects, exclude []string
		want              bool
	}{
		{name: "delete before first push", projects: []string{"commit-project"}, want: true},
		{name: "narrow scope", projects: []string{"other"}},
		{name: "widen scope", projects: []string{"commit-project"}, want: true},
		{name: "exclude project", exclude: []string{"commit-project"}},
		{name: "include after exclusion", exclude: []string{"other"}, want: true},
	} {
		t.Run(step.name, func(t *testing.T) {
			push(step.projects, step.exclude)
			links, err := reader().SessionCommitLinks(t.Context(), "commit-delete")
			require.NoError(t, err)
			if !step.want {
				assert.Empty(t, links.Commits)
				return
			}
			require.Len(t, links.Commits, 1)
			assert.Equal(t, "abc1234567890123456789012345678901234567", links.Commits[0].SHA)
			assert.True(t, links.Commits[0].Missing)
			assert.Nil(t, links.Commits[0].Turn)
		})
	}
	// Move after the last publication, then delete without another live push.
	session, err := local.GetSession(t.Context(), "commit-live")
	require.NoError(t, err)
	require.NotNil(t, session)
	session.Project = "other"
	require.NoError(t, local.UpsertSession(t.Context(), *session))
	require.NoError(t, local.DeleteSession(t.Context(), "commit-live"))
	push([]string{"commit-project"}, nil)
	links, err := reader().SessionCommitLinks(t.Context(), "commit-live")
	require.NoError(t, err)
	assert.Empty(t, links.Commits, "the old project must not retain the moved session")
	push([]string{"other"}, nil)
	links, err = reader().SessionCommitLinks(t.Context(), "commit-live")
	require.NoError(t, err)
	require.Len(t, links.Commits, 1)
	assert.True(t, links.Commits[0].Missing)
	assert.Equal(t, "Save the change", links.Commits[0].Subject)
}

// AssertTrailerIncrementalPush checks changes without rewriting the transcript.
func AssertTrailerIncrementalPush(t *testing.T, local *db.DB, reader func() db.CommitLinkReader, push func()) {
	t.Helper()
	require.NoError(t, local.StoreTrailerCommitLinks(t.Context(), "trailer-repo", []gitlog.TrailerCommit{
		{SHA: strings.Repeat("c", 40), Sessions: []string{"claude:commit-trailer"}, Subject: "Amended editor change", CommittedAt: "2026-01-01T00:01:00Z"},
	}))
	links, err := local.SessionCommitLinks(t.Context(), "commit-trailer")
	require.NoError(t, err)
	require.Len(t, links.Commits, 2)
	assert.ElementsMatch(t, []string{strings.Repeat("b", 40), strings.Repeat("c", 40)}, []string{links.Commits[0].SHA, links.Commits[1].SHA})
	push()
	AssertCommitLinkParity(t, local, reader())
}
