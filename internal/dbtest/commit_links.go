package dbtest

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
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
			{SessionID: id, Ordinal: 0, Role: "user", Content: "Save the change"},
			{SessionID: id, Ordinal: 1, Role: "assistant", HasToolUse: true, ToolCalls: []db.ToolCall{{ToolName: "Bash", Category: "Bash", ToolUseID: "commit-call", InputJSON: `{"command":"git commit -m change"}`, ResultContent: result, ResultEvents: []db.ToolResultEvent{{Source: "tool_result", Content: result}}}}},
		}))
	}
	require.NoError(t, d.SoftDeleteSession(t.Context(), "commit-trash"))
}

func AssertCommitLinkParity(t *testing.T, local *db.DB, store db.CommitLinkReader) {
	t.Helper()
	queries := []string{"ABC1234567890123456789012345678901234567", "abc1234", "abc123456", "deadbee", strings.Repeat("a", 64)}
	want, err := local.CommitSessionLinks(t.Context(), queries)
	require.NoError(t, err)
	got, err := store.CommitSessionLinks(t.Context(), queries)
	require.NoError(t, err)
	assert.Equal(t, want, got)
	require.NotEmpty(t, got.Commits[0].Links)
	assert.Equal(t, "Save the change", got.Commits[0].Links[0].Subject)
	for _, id := range []string{"commit-live", "commit-trash", "commit-delete", "unknown"} {
		want, err := local.SessionCommitLinks(t.Context(), id)
		require.NoError(t, err)
		got, err := store.SessionCommitLinks(t.Context(), id)
		require.NoError(t, err)
		assert.Equal(t, want, got)
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

	for _, queries := range [][]string{{"bad"}, make([]string, 201)} {
		_, err := store.CommitSessionLinks(t.Context(), queries)
		require.ErrorIs(t, err, db.ErrInvalidCommitSHA)
	}
}
