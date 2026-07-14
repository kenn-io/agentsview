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

func TestPGSessionIdentityVisibleInReadPaths(t *testing.T) {
	pgURL := testPGURL(t)

	const schema = "agentsview_session_identity_test"
	pg, err := Open(pgURL, schema, true)
	require.NoError(t, err, "Open")
	defer pg.Close()

	ctx := context.Background()
	_, err = pg.Exec(`DROP SCHEMA IF EXISTS ` + schema + ` CASCADE`)
	require.NoError(t, err, "drop schema")
	require.NoError(t, EnsureSchema(ctx, pg, schema), "EnsureSchema")

	localDB, err := db.Open(filepath.Join(t.TempDir(), "local.db"))
	require.NoError(t, err, "db.Open")
	defer localDB.Close()

	sess := db.Session{
		ID:               "sid-001",
		Project:          "proj",
		Machine:          "test-machine",
		Agent:            "claude",
		AgentLabel:       "triage",
		Entrypoint:       "sdk-cli",
		MessageCount:     1,
		UserMessageCount: 1,
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

	var agentLabel sql.NullString
	var entrypoint sql.NullString
	require.NoError(t, pg.QueryRowContext(ctx,
		`SELECT agent_label, entrypoint FROM sessions WHERE id = $1`,
		sess.ID,
	).Scan(&agentLabel, &entrypoint), "query raw PG row")
	assert.Equal(t, "triage", agentLabel.String)
	assert.Equal(t, "sdk-cli", entrypoint.String)

	store, err := NewStore(pgURL, schema, true)
	require.NoError(t, err, "NewStore")
	defer store.Close()

	idx, err := store.GetSidebarSessionIndex(ctx, db.SessionFilter{
		IncludeChildren: true,
	})
	require.NoError(t, err, "GetSidebarSessionIndex")
	require.Len(t, idx.Sessions, 1, "expected one session in sidebar index")
	assert.Equal(t, "triage", idx.Sessions[0].AgentLabel)
	assert.Equal(t, "sdk-cli", idx.Sessions[0].Entrypoint)

	full, err := store.GetSession(ctx, sess.ID)
	require.NoError(t, err, "GetSession")
	require.NotNil(t, full, "GetSession must return the session")
	assert.Equal(t, "triage", full.AgentLabel)
	assert.Equal(t, "sdk-cli", full.Entrypoint)
}
