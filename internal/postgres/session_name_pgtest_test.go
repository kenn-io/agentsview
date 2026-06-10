//go:build pgtest

package postgres

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

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
