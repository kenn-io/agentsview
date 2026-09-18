package db

import (
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
	assert := assert.New(t)
	require := require.New(t)

	d := testDB(t)
	ctx := t.Context()

	const sessionID = "codebuff:proj:1704067200"
	const chatPath = "/sessions/proj/chats/1704067200"
	insertSession(t, d, sessionID, "proj", func(s *Session) {
		s.Agent = "codebuff"
		s.FileMtime = new(int64(1704067200))
	})
	require.NoError(d.UpsertProviderStatHash(
		ctx, parser.AgentCodebuff, chatPath, 42))

	_, mtime, ok := d.GetSessionFileInfo(sessionID)
	require.True(ok)
	require.Equal(int64(1704067200), mtime,
		"precondition: session must start with a non-zero mtime")
	hash, present, err := d.GetProviderStatHash(
		ctx, parser.AgentCodebuff, chatPath)
	require.NoError(err)
	require.True(present,
		"precondition: freshness row must exist before the reset")
	require.Equal(uint64(42), hash)

	require.NoError(d.ResetAllMtimes())

	_, mtime, ok = d.GetSessionFileInfo(sessionID)
	require.True(ok)
	assert.Zero(mtime, "file_mtime must be zeroed for every session")
	_, present, err = d.GetProviderStatHash(
		ctx, parser.AgentCodebuff, chatPath)
	require.NoError(err)
	assert.False(present,
		"provider_freshness must be cleared so the stat-digest "+
			"shortcut cannot defeat the forced re-sync")
}

func TestRestoreSessionStalesSourceAndClearsFreshness(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	d := testDB(t)
	ctx := t.Context()
	path := "/sessions/project/transcript.jsonl"

	insertSessionWithSourcePath(t, d, "restored", "claude", path)
	require.NoError(d.SetSessionDataVersion(
		"restored", CurrentDataVersion(),
	))
	require.NoError(d.UpsertProviderStatHash(
		ctx, parser.AgentClaude, path, 42,
	))
	require.NoError(d.SoftDeleteSession("restored"))

	restored, err := d.RestoreSession("restored")
	require.NoError(err)
	require.EqualValues(1, restored)
	assert.Less(d.GetSessionDataVersion("restored"), CurrentDataVersion(),
		"restoring a source member must force a source reparse")
	_, present, err := d.GetProviderStatHash(
		ctx, parser.AgentClaude, path,
	)
	require.NoError(err)
	assert.False(present,
		"restoring a source member must invalidate its source digest")
}

func TestGetSessionFilePathNotSourceMissing(t *testing.T) {
	require := require.New(t)

	d := testDB(t)
	ctx := t.Context()
	path := "/sessions/project/transcript.jsonl"
	insertSessionWithSourcePath(t, d, "active", "claude", path)
	insertSessionWithSourcePath(t, d, "trashed", "claude", path)
	require.NoError(d.SoftDeleteSession("trashed"))
	insertSessionWithSourcePath(t, d, "missing", "claude", path,
		func(s *Session) { s.Machine = "local" })
	require.NoError(d.BaselineActiveSessionSourceOwnerships(
		ctx, []SessionSourceOwnership{{
			ID: "missing", Machine: "local", Agent: "claude", FilePath: path,
		}},
	))
	tombstoned, err := d.MarkSessionSourceMissing(
		ctx, "local", "claude", "missing", path,
	)
	require.NoError(err)
	require.True(tombstoned)
	require.Equal(path, d.GetSessionFilePath("missing"),
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
