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

// TestPushSessionGuardsAgainstCrossMachineCollision verifies that when two
// machines share the same session ID (from dotfile sync, directory restore, etc.),
// the second machine's push is skipped if the session is already owned by a
// different machine. This prevents the ping-pong effect where two pushers
// fight over the same row on every push cycle.
//
// Steps:
//  1. Insert a session with id="clash-001" and machine="machine-a" directly into PG.
//  2. Call pushSession with machine="machine-b" and a db.Session{ID: "clash-001", Machine: "machine-b", ...}.
//  3. Assert that the row's machine column is still "machine-a" (not overwritten).
//  4. Assert that no messages were written for the conflicting session.
func TestPushSessionGuardsAgainstCrossMachineCollision(t *testing.T) {
	pgURL := testPGURL(t)

	const schema = "agentsview_collision_guard_test"
	pg, err := Open(pgURL, schema, true)
	require.NoError(t, err, "Open")
	defer pg.Close()

	ctx := context.Background()
	_, err = pg.Exec(`DROP SCHEMA IF EXISTS ` + schema + ` CASCADE`)
	require.NoError(t, err, "drop schema")
	require.NoError(t, EnsureSchema(ctx, pg, schema), "EnsureSchema")

	// Local SQLite DB.
	localDB, err := db.Open(filepath.Join(t.TempDir(), "local.db"))
	require.NoError(t, err, "db.Open")
	defer localDB.Close()

	sync := &Sync{
		pg:         pg,
		local:      localDB,
		machine:    "machine-b",
		schema:     schema,
		schemaDone: true,
	}

	const clashID = "clash-001"

	// Step 1: Insert a session owned by machine-a directly into PG.
	markerID, err := sync.pushMarkerID()
	require.NoError(t, err, "pushMarkerID")

	_, err = pg.ExecContext(ctx, `
		INSERT INTO sessions (
			id, machine, owner_marker, project, agent, created_at
		) VALUES ($1, $2, $3, $4, $5, NOW())
	`, clashID, "machine-a", "different-owner", "test-proj", "claude")
	require.NoError(t, err, "insert existing session")

	// Step 2: Attempt to push the same session from machine-b.
	sess := db.Session{
		ID:           clashID,
		Project:      "test-proj",
		Machine:      "machine-b",
		Agent:        "claude",
		MessageCount: 1,
		CreatedAt:    "2026-01-01T00:00:00Z",
	}
	require.NoError(t, localDB.UpsertSession(sess), "UpsertSession")
	require.NoError(t, localDB.InsertMessages([]db.Message{{
		SessionID:     clashID,
		Ordinal:       0,
		Role:          "user",
		Content:       "test",
		ContentLength: 4,
	}}), "InsertMessages")

	// Execute pushSession.
	tx, err := sync.bunDB().BeginTx(ctx, nil)
	require.NoError(t, err, "BeginTx")
	err = sync.pushSession(ctx, tx, sess, markerID, nil)
	require.ErrorIs(t, err, errSessionOwnershipConflict, "pushSession should return ownership conflict sentinel")
	require.NoError(t, tx.Commit(), "Commit")

	// Step 3: Verify the machine column is still "machine-a".
	var existingMachine string
	err = pg.QueryRowContext(ctx,
		`SELECT machine FROM sessions WHERE id = $1`, clashID,
	).Scan(&existingMachine)
	require.NoError(t, err, "read back machine")
	assert.Equal(t, "machine-a", existingMachine,
		"machine should remain as 'machine-a', not overwritten by 'machine-b'")

	// Step 4: Verify no messages were written for this session.
	var messageCount int
	err = pg.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM messages WHERE session_id = $1`, clashID,
	).Scan(&messageCount)
	require.NoError(t, err, "count messages")
	assert.Equal(t, 0, messageCount,
		"no messages should be written when session is skipped due to collision")

	assert.NotEqual(t, markerID, "different-owner", "precondition: foreign owner marker differs from local marker")
}

func TestPushSessionAllowsMachineRenameForSameOwnerMarker(t *testing.T) {
	pgURL := testPGURL(t)

	const schema = "agentsview_collision_owner_marker_test"
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
		machine:    "renamed-host",
		schema:     schema,
		schemaDone: true,
	}
	markerID, err := sync.pushMarkerID()
	require.NoError(t, err, "pushMarkerID")

	const sessID = "rename-001"
	_, err = pg.ExecContext(ctx, `
		INSERT INTO sessions (
			id, machine, owner_marker, project, agent, created_at
		) VALUES ($1, $2, $3, $4, $5, NOW())
	`, sessID, "old-host", markerID, "test-proj", "claude")
	require.NoError(t, err, "insert existing session")

	sess := db.Session{
		ID:           sessID,
		Project:      "test-proj",
		Machine:      "local",
		Agent:        "claude",
		MessageCount: 1,
		CreatedAt:    "2026-01-01T00:00:00Z",
	}
	require.NoError(t, localDB.UpsertSession(sess), "UpsertSession")

	tx, err := sync.bunDB().BeginTx(ctx, nil)
	require.NoError(t, err, "BeginTx")
	require.NoError(t, sync.pushSession(ctx, tx, sess, markerID, nil), "pushSession")
	require.NoError(t, tx.Commit(), "Commit")

	var machine, ownerMarker string
	err = pg.QueryRowContext(ctx,
		`SELECT machine, owner_marker FROM sessions WHERE id = $1`, sessID,
	).Scan(&machine, &ownerMarker)
	require.NoError(t, err, "read back session")
	assert.Equal(t, "renamed-host", machine)
	assert.Equal(t, markerID, ownerMarker)
}

func TestPushSessionAdoptsLegacyLocalSentinelRow(t *testing.T) {
	pgURL := testPGURL(t)

	const schema = "agentsview_collision_legacy_local_test"
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
		machine:    "host-a",
		schema:     schema,
		schemaDone: true,
	}

	const sessID = "legacy-local-001"
	_, err = pg.ExecContext(ctx, `
		INSERT INTO sessions (
			id, machine, owner_marker, project, agent, created_at
		) VALUES ($1, $2, $3, $4, $5, NOW())
	`, sessID, "local", "", "test-proj", "claude")
	require.NoError(t, err, "insert legacy local sentinel row")

	sess := db.Session{
		ID:           sessID,
		Project:      "test-proj",
		Machine:      "local",
		Agent:        "claude",
		MessageCount: 1,
		CreatedAt:    "2026-01-01T00:00:00Z",
	}
	require.NoError(t, localDB.UpsertSession(sess), "UpsertSession")

	tx, err := sync.bunDB().BeginTx(ctx, nil)
	require.NoError(t, err, "BeginTx")
	markerID, err := sync.pushMarkerID()
	require.NoError(t, err, "pushMarkerID")
	require.NoError(t, sync.pushSession(ctx, tx, sess, markerID, nil), "pushSession")
	require.NoError(t, tx.Commit(), "Commit")

	var machine, ownerMarker string
	err = pg.QueryRowContext(ctx,
		`SELECT machine, owner_marker FROM sessions WHERE id = $1`, sessID,
	).Scan(&machine, &ownerMarker)
	require.NoError(t, err, "read back session")
	assert.Equal(t, "host-a", machine)
	assert.Equal(t, markerID, ownerMarker)
}

func TestPushSessionSerializesConcurrentFirstOwner(t *testing.T) {
	pgURL := testPGURL(t)

	const schema = "agentsview_collision_concurrent_insert_test"
	pg, err := Open(pgURL, schema, true)
	require.NoError(t, err, "Open")
	defer pg.Close()
	ctx := t.Context()
	_, err = pg.Exec(`DROP SCHEMA IF EXISTS ` + schema + ` CASCADE`)
	require.NoError(t, err, "drop schema")
	require.NoError(t, EnsureSchema(ctx, pg, schema), "EnsureSchema")

	localA, err := db.Open(filepath.Join(t.TempDir(), "a.db"))
	require.NoError(t, err, "open local A")
	defer localA.Close()
	localB, err := db.Open(filepath.Join(t.TempDir(), "b.db"))
	require.NoError(t, err, "open local B")
	defer localB.Close()
	syncA := &Sync{
		pg: pg, local: localA, machine: "machine-a", schema: schema, schemaDone: true,
	}
	syncB := &Sync{
		pg: pg, local: localB, machine: "machine-b", schema: schema, schemaDone: true,
	}
	markerA, err := syncA.pushMarkerID()
	require.NoError(t, err, "marker A")
	markerB, err := syncB.pushMarkerID()
	require.NoError(t, err, "marker B")

	ownerLocked := make(chan struct{})
	releaseOwner := make(chan struct{})
	competitorAttempting := make(chan struct{})
	syncA.afterSessionOwnershipLock = func() {
		close(ownerLocked)
		<-releaseOwner
	}
	syncB.beforeSessionOwnershipLock = func() {
		close(competitorAttempting)
	}
	push := func(syncer *Sync, machine, marker string) <-chan error {
		result := make(chan error, 1)
		go func() {
			tx, err := syncer.bunDB().BeginTx(ctx, nil)
			if err == nil {
				err = syncer.pushSession(ctx, tx, db.Session{
					ID: "concurrent-owner", Project: "project", Machine: machine,
					Agent: "codex", CreatedAt: "2026-08-04T10:00:00Z",
				}, marker, nil)
			}
			if err == nil {
				err = tx.Commit()
			} else {
				_ = tx.Rollback()
			}
			result <- err
		}()
		return result
	}

	resultA := push(syncA, "machine-a", markerA)
	<-ownerLocked
	resultB := push(syncB, "machine-b", markerB)
	<-competitorAttempting
	close(releaseOwner)
	require.NoError(t, <-resultA)
	require.ErrorIs(t, <-resultB, errSessionOwnershipConflict)

	var machine, marker string
	require.NoError(t, pg.QueryRowContext(ctx,
		`SELECT machine, owner_marker FROM sessions WHERE id = $1`,
		"concurrent-owner",
	).Scan(&machine, &marker))
	assert.Equal(t, "machine-a", machine)
	assert.Equal(t, markerA, marker)
}

func TestSessionOwnershipLocksKeepDistinctFullDigestKeys(t *testing.T) {
	pgURL := testPGURL(t)

	const schema = "agentsview"
	pg, err := Open(pgURL, schema, true)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, pg.Close()) })
	ctx := t.Context()
	_, err = pg.Exec(`DROP SCHEMA IF EXISTS ` + schema + ` CASCADE`)
	require.NoError(t, err)
	require.NoError(t, EnsureSchema(ctx, pg, schema))
	syncer := &Sync{pg: pg, schema: schema, schemaDone: true}

	// These IDs have the same first two SHA-256 bytes for this schema. They
	// must nevertheless own independent persistent lock rows.
	for _, sessionID := range []string{
		"collision-session-366",
		"collision-session-504",
	} {
		tx, err := syncer.bunDB().BeginTx(ctx, nil)
		require.NoError(t, err)
		require.NoError(t, syncer.lockSessionOwnership(ctx, tx, sessionID))
		require.NoError(t, tx.Commit())
	}

	var lockKeys []string
	require.NoError(t, syncer.bunDB().NewSelect().
		Model((*pgSessionOwnershipLockRow)(nil)).
		Column("key").
		Where("key LIKE ?", "session_ownership_lock_v1:%").
		Order("key ASC").Scan(ctx, &lockKeys))
	assert.Len(t, lockKeys, 2)
}
