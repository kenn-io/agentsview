//go:build pgtest

package postgres

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
)

// TestPushNameSourceRoundTrip verifies that name_source is pushed from
// SQLite to PostgreSQL and survives a round-trip via the sidebar index
// read path and the scanPGSession read path.
func TestPushNameSourceRoundTrip(t *testing.T) {
	pgURL := testPGURL(t)

	const schema = "agentsview_push_namesource_test"
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

	nameSource := "agent"
	displayName := "My renamed session"
	sess := db.Session{
		ID:               "namesource-test-001",
		Project:          "test-project",
		Machine:          "test-machine",
		Agent:            "claude",
		MessageCount:     1,
		UserMessageCount: 1,
		CreatedAt:        "2026-01-01T00:00:00Z",
		DisplayName:      &displayName,
		NameSource:       &nameSource,
	}

	// Push via pushSession directly (mirrors TestPushSessionTerminationStatus).
	tx, err := pg.BeginTx(ctx, nil)
	require.NoError(t, err, "BeginTx")
	if err := sync.pushSession(ctx, tx, sess); err != nil {
		_ = tx.Rollback()
		t.Fatalf("pushSession: %v", err)
	}
	require.NoError(t, tx.Commit(), "Commit")

	// Read back via direct query (scanPGSession path).
	var gotNameSource *string
	require.NoError(t, pg.QueryRow(
		`SELECT name_source FROM sessions WHERE id = $1`,
		sess.ID,
	).Scan(&gotNameSource), "read back name_source")
	require.NotNil(t, gotNameSource, "name_source should not be NULL")
	assert.Equal(t, "agent", *gotNameSource, "name_source round-trip")

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
	require.NotNil(t, found.NameSource, "NameSource should not be nil in sidebar index")
	assert.Equal(t, "agent", *found.NameSource, "NameSource round-trip via sidebar index")
	require.NotNil(t, found.DisplayName, "DisplayName should not be nil in sidebar index")
	assert.Equal(t, "My renamed session", *found.DisplayName, "DisplayName round-trip via sidebar index")

	// Verify that updating to NULL clears it via ON CONFLICT path.
	sess.NameSource = nil
	sess.DisplayName = nil

	tx2, err := pg.BeginTx(ctx, nil)
	require.NoError(t, err, "BeginTx (second)")
	if err := sync.pushSession(ctx, tx2, sess); err != nil {
		_ = tx2.Rollback()
		t.Fatalf("pushSession (second): %v", err)
	}
	require.NoError(t, tx2.Commit(), "Commit (second)")

	require.NoError(t, pg.QueryRow(
		`SELECT name_source FROM sessions WHERE id = $1`,
		sess.ID,
	).Scan(&gotNameSource), "read back name_source after clear")
	assert.Nil(t, gotNameSource, "name_source should be NULL after clearing")
}
