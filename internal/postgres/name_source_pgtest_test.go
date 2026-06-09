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
	// name_source is a backend-only write guard; the read path intentionally
	// does not return it to callers, so NameSource is always nil here.
	assert.Nil(t, found.NameSource, "NameSource is not exposed via read path")
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

// TestPushNameSourceViaPushPath verifies name_source survives the REAL push
// path (Push -> ListSessionsModifiedBetween read), not just a direct
// pushSession call. ListSessionsModifiedBetween reads sessionFullCols, so a
// missing name_source there silently dropped the value on every real push
// despite the pushSession-level round-trip passing.
func TestPushNameSourceViaPushPath(t *testing.T) {
	pgURL := testPGURL(t)
	cleanPGSchema(t, pgURL)
	t.Cleanup(func() { cleanPGSchema(t, pgURL) })

	local := testDB(t)
	ps, err := New(
		pgURL, "agentsview", local, "machine-namesource-push", true,
		SyncOptions{},
	)
	require.NoError(t, err, "creating sync")
	defer ps.Close()

	ctx := context.Background()
	require.NoError(t, ps.EnsureSchema(ctx), "ensure schema")

	started := time.Now().UTC().Format(time.RFC3339)
	firstMsg := "real push path"
	displayName := "plan-2b-review"
	nameSource := "agent"
	require.NoError(t, local.UpsertSession(db.Session{
		ID:           "ns-push-001",
		Project:      "p",
		Machine:      "local",
		Agent:        "claude",
		FirstMessage: &firstMsg,
		DisplayName:  &displayName,
		NameSource:   &nameSource,
		StartedAt:    &started,
		MessageCount: 1,
	}), "upsert session")

	pushResult, err := ps.Push(ctx, false, nil)
	require.NoError(t, err, "push")
	require.Equal(t, 1, pushResult.SessionsPushed)

	store, err := NewStore(pgURL, "agentsview", true)
	require.NoError(t, err, "opening store")
	defer store.Close()

	// Verify name_source was pushed correctly via direct SQL —
	// the store read path intentionally omits it (backend-only field).
	var pushedNameSource *string
	require.NoError(t, ps.DB().QueryRow(
		`SELECT name_source FROM agentsview.sessions WHERE id = $1`,
		"ns-push-001",
	).Scan(&pushedNameSource), "read name_source via direct SQL")
	require.NotNil(t, pushedNameSource, "name_source must be stored in PG after push")
	assert.Equal(t, "agent", *pushedNameSource)

	index, err := store.GetSidebarSessionIndex(ctx, db.SessionFilter{Limit: 50})
	require.NoError(t, err, "GetSidebarSessionIndex")
	require.Len(t, index.Sessions, 1)
	// name_source is not exposed via read path; DisplayName still round-trips.
	assert.Nil(t, index.Sessions[0].NameSource,
		"name_source is not exposed via read path")
	require.NotNil(t, index.Sessions[0].DisplayName)
	assert.Equal(t, "plan-2b-review", *index.Sessions[0].DisplayName)
}
