package db

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/parser"
)

// TestResetAllMtimes_ZeroesMtimesAndClearsFreshness pins the forced
// full-resync contract: ResetAllMtimes must zero file_mtime on every
// session AND drop the provider_freshness side-table in one atomic
// step, so the per-component stat-digest shortcut cannot skip
// re-processing a session whose mtime was just zeroed.
func TestResetAllMtimes_ZeroesMtimesAndClearsFreshness(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	const sessionID = "codebuff:proj:1704067200"
	const chatPath = "/sessions/proj/chats/1704067200"
	insertSession(t, d, sessionID, "proj", func(s *Session) {
		s.Agent = "codebuff"
		s.FileMtime = new(int64(1704067200))
	})
	require.NoError(t, d.UpsertProviderStatHash(
		ctx, parser.AgentCodebuff, chatPath, 42))

	_, mtime, ok := d.GetSessionFileInfo(sessionID)
	require.True(t, ok)
	require.Equal(t, int64(1704067200), mtime,
		"precondition: session must start with a non-zero mtime")
	hash, present, err := d.GetProviderStatHash(
		ctx, parser.AgentCodebuff, chatPath)
	require.NoError(t, err)
	require.True(t, present,
		"precondition: freshness row must exist before the reset")
	require.Equal(t, uint64(42), hash)

	require.NoError(t, d.ResetAllMtimes())

	_, mtime, ok = d.GetSessionFileInfo(sessionID)
	require.True(t, ok)
	assert.Zero(t, mtime, "file_mtime must be zeroed for every session")
	_, present, err = d.GetProviderStatHash(
		ctx, parser.AgentCodebuff, chatPath)
	require.NoError(t, err)
	assert.False(t, present,
		"provider_freshness must be cleared so the stat-digest "+
			"shortcut cannot defeat the forced re-sync")
}

func TestRestoreSessionStalesSourceAndClearsFreshness(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()
	path := "/sessions/project/transcript.jsonl"

	insertSessionWithSourcePath(t, d, "restored", "claude", path)
	require.NoError(t, d.SetSessionDataVersion(
		"restored", CurrentDataVersion(),
	))
	require.NoError(t, d.UpsertProviderStatHash(
		ctx, parser.AgentClaude, path, 42,
	))
	require.NoError(t, d.SoftDeleteSession("restored"))

	restored, err := d.RestoreSession("restored")
	require.NoError(t, err)
	require.EqualValues(t, 1, restored)
	assert.Less(t, d.GetSessionDataVersion("restored"), CurrentDataVersion(),
		"restoring a source member must force a source reparse")
	_, present, err := d.GetProviderStatHash(
		ctx, parser.AgentClaude, path,
	)
	require.NoError(t, err)
	assert.False(t, present,
		"restoring a source member must invalidate its source digest")
}

func TestGetSessionFilePathNotSourceMissing(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()
	path := "/sessions/project/transcript.jsonl"
	insertSessionWithSourcePath(t, d, "active", "claude", path)
	insertSessionWithSourcePath(t, d, "trashed", "claude", path)
	require.NoError(t, d.SoftDeleteSession("trashed"))
	insertSessionWithSourcePath(t, d, "missing", "claude", path,
		func(s *Session) { s.Machine = "local" })
	require.NoError(t, d.BaselineActiveSessionSourceOwnerships(
		ctx, []SessionSourceOwnership{{
			ID: "missing", Machine: "local", Agent: "claude", FilePath: path,
		}},
	))
	tombstoned, err := d.SoftDeleteSessionSourceOwnership(
		ctx, "local", "claude", "missing", path,
	)
	require.NoError(t, err)
	require.True(t, tombstoned)
	require.Equal(t, path, d.GetSessionFilePath("missing"),
		"the unfiltered lookup still returns tombstoned paths")

	tests := []struct {
		name string
		id   string
		want string
	}{
		{name: "active row keeps its path", id: "active", want: path},
		{name: "user-trashed row keeps its path", id: "trashed", want: path},
		{name: "source-missing row reads as absent", id: "missing", want: ""},
		{name: "unknown id reads as absent", id: "unknown", want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, d.GetSessionFilePathNotSourceMissing(tt.id))
		})
	}
}
