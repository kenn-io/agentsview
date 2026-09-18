package db

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGetCwdByAgentPathScopesIdentityAndPreservesEmptyRows(t *testing.T) {
	d := testDB(t)
	path := "cursor-project/agent-transcripts/session.jsonl"
	emptyPath := "empty.jsonl"
	require.NoError(t, d.UpsertSession(t.Context(), Session{
		ID: "cursor:positive", Agent: "cursor", FilePath: &path, Cwd: "/work/a",
	}))
	require.NoError(t, d.UpsertSession(t.Context(), Session{
		ID: "codex:same-path", Agent: "codex", FilePath: &path, Cwd: "/work/b",
	}))
	require.NoError(t, d.UpsertSession(t.Context(), Session{
		ID: "cursor:empty", Agent: "cursor", FilePath: &emptyPath, Cwd: "",
	}))

	cwd, ok := d.GetCwdByAgentPath(t.Context(), path, "cursor")
	assert.True(t, ok)
	assert.Equal(t, "/work/a", cwd)

	cwd, ok = d.GetCwdByAgentPath(t.Context(), path, "codex")
	assert.True(t, ok)
	assert.Equal(t, "/work/b", cwd)

	cwd, ok = d.GetCwdByAgentPath(t.Context(), "empty.jsonl", "cursor")
	assert.True(t, ok)
	assert.Empty(t, cwd)

	cwd, ok = d.GetCwdByAgentPath(t.Context(), "missing.jsonl", "cursor")
	assert.False(t, ok)
	assert.Empty(t, cwd)

	require.NoError(t, d.UpdateSessionCwd(t.Context(), "cursor:positive", ""))
	cwd, ok = d.GetCwdByAgentPath(t.Context(), path, "cursor")
	assert.True(t, ok)
	assert.Empty(t, cwd)
	require.NoError(t, d.UpdateCwdByAgentPath(t.Context(), path, "cursor", "/work/c"))
	cwd, ok = d.GetCwdByAgentPath(t.Context(), path, "cursor")
	assert.True(t, ok)
	assert.Equal(t, "/work/c", cwd)
}

func TestGetCwdByAgentPathUsesSourceMissingPreservationRow(t *testing.T) {
	d := testDB(t)
	path := "cursor-project/agent-transcripts/revive.jsonl"
	require.NoError(t, d.UpsertSession(t.Context(), Session{
		ID: "cursor:revive", Agent: "cursor", FilePath: &path, Cwd: "/work/revive",
	}))
	require.NoError(t, d.Update(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(),
			"UPDATE sessions SET source_missing_at = 'now' WHERE id = ?",
			"cursor:revive",
		)
		return err
	}))

	cwd, ok := d.GetCwdByAgentPath(t.Context(), path, "cursor")
	assert.True(t, ok)
	assert.Equal(t, "/work/revive", cwd)
}

func TestUpdateCwdByAgentPathDoesNotTouchUnchangedRows(t *testing.T) {
	d := testDB(t)
	path := "cursor-project/agent-transcripts/steady.jsonl"
	require.NoError(t, d.UpsertSession(t.Context(), Session{
		ID: "cursor:steady", Agent: "cursor", FilePath: &path, Cwd: "/work/a",
	}))

	_, err := d.getWriter().Exec(t.Context(), "UPDATE sessions SET local_modified_at = ? WHERE id = ?", "2000-01-01T00:00:00.000Z", "cursor:steady")
	require.NoError(t, err)
	var before sql.NullString
	require.NoError(t, d.getReader().QueryRow(t.Context(),
		"SELECT local_modified_at FROM sessions WHERE id = ?",
		"cursor:steady",
	).Scan(&before))
	require.NoError(t, d.UpdateCwdByAgentPath(t.Context(), path, "cursor", "/work/a"))

	var after sql.NullString
	require.NoError(t, d.getReader().QueryRow(t.Context(),
		"SELECT local_modified_at FROM sessions WHERE id = ?",
		"cursor:steady",
	).Scan(&after))
	assert.Equal(t, before, after)
}

