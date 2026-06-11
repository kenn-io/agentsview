//go:build pgtest

package postgres

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
)

// TestPushSessionNameRoundTrip verifies that session_name is pushed from
// SQLite to PostgreSQL and survives a round-trip via the sidebar index
// read path and the scanPGSession read path.
func TestPushSessionNameRoundTrip(t *testing.T) {
	pgURL := testPGURL(t)

	const schema = "agentsview_push_sessionname_test"
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

	sync := &Sync{
		pg:         pg,
		local:      localDB,
		machine:    "test-machine",
		schema:     schema,
		schemaDone: true,
	}

	sessionName := "Agent Title"
	displayName := "My renamed session"
	sess := db.Session{
		ID:               "sessionname-test-001",
		Project:          "test-project",
		Machine:          "test-machine",
		Agent:            "claude",
		MessageCount:     1,
		UserMessageCount: 1,
		CreatedAt:        "2026-01-01T00:00:00Z",
		DisplayName:      &displayName,
		SessionName:      &sessionName,
	}

	// Push via pushSession directly.
	tx, err := pg.BeginTx(ctx, nil)
	require.NoError(t, err, "BeginTx")
	if err := sync.pushSession(ctx, tx, sess); err != nil {
		_ = tx.Rollback()
		t.Fatalf("pushSession: %v", err)
	}
	require.NoError(t, tx.Commit(), "Commit")

	// Read back session_name via direct query.
	var gotSessionName *string
	require.NoError(t, pg.QueryRow(
		`SELECT session_name FROM sessions WHERE id = $1`,
		sess.ID,
	).Scan(&gotSessionName), "read back session_name")
	require.NotNil(t, gotSessionName, "session_name should not be NULL")
	assert.Equal(t, "Agent Title", *gotSessionName, "session_name round-trip")

	// Read back via store and GetSidebarSessionIndex.
	store, err := NewStore(pgURL, schema, true)
	require.NoError(t, err, "NewStore")
	defer store.Close()

	index, err := store.GetSidebarSessionIndex(ctx, db.SessionFilter{
		Limit: 50,
	})
	require.NoError(t, err, "GetSidebarSessionIndex")

	var found *db.SidebarSessionIndexRow
	for i := range index.Sessions {
		if index.Sessions[i].ID == sess.ID {
			found = &index.Sessions[i]
			break
		}
	}
	require.NotNil(t, found, "session not found in sidebar index")
	// display_name wins over session_name when set.
	require.NotNil(t, found.DisplayName, "DisplayName should not be nil in sidebar index")
	assert.Equal(t, "My renamed session", *found.DisplayName, "DisplayName round-trip via sidebar index")

	// Verify that updating to NULL clears both via ON CONFLICT path.
	sess.SessionName = nil
	sess.DisplayName = nil

	tx2, err := pg.BeginTx(ctx, nil)
	require.NoError(t, err, "BeginTx (second)")
	if err := sync.pushSession(ctx, tx2, sess); err != nil {
		_ = tx2.Rollback()
		t.Fatalf("pushSession (second): %v", err)
	}
	require.NoError(t, tx2.Commit(), "Commit (second)")

	require.NoError(t, pg.QueryRow(
		`SELECT session_name FROM sessions WHERE id = $1`,
		sess.ID,
	).Scan(&gotSessionName), "read back session_name after clear")
	assert.Nil(t, gotSessionName, "session_name should be NULL after clearing")
}

// TestPushSessionNameViaPushPath verifies session_name survives the REAL push
// path (Push -> ListSessionsModifiedBetween read), not just a direct
// pushSession call. ListSessionsModifiedBetween reads sessionFullCols, so a
// missing session_name there would silently drop the value on every real push.
func TestPushSessionNameViaPushPath(t *testing.T) {
	pgURL := testPGURL(t)
	cleanPGSchema(t, pgURL)
	t.Cleanup(func() { cleanPGSchema(t, pgURL) })

	local := testDB(t)
	ps, err := New(
		pgURL, "agentsview", local, "machine-sessionname-push", true,
		SyncOptions{},
	)
	require.NoError(t, err, "creating sync")
	defer ps.Close()

	ctx := context.Background()
	require.NoError(t, ps.EnsureSchema(ctx), "ensure schema")

	started := time.Now().UTC().Format(time.RFC3339)
	firstMsg := "real push path"
	sessionName := "plan-2b-review"
	require.NoError(t, local.UpsertSession(db.Session{
		ID:           "sn-push-001",
		Project:      "p",
		Machine:      "local",
		Agent:        "claude",
		FirstMessage: &firstMsg,
		SessionName:  &sessionName,
		StartedAt:    &started,
		MessageCount: 1,
	}), "upsert session")

	pushResult, err := ps.Push(ctx, false, nil)
	require.NoError(t, err, "push")
	require.Equal(t, 1, pushResult.SessionsPushed)

	store, err := NewStore(pgURL, "agentsview", true)
	require.NoError(t, err, "opening store")
	defer store.Close()

	// Verify session_name was pushed correctly via direct SQL.
	var pushedSessionName *string
	require.NoError(t, ps.DB().QueryRow(
		`SELECT session_name FROM agentsview.sessions WHERE id = $1`,
		"sn-push-001",
	).Scan(&pushedSessionName), "read session_name via direct SQL")
	require.NotNil(t, pushedSessionName, "session_name must be stored in PG after push")
	assert.Equal(t, "plan-2b-review", *pushedSessionName)

	index, err := store.GetSidebarSessionIndex(ctx, db.SessionFilter{Limit: 50})
	require.NoError(t, err, "GetSidebarSessionIndex")
	require.Len(t, index.Sessions, 1)
	// session_name is visible via COALESCE(display_name, session_name) when no user rename.
	require.NotNil(t, index.Sessions[0].DisplayName)
	assert.Equal(t, "plan-2b-review", *index.Sessions[0].DisplayName)
}
