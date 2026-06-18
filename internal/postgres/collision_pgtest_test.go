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
	tx, err := pg.BeginTx(ctx, nil)
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

	tx, err := pg.BeginTx(ctx, nil)
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

	tx, err := pg.BeginTx(ctx, nil)
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
