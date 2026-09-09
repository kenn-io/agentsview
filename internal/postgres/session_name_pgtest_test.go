//go:build pgtest

package postgres

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
)

// TestPGSessionNameVisibleInReadPaths verifies that a session with only a
// SessionName (agent-provided title, no user rename) surfaces its name through
// all PG read paths.
func TestPGSessionNameVisibleInReadPaths(t *testing.T) {
	pgURL := testPGURL(t)

	const schema = "agentsview_session_name_test"
	pg, err := Open(pgURL, schema, true)
	require.NoError(t, err, "Open")
	defer pg.Close()

	ctx := context.Background()
	_, err = pg.Exec(`DROP SCHEMA IF EXISTS ` + schema + ` CASCADE`)
	require.NoError(t, err, "drop schema")
	require.NoError(t, EnsureSchema(ctx, pg, schema), "EnsureSchema")

	// Local SQLite DB used by the Sync push path.
	localDB, err := db.Open(filepath.Join(t.TempDir(), "local.db"))
	require.NoError(t, err, "db.Open")
	defer localDB.Close()

	agentTitle := "Agent Title"
	sess := db.Session{
		ID:          "sn-001",
		Project:     "test-proj",
		Machine:     "test-machine",
		Agent:       "claude",
		SessionName: &agentTitle,
		// DisplayName is nil — no user rename.
		MessageCount:     5,
		UserMessageCount: 3,
		CreatedAt:        "2026-01-01T00:00:00Z",
		StartedAt:        strPtr("2026-01-01T00:00:00Z"),
		EndedAt:          strPtr("2026-01-01T01:00:00Z"),
	}
	require.NoError(t, localDB.UpsertSession(sess), "UpsertSession")

	sync := &Sync{
		pg:         pg,
		local:      localDB,
		machine:    "test-machine",
		schema:     schema,
		schemaDone: true,
	}
	_, pushErr := sync.Push(ctx, true, nil)
	require.NoError(t, pushErr, "Push")

	// Verify PG stored session_name correctly.
	var pgSessionName sql.NullString
	var pgDisplayName sql.NullString
	require.NoError(t, pg.QueryRowContext(ctx,
		`SELECT session_name, display_name FROM sessions WHERE id = $1`,
		sess.ID,
	).Scan(&pgSessionName, &pgDisplayName), "query raw PG row")
	assert.Equal(t, agentTitle, pgSessionName.String,
		"PG session_name should equal the pushed SessionName")
	assert.False(t, pgDisplayName.Valid,
		"PG display_name should be NULL (no user rename)")

	// Verify COALESCE resolves session_name when display_name is NULL.
	var coalesced string
	require.NoError(t, pg.QueryRowContext(ctx,
		`SELECT COALESCE(display_name, session_name) FROM sessions WHERE id = $1`,
		sess.ID,
	).Scan(&coalesced), "COALESCE query")
	assert.Equal(t, agentTitle, coalesced,
		"COALESCE(display_name, session_name) should return session_name")

	// Verify GetSidebarSessionIndex surfaces the agent title.
	store, err := NewStore(pgURL, schema, true)
	require.NoError(t, err, "NewStore")
	defer store.Close()

	idx, err := store.GetSidebarSessionIndex(ctx, db.SessionFilter{
		IncludeChildren: true,
	})
	require.NoError(t, err, "GetSidebarSessionIndex")
	require.Len(t, idx.Sessions, 1, "expected one session in sidebar index")
	row := idx.Sessions[0]
	assert.Equal(t, sess.ID, row.ID)
	assert.NotNil(t, row.DisplayName,
		"sidebar row DisplayName must not be nil for agent-named session")
	if row.DisplayName != nil {
		assert.Equal(t, agentTitle, *row.DisplayName,
			"sidebar row DisplayName should equal the agent title")
	}

	// Verify GetSession (full session via pgSessionCols) surfaces the agent title.
	full, err := store.GetSession(ctx, sess.ID)
	require.NoError(t, err, "GetSession")
	require.NotNil(t, full, "GetSession must return the session")
	assert.NotNil(t, full.DisplayName,
		"full session DisplayName must not be nil for agent-named session")
	if full.DisplayName != nil {
		assert.Equal(t, agentTitle, *full.DisplayName,
			"full session DisplayName should equal the agent title")
	}
}

// strPtr is a helper to take the address of a string literal.
func strPtr(s string) *string { return &s }

func TestPGPushUsageOnlyClearsRenamedTitle(t *testing.T) {
	pgURL := testPGURL(t)
	cleanPGSchema(t, pgURL)
	t.Cleanup(func() { cleanPGSchema(t, pgURL) })
	local := testDB(t)
	ps, err := New(pgURL, "agentsview", local, "test-machine", true, SyncOptions{})
	require.NoError(t, err)
	t.Cleanup(func() { _ = ps.Close() })
	ctx := context.Background()
	session := db.Session{
		ID: "title-policy", Agent: "claude", Project: "proj", Machine: "test-machine",
		FirstMessage: strPtr("original prompt"), DisplayName: strPtr("source title"),
		SessionName: strPtr("provider title"),
	}
	require.NoError(t, local.UpsertSession(session))
	require.NoError(t, local.RenameSession(session.ID, strPtr("source title")))
	_, err = ps.Push(ctx, true, nil)
	require.NoError(t, err)
	_, err = ps.pg.ExecContext(ctx, `UPDATE sessions SET display_name = 'remote title' WHERE id = $1`, session.ID)
	require.NoError(t, err)
	// Full-content pushes preserve an independent PostgreSQL rename.
	_, err = ps.Push(ctx, true, nil)
	require.NoError(t, err)
	var display, source, provider sql.NullString
	require.NoError(t, ps.pg.QueryRowContext(ctx,
		`SELECT display_name, source_display_name, session_name FROM sessions WHERE id = $1`, session.ID,
	).Scan(&display, &source, &provider))
	assert.Equal(t, "remote title", display.String)
	assert.Equal(t, "source title", source.String)
	assert.Equal(t, "provider title", provider.String)

	local.SetArchiveContent(config.ArchiveContentUsage)
	require.NoError(t, local.UpsertSession(session))
	_, err = ps.Push(ctx, true, nil)
	require.NoError(t, err)
	require.NoError(t, ps.pg.QueryRowContext(ctx,
		`SELECT display_name, source_display_name, session_name FROM sessions WHERE id = $1`, session.ID,
	).Scan(&display, &source, &provider))
	assert.False(t, display.Valid, "usage-only push must remove the remote rename")
	assert.False(t, source.Valid)
	assert.False(t, provider.Valid)
}
