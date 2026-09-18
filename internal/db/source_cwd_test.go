package db

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGetCwdByAgentPathScopesIdentityAndPreservesEmptyRows(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	d := testDB(t)
	path := "cursor-project/agent-transcripts/session.jsonl"
	emptyPath := "empty.jsonl"
	require.NoError(d.UpsertSession(Session{
		ID: "cursor:positive", Agent: "cursor", FilePath: &path, Cwd: "/work/a",
	}))
	require.NoError(d.UpsertSession(Session{
		ID: "codex:same-path", Agent: "codex", FilePath: &path, Cwd: "/work/b",
	}))
	require.NoError(d.UpsertSession(Session{
		ID: "cursor:empty", Agent: "cursor", FilePath: &emptyPath, Cwd: "",
	}))

	cwd, ok := d.GetCwdByAgentPath(path, "cursor")
	assert.True(ok)
	assert.Equal("/work/a", cwd)

	cwd, ok = d.GetCwdByAgentPath(path, "codex")
	assert.True(ok)
	assert.Equal("/work/b", cwd)

	cwd, ok = d.GetCwdByAgentPath("empty.jsonl", "cursor")
	assert.True(ok)
	assert.Empty(cwd)

	cwd, ok = d.GetCwdByAgentPath("missing.jsonl", "cursor")
	assert.False(ok)
	assert.Empty(cwd)

	require.NoError(d.UpdateSessionCwd("cursor:positive", ""))
	cwd, ok = d.GetCwdByAgentPath(path, "cursor")
	assert.True(ok)
	assert.Empty(cwd)
	require.NoError(d.UpdateCwdByAgentPath(path, "cursor", "/work/c"))
	cwd, ok = d.GetCwdByAgentPath(path, "cursor")
	assert.True(ok)
	assert.Equal("/work/c", cwd)
}

func TestGetCwdByAgentPathUsesSourceMissingPreservationRow(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	d := testDB(t)
	path := "cursor-project/agent-transcripts/revive.jsonl"
	require.NoError(d.UpsertSession(Session{
		ID: "cursor:revive", Agent: "cursor", FilePath: &path, Cwd: "/work/revive",
	}))
	require.NoError(d.Update(func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(),
			"UPDATE sessions SET source_missing_at = 'now' WHERE id = ?",
			"cursor:revive",
		)
		return err
	}))

	cwd, ok := d.GetCwdByAgentPath(path, "cursor")
	assert.True(ok)
	assert.Equal("/work/revive", cwd)
}

func TestUpdateCwdByAgentPathDoesNotTouchUnchangedRows(t *testing.T) {
	require := require.New(t)

	d := testDB(t)
	path := "cursor-project/agent-transcripts/steady.jsonl"
	require.NoError(d.UpsertSession(Session{
		ID: "cursor:steady", Agent: "cursor", FilePath: &path, Cwd: "/work/a",
	}))

	_, err := d.getWriter().Exec("UPDATE sessions SET local_modified_at = ? WHERE id = ?", "2000-01-01T00:00:00.000Z", "cursor:steady")
	require.NoError(err)
	var before sql.NullString
	require.NoError(d.getReader().QueryRow(
		"SELECT local_modified_at FROM sessions WHERE id = ?",
		"cursor:steady",
	).Scan(&before))
	require.NoError(d.UpdateCwdByAgentPath(path, "cursor", "/work/a"))

	var after sql.NullString
	require.NoError(d.getReader().QueryRow(
		"SELECT local_modified_at FROM sessions WHERE id = ?",
		"cursor:steady",
	).Scan(&after))
	assert.Equal(t, before, after)
}

func TestUpdateSessionCwdByIdentityScopesSourceOwnership(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	d := testDB(t)
	path := "cursor-project/agent-transcripts/scoped.jsonl"
	otherPath := "other-project/agent-transcripts/scoped.jsonl"
	const id = "cursor:scoped"
	require.NoError(d.UpsertSession(Session{
		ID: id, Agent: "cursor", FilePath: &path, Cwd: "/work/old",
	}))
	require.NoError(d.SetSessionDataVersion(id, CurrentDataVersion()))

	updated, err := d.UpdateSessionCwdByIdentity(
		id, otherPath, "cursor", "/work/wrong-source",
	)
	require.NoError(err)
	assert.False(updated)
	stored, err := d.GetSession(t.Context(), id)
	require.NoError(err)
	require.NotNil(stored)
	assert.Equal("/work/old", stored.Cwd)

	updated, err = d.UpdateSessionCwdByIdentity(
		id, path, "cursor", "/work/new",
	)
	require.NoError(err)
	assert.True(updated)
	stored, err = d.GetSession(t.Context(), id)
	require.NoError(err)
	require.NotNil(stored)
	assert.Equal("/work/new", stored.Cwd)
	assert.Less(d.GetSessionDataVersion(id), CurrentDataVersion())
}

func TestUpdateSessionCwdDoesNotTouchUserTrashedRows(t *testing.T) {
	require := require.New(t)

	d := testDB(t)
	path := "cursor-project/agent-transcripts/trashed.jsonl"
	require.NoError(d.UpsertSession(Session{
		ID: "cursor:trashed", Agent: "cursor", FilePath: &path, Cwd: "/work/a",
	}))
	require.NoError(d.Update(func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(),
			"UPDATE sessions SET deleted_at = 'now', deletion_cause = 'user_deleted' WHERE id = ?",
			"cursor:trashed",
		)
		return err
	}))

	require.NoError(d.UpdateSessionCwd("cursor:trashed", "/work/b"))
	stored, err := d.GetSessionFull(t.Context(), "cursor:trashed")
	require.NoError(err)
	require.NotNil(stored)
	assert.Equal(t, "/work/a", stored.Cwd)
}

func TestStaleDataVersionAgentPathsMatchesPerPathForm(t *testing.T) {
	require := require.New(t)

	d := testDB(t)
	stalePath := "cursor-project/agent-transcripts/stale.jsonl"
	freshPath := "cursor-project/agent-transcripts/fresh.jsonl"
	missingPath := "cursor-project/agent-transcripts/missing.jsonl"
	require.NoError(d.UpsertSession(Session{
		ID: "cursor:stale", Agent: "cursor", FilePath: &stalePath,
	}))
	require.NoError(d.SetSessionDataVersion(
		"cursor:stale", CurrentDataVersion()-1,
	))
	require.NoError(d.UpsertSession(Session{
		ID: "cursor:fresh", Agent: "cursor", FilePath: &freshPath,
	}))
	require.NoError(d.SetSessionDataVersion(
		"cursor:fresh", CurrentDataVersion(),
	))
	require.NoError(d.UpsertSession(Session{
		ID: "cursor:missing", Agent: "cursor", FilePath: &missingPath,
	}))
	require.NoError(d.SetSessionDataVersion("cursor:missing", 0))
	require.NoError(d.Update(func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(),
			"UPDATE sessions SET source_missing_at = 'now' WHERE id = ?",
			"cursor:missing",
		)
		return err
	}))

	identities, err := d.StaleDataVersionAgentPaths(CurrentDataVersion())
	require.NoError(err)
	assert.Equal(t, []SessionSourcePath{
		{Agent: "cursor", FilePath: stalePath},
	}, identities)
}