func TestUpdateSessionCwdByIdentityScopesSourceOwnership(t *testing.T) {
	d := testDB(t)
	path := "cursor-project/agent-transcripts/scoped.jsonl"
	otherPath := "other-project/agent-transcripts/scoped.jsonl"
	const id = "cursor:scoped"
	require.NoError(t, d.UpsertSession(t.Context(), Session{
		ID: id, Agent: "cursor", FilePath: &path, Cwd: "/work/old",
	}))
	require.NoError(t, d.SetSessionDataVersion(t.Context(), id, CurrentDataVersion()))

	updated, err := d.UpdateSessionCwdByIdentity(t.Context(),
		id, otherPath, "cursor", "/work/wrong-source",
	)
	require.NoError(t, err)
	assert.False(t, updated)
	stored, err := d.GetSession(t.Context(), id)
	require.NoError(t, err)
	require.NotNil(t, stored)
	assert.Equal(t, "/work/old", stored.Cwd)

	updated, err = d.UpdateSessionCwdByIdentity(t.Context(),
		id, path, "cursor", "/work/new",
	)
	require.NoError(t, err)
	assert.True(t, updated)
	stored, err = d.GetSession(t.Context(), id)
	require.NoError(t, err)
	require.NotNil(t, stored)
	assert.Equal(t, "/work/new", stored.Cwd)
	assert.Less(t, d.GetSessionDataVersion(t.Context(), id), CurrentDataVersion())
}

func TestUpdateSessionCwdDoesNotTouchUserTrashedRows(t *testing.T) {
	d := testDB(t)
	path := "cursor-project/agent-transcripts/trashed.jsonl"
	require.NoError(t, d.UpsertSession(t.Context(), Session{
		ID: "cursor:trashed", Agent: "cursor", FilePath: &path, Cwd: "/work/a",
	}))
	require.NoError(t, d.Update(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(),
			"UPDATE sessions SET deleted_at = 'now', deletion_cause = 'user_deleted' WHERE id = ?",
			"cursor:trashed",
		)
		return err
	}))

	require.NoError(t, d.UpdateSessionCwd(t.Context(), "cursor:trashed", "/work/b"))
	stored, err := d.GetSessionFull(t.Context(), "cursor:trashed")
	require.NoError(t, err)
	require.NotNil(t, stored)
	assert.Equal(t, "/work/a", stored.Cwd)
}

func TestStaleDataVersionAgentPathsMatchesPerPathForm(t *testing.T) {
	d := testDB(t)
	stalePath := "cursor-project/agent-transcripts/stale.jsonl"
	freshPath := "cursor-project/agent-transcripts/fresh.jsonl"
	missingPath := "cursor-project/agent-transcripts/missing.jsonl"
	require.NoError(t, d.UpsertSession(t.Context(), Session{
		ID: "cursor:stale", Agent: "cursor", FilePath: &stalePath,
	}))
	require.NoError(t, d.SetSessionDataVersion(t.Context(),
		"cursor:stale", CurrentDataVersion()-1,
	))
	require.NoError(t, d.UpsertSession(t.Context(), Session{
		ID: "cursor:fresh", Agent: "cursor", FilePath: &freshPath,
	}))
	require.NoError(t, d.SetSessionDataVersion(t.Context(),
		"cursor:fresh", CurrentDataVersion(),
	))
	require.NoError(t, d.UpsertSession(t.Context(), Session{
		ID: "cursor:missing", Agent: "cursor", FilePath: &missingPath,
	}))
	require.NoError(t, d.SetSessionDataVersion(t.Context(), "cursor:missing", 0))
	require.NoError(t, d.Update(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(),
			"UPDATE sessions SET source_missing_at = 'now' WHERE id = ?",
			"cursor:missing",
		)
		return err
	}))

	identities, err := d.StaleDataVersionAgentPaths(t.Context(), CurrentDataVersion())
	require.NoError(t, err)
	assert.Equal(t, []SessionSourcePath{
		{Agent: "cursor", FilePath: stalePath},
	}, identities)
}
