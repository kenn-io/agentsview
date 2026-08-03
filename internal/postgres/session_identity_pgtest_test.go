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

func TestPGClaudeProvenanceVisibleInReadPaths(t *testing.T) {
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

	filePath := "/fixtures/sessions/sid-001.jsonl"
	sess := db.Session{
		ID:               "sid-001",
		Project:          "proj",
		Machine:          "test-machine",
		Agent:            "claude",
		AgentLabel:       "triage",
		Entrypoint:       "sdk-cli",
		SessionKind:      "bg",
		MessageCount:     1,
		UserMessageCount: 1,
		CreatedAt:        "2026-01-01T00:00:00Z",
		StartedAt:        strPtr("2026-01-01T00:00:00Z"),
		EndedAt:          strPtr("2026-01-01T01:00:00Z"),
		FilePath:         &filePath,
	}
	require.NoError(t, localDB.UpsertSession(sess), "UpsertSession")
	require.NoError(t, localDB.InsertMessages([]db.Message{{
		SessionID:     sess.ID,
		Ordinal:       0,
		Role:          "user",
		Content:       "hello",
		ContentLength: 5,
		PromptSource:  "typed",
	}}), "InsertMessages")

	sync := &Sync{
		pg:         pg,
		local:      localDB,
		machine:    "test-machine",
		schema:     schema,
		schemaDone: true,
	}
	_, pushErr := sync.Push(ctx, true, nil)
	require.NoError(t, pushErr, "Push")

	var agentLabel, entrypoint, sessionKind sql.NullString
	require.NoError(t, pg.QueryRowContext(ctx,
		`SELECT agent_label, entrypoint, session_kind
		 FROM sessions WHERE id = $1`,
		sess.ID,
	).Scan(&agentLabel, &entrypoint, &sessionKind), "query raw PG row")
	assert.Equal(t, "triage", agentLabel.String)
	assert.Equal(t, "sdk-cli", entrypoint.String)
	assert.Equal(t, "bg", sessionKind.String)

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
	assert.Equal(t, "bg", idx.Sessions[0].SessionKind)

	page, err := store.ListSessions(ctx, db.SessionFilter{
		IncludeChildren: true,
	})
	require.NoError(t, err, "ListSessions")
	require.Len(t, page.Sessions, 1, "expected one listed session")
	assert.Nil(t, page.Sessions[0].FilePath)

	visible, err := store.GetSession(ctx, sess.ID)
	require.NoError(t, err, "GetSession")
	require.NotNil(t, visible, "GetSession must return the session")
	assert.Equal(t, "triage", visible.AgentLabel)
	assert.Equal(t, "sdk-cli", visible.Entrypoint)
	assert.Nil(t, visible.FilePath)
	assert.Equal(t, "bg", visible.SessionKind)

	full, err := store.GetSessionFull(ctx, sess.ID)
	require.NoError(t, err, "GetSessionFull")
	require.NotNil(t, full, "GetSessionFull must return the session")
	require.NotNil(t, full.FilePath)
	assert.Equal(t, filePath, *full.FilePath)

	messages, err := store.GetMessages(ctx, sess.ID, 0, 10, true)
	require.NoError(t, err, "GetMessages")
	require.Len(t, messages, 1)
	assert.Equal(t, "typed", messages[0].PromptSource)
}
