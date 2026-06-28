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
	err = sync.pushSession(ctx, tx, sess, pushedSessionIdentity{
		ID:      sess.ID,
		Machine: sess.Machine,
	}, markerID, nil)
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
	require.NoError(t, sync.pushSession(ctx, tx, sess, pushedSessionIdentity{
		ID:      sess.ID,
		Machine: "renamed-host",
	}, markerID, nil), "pushSession")
	require.NoError(t, tx.Commit(), "Commit")

	var machine, ownerMarker string
	err = pg.QueryRowContext(ctx,
		`SELECT machine, owner_marker FROM sessions WHERE id = $1`, sessID,
	).Scan(&machine, &ownerMarker)
	require.NoError(t, err, "read back session")
	assert.Equal(t, "renamed-host", machine)
	assert.Equal(t, markerID, ownerMarker)
}

// TestPushResolvesRelationshipIDsToPrefixedTargets verifies that when a
// referenced session is pushed under a collision-avoidance prefix, the
// relationship ids pointing at it -- source_session_id, parent_session_id, and
// a tool-call subagent_session_id -- are rewritten to the prefixed id so child
// and subagent rows link to the right PG session instead of a foreign machine's
// row or a dangling id. A non-colliding parent keeps its bare id.
func TestPushResolvesRelationshipIDsToPrefixedTargets(t *testing.T) {
	pgURL := testPGURL(t)

	const schema = "agentsview_relationship_resolution_test"
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
		machine:    "machine-b",
		schema:     schema,
		schemaDone: true,
	}

	// A foreign machine already owns the bare "shared" id, so machine-b's
	// "shared" session must be pushed under the "machine-b~shared" prefix.
	_, err = pg.ExecContext(ctx, `
		INSERT INTO sessions (
			id, machine, owner_marker, project, agent, created_at
		) VALUES ($1, $2, $3, $4, $5, NOW())
	`, "shared", "machine-a", "foreign-owner", "test-proj", "claude")
	require.NoError(t, err, "insert foreign-owned shared session")

	sharedParent := "shared"
	plainParent := "plain"
	sessions := []db.Session{
		{ID: "shared", Project: "test-proj", Machine: "machine-b",
			Agent: "claude", MessageCount: 1, CreatedAt: "2026-01-01T00:00:00Z"},
		{ID: "plain", Project: "test-proj", Machine: "machine-b",
			Agent: "claude", MessageCount: 1, CreatedAt: "2026-01-01T00:00:00Z"},
		{ID: "child-shared", Project: "test-proj", Machine: "machine-b",
			Agent: "claude", MessageCount: 1, CreatedAt: "2026-01-01T00:00:00Z",
			SourceSessionID: "shared", ParentSessionID: &sharedParent},
		{ID: "child-plain", Project: "test-proj", Machine: "machine-b",
			Agent: "claude", MessageCount: 1, CreatedAt: "2026-01-01T00:00:00Z",
			ParentSessionID: &plainParent},
	}
	for _, s := range sessions {
		require.NoError(t, localDB.UpsertSession(s), "UpsertSession "+s.ID)
	}

	// child-shared references the colliding "shared" id from a tool call too.
	require.NoError(t, localDB.InsertMessages([]db.Message{{
		SessionID: "child-shared", Ordinal: 0, Role: "assistant",
		Content: "spawning", HasToolUse: true,
		ToolCalls: []db.ToolCall{{
			ToolName: "subagent", Category: "Task",
			SubagentSessionID: "shared",
		}},
	}}), "InsertMessages child-shared")
	for _, id := range []string{"shared", "plain", "child-plain"} {
		require.NoError(t, localDB.InsertMessages([]db.Message{{
			SessionID: id, Ordinal: 0, Role: "user",
			Content: "hi", ContentLength: 2,
		}}), "InsertMessages "+id)
	}

	_, err = sync.Push(ctx, false, nil)
	require.NoError(t, err, "Push")

	const prefixedShared = "machine-b~shared"
	var n int
	require.NoError(t, pg.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sessions WHERE id = $1 AND machine = $2`,
		prefixedShared, "machine-b").Scan(&n), "count prefixed shared")
	assert.Equal(t, 1, n, "machine-b's shared session stored under prefixed id")

	var source, parent string
	require.NoError(t, pg.QueryRowContext(ctx,
		`SELECT source_session_id, parent_session_id
		 FROM sessions WHERE id = $1`,
		"child-shared").Scan(&source, &parent), "read child-shared relations")
	assert.Equal(t, prefixedShared, source, "source_session_id resolved")
	assert.Equal(t, prefixedShared, parent, "parent_session_id resolved")

	var subagent string
	require.NoError(t, pg.QueryRowContext(ctx,
		`SELECT subagent_session_id FROM tool_calls WHERE session_id = $1`,
		"child-shared").Scan(&subagent), "read child-shared subagent link")
	assert.Equal(t, prefixedShared, subagent, "subagent_session_id resolved")

	var plainParentGot string
	require.NoError(t, pg.QueryRowContext(ctx,
		`SELECT parent_session_id FROM sessions WHERE id = $1`,
		"child-plain").Scan(&plainParentGot), "read child-plain parent")
	assert.Equal(t, "plain", plainParentGot, "non-colliding parent stays bare")
}

// TestPushRepairsStaleSubagentLinkOnIncrementalPush verifies that an
// incremental push repairs a PG tool-call subagent link left at its unprefixed
// local id by a push that predated relationship resolution. The local and PG
// tool-call fingerprints both hold the stale id, so the message fast path would
// otherwise skip the session and never rewrite the link.
func TestPushRepairsStaleSubagentLinkOnIncrementalPush(t *testing.T) {
	pgURL := testPGURL(t)

	const schema = "agentsview_stale_subagent_repair_test"
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
		machine:    "machine-b",
		schema:     schema,
		schemaDone: true,
	}

	// A foreign machine owns the bare "sub-1" id, so machine-b's subagent
	// session is pushed under the "machine-b~sub-1" prefix.
	_, err = pg.ExecContext(ctx, `
		INSERT INTO sessions (
			id, machine, owner_marker, project, agent, created_at
		) VALUES ($1, $2, $3, $4, $5, NOW())
	`, "sub-1", "machine-a", "foreign-owner", "proj", "claude")
	require.NoError(t, err, "insert foreign-owned subagent session")

	for _, s := range []db.Session{
		{ID: "sub-1", Project: "proj", Machine: "machine-b", Agent: "claude",
			MessageCount: 1, CreatedAt: "2026-01-01T00:00:00Z"},
		{ID: "parent-1", Project: "proj", Machine: "machine-b", Agent: "claude",
			MessageCount: 1, CreatedAt: "2026-01-01T00:00:00Z"},
	} {
		require.NoError(t, localDB.UpsertSession(s), "UpsertSession "+s.ID)
	}
	require.NoError(t, localDB.InsertMessages([]db.Message{{
		SessionID: "sub-1", Ordinal: 0, Role: "user", Content: "hi", ContentLength: 2,
	}}), "InsertMessages sub-1")
	require.NoError(t, localDB.InsertMessages([]db.Message{{
		SessionID: "parent-1", Ordinal: 0, Role: "assistant",
		Content: "spawning", HasToolUse: true,
		ToolCalls: []db.ToolCall{{
			ToolName: "subagent", Category: "Task", SubagentSessionID: "sub-1",
		}},
	}}), "InsertMessages parent-1")

	_, err = sync.Push(ctx, false, nil)
	require.NoError(t, err, "first Push")

	const prefixedSub = "machine-b~sub-1"
	subagent := func() string {
		var s string
		require.NoError(t, pg.QueryRowContext(ctx,
			`SELECT subagent_session_id FROM tool_calls WHERE session_id = $1`,
			"parent-1").Scan(&s), "read parent-1 subagent link")
		return s
	}
	require.Equal(t, prefixedSub, subagent(), "first push resolves the link")

	// Simulate a row written before relationship resolution: revert the PG
	// subagent link to the unprefixed local id while the local row is unchanged.
	_, err = pg.ExecContext(ctx,
		`UPDATE tool_calls SET subagent_session_id = $1 WHERE session_id = $2`,
		"sub-1", "parent-1")
	require.NoError(t, err, "stale-revert subagent link")
	require.Equal(t, "sub-1", subagent(), "precondition: link is stale")

	// Re-list the parent without touching its message content, so only the
	// stale subagent link distinguishes it from PG.
	require.NoError(t, localDB.BumpLocalModifiedAt("parent-1"),
		"mark parent-1 modified")

	res, err := sync.Push(ctx, false, nil)
	require.NoError(t, err, "second Push")
	assert.Zero(t, res.Errors, "second push should report no failures")
	assert.Equal(t, prefixedSub, subagent(),
		"incremental push repairs the stale subagent link")
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
	require.NoError(t, sync.pushSession(ctx, tx, sess, pushedSessionIdentity{
		ID:      sess.ID,
		Machine: "host-a",
	}, markerID, nil), "pushSession")
	require.NoError(t, tx.Commit(), "Commit")

	var machine, ownerMarker string
	err = pg.QueryRowContext(ctx,
		`SELECT machine, owner_marker FROM sessions WHERE id = $1`, sessID,
	).Scan(&machine, &ownerMarker)
	require.NoError(t, err, "read back session")
	assert.Equal(t, "host-a", machine)
	assert.Equal(t, markerID, ownerMarker)
}

// TestResolveOwnedPushIDReusesLegacyPrefixAfterRename verifies that the shared
// id resolver (used by both resolvePushedSessionIdentity and
// relationshipResolver.lookup) reuses a row owned under a prior machine prefix.
// A pusher that once stored a colliding session under "old-host~id" and later
// renamed to "new-host" must resolve back to "old-host~id" instead of minting a
// duplicate "new-host~id".
func TestResolveOwnedPushIDReusesLegacyPrefixAfterRename(t *testing.T) {
	pgURL := testPGURL(t)

	const schema = "agentsview_legacy_prefix_resolve_test"
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
		machine:    "new-host",
		schema:     schema,
		schemaDone: true,
	}
	markerID, err := sync.pushMarkerID()
	require.NoError(t, err, "pushMarkerID")

	// A foreign machine owns the bare id, which is why this pusher stored its
	// session under the old machine prefix before the rename.
	_, err = pg.ExecContext(ctx, `
		INSERT INTO sessions (id, machine, owner_marker, project, agent, created_at)
		VALUES ($1, $2, $3, $4, $5, NOW())`,
		"sess-x", "foreign", "foreign-owner", "proj", "claude")
	require.NoError(t, err, "insert foreign bare row")
	_, err = pg.ExecContext(ctx, `
		INSERT INTO sessions (id, machine, owner_marker, project, agent, created_at)
		VALUES ($1, $2, $3, $4, $5, NOW())`,
		"old-host~sess-x", "old-host", markerID, "proj", "claude")
	require.NoError(t, err, "insert owned legacy-prefixed row")

	got, err := sync.resolveOwnedPushID(
		ctx, "sess-x", "new-host", markerID, []string{"old-host"},
	)
	require.NoError(t, err)
	assert.Equal(t, "old-host~sess-x", got,
		"a renamed pusher must reuse the row it owns under the old machine prefix")

	// Without the old machine name, the resolver cannot find the owned row and
	// would mint a duplicate under the new prefix -- the blind spot being fixed.
	got, err = sync.resolveOwnedPushID(ctx, "sess-x", "new-host", markerID, nil)
	require.NoError(t, err)
	assert.Equal(t, "new-host~sess-x", got,
		"precondition: without the legacy machine the resolver duplicates the row")
}

// TestPushReusesLegacyPrefixedRowAfterRename verifies the end-to-end push:
// after a machine rename, a session previously stored under the old machine
// prefix is updated in place rather than duplicated under the new prefix.
func TestPushReusesLegacyPrefixedRowAfterRename(t *testing.T) {
	pgURL := testPGURL(t)

	const schema = "agentsview_legacy_prefix_push_test"
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
		machine:    "new-host",
		schema:     schema,
		schemaDone: true,
	}
	markerID, err := sync.pushMarkerID()
	require.NoError(t, err, "pushMarkerID")

	// Record that this marker last pushed as "old-host", so the rename push
	// treats "old-host" as a legacy machine prefix.
	_, err = pg.ExecContext(ctx, `
		INSERT INTO sync_metadata (key, value) VALUES ($1, $2)
		ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value`,
		pushMarkerKeyPrefix+markerID, "old-host")
	require.NoError(t, err, "seed push marker machine")

	// A foreign machine owns the bare id; this pusher's session lives under the
	// old machine prefix from before the rename.
	_, err = pg.ExecContext(ctx, `
		INSERT INTO sessions (id, machine, owner_marker, project, agent, created_at)
		VALUES ($1, $2, $3, $4, $5, NOW())`,
		"sess-x", "foreign", "foreign-owner", "proj", "claude")
	require.NoError(t, err, "insert foreign bare row")
	_, err = pg.ExecContext(ctx, `
		INSERT INTO sessions (id, machine, owner_marker, project, agent, created_at)
		VALUES ($1, $2, $3, $4, $5, NOW())`,
		"old-host~sess-x", "old-host", markerID, "proj", "claude")
	require.NoError(t, err, "insert owned legacy-prefixed row")

	sess := db.Session{
		ID: "sess-x", Project: "proj", Machine: "local", Agent: "claude",
		MessageCount: 1, CreatedAt: "2026-01-01T00:00:00Z",
	}
	require.NoError(t, localDB.UpsertSession(sess), "UpsertSession")
	require.NoError(t, localDB.InsertMessages([]db.Message{{
		SessionID: "sess-x", Ordinal: 0, Role: "user", Content: "hi", ContentLength: 2,
	}}), "InsertMessages")

	_, err = sync.Push(ctx, false, nil)
	require.NoError(t, err, "Push")

	var dup int
	require.NoError(t, pg.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sessions WHERE id = $1`, "new-host~sess-x").Scan(&dup),
		"count new-prefix duplicate")
	assert.Equal(t, 0, dup, "rename must not create a duplicate under the new prefix")

	// The legacy-prefixed row was updated in place: the machine column reflects
	// the rename and the pushed message landed there.
	var machine string
	require.NoError(t, pg.QueryRowContext(ctx,
		`SELECT machine FROM sessions WHERE id = $1`, "old-host~sess-x").Scan(&machine),
		"read reused row machine")
	assert.Equal(t, "new-host", machine, "owned legacy row updated in place")
	var msgs int
	require.NoError(t, pg.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM messages WHERE session_id = $1`, "old-host~sess-x").Scan(&msgs),
		"count reused row messages")
	assert.Equal(t, 1, msgs, "message pushed to the reused legacy row")

	var foreignOwner string
	require.NoError(t, pg.QueryRowContext(ctx,
		`SELECT owner_marker FROM sessions WHERE id = $1`, "sess-x").Scan(&foreignOwner),
		"read foreign bare row")
	assert.Equal(t, "foreign-owner", foreignOwner, "foreign bare row not adopted")
}

// TestPushSkipsPrefixedConflictWithoutAbortingBatch verifies that a single
// per-session ownership conflict on the current-machine prefixed id is skipped
// and reported, while unrelated sessions in the same push still go through. The
// conflict must not fail the whole push from identity pre-resolution.
func TestPushSkipsPrefixedConflictWithoutAbortingBatch(t *testing.T) {
	pgURL := testPGURL(t)

	const schema = "agentsview_prefixed_conflict_skip_test"
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
		machine:    "new-host",
		schema:     schema,
		schemaDone: true,
	}
	markerID, err := sync.pushMarkerID()
	require.NoError(t, err, "pushMarkerID")

	// A different owner holds both the bare id and the current-machine prefixed
	// id, so "conf-1" has nowhere to land and must be skipped as a conflict.
	for _, id := range []string{"conf-1", "new-host~conf-1"} {
		_, err = pg.ExecContext(ctx, `
			INSERT INTO sessions (id, machine, owner_marker, project, agent, created_at)
			VALUES ($1, $2, $3, $4, $5, NOW())`,
			id, "other-host", "other-owner", "proj", "claude")
		require.NoError(t, err, "insert foreign "+id)
	}

	for _, s := range []db.Session{
		{ID: "conf-1", Project: "proj", Machine: "new-host", Agent: "claude",
			MessageCount: 1, CreatedAt: "2026-01-01T00:00:00Z"},
		{ID: "clean-1", Project: "proj", Machine: "new-host", Agent: "claude",
			MessageCount: 1, CreatedAt: "2026-01-01T00:00:00Z"},
	} {
		require.NoError(t, localDB.UpsertSession(s), "UpsertSession "+s.ID)
		require.NoError(t, localDB.InsertMessages([]db.Message{{
			SessionID: s.ID, Ordinal: 0, Role: "user", Content: "hi", ContentLength: 2,
		}}), "InsertMessages "+s.ID)
	}

	res, err := sync.Push(ctx, false, nil)
	require.NoError(t, err, "a per-session conflict must not fail the whole push")
	assert.Equal(t, 1, res.SkippedConflicts, "the conflicting session is reported as skipped")

	var clean int
	require.NoError(t, pg.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sessions WHERE id = $1 AND owner_marker = $2`,
		"clean-1", markerID).Scan(&clean), "count clean session")
	assert.Equal(t, 1, clean, "unrelated session pushed despite the conflict")

	var owner string
	require.NoError(t, pg.QueryRowContext(ctx,
		`SELECT owner_marker FROM sessions WHERE id = $1`, "new-host~conf-1").Scan(&owner),
		"read foreign prefixed row")
	assert.Equal(t, "other-owner", owner, "foreign prefixed row not overwritten")
}

func TestPushSkipsRelationshipsToPrefixedOwnershipConflict(t *testing.T) {
	pgURL := testPGURL(t)

	const schema = "agentsview_prefixed_conflict_relationship_test"
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
		machine:    "new-host",
		schema:     schema,
		schemaDone: true,
	}

	for _, id := range []string{"conf-1", "new-host~conf-1"} {
		_, err = pg.ExecContext(ctx, `
			INSERT INTO sessions (id, machine, owner_marker, project, agent, created_at)
			VALUES ($1, $2, $3, $4, $5, NOW())`,
			id, "other-host", "other-owner", "proj", "claude")
		require.NoError(t, err, "insert foreign "+id)
	}

	sourceID := "conf-1"
	for _, s := range []db.Session{
		{ID: "conf-1", Project: "proj", Machine: "new-host", Agent: "claude",
			MessageCount: 1, CreatedAt: "2026-01-01T00:00:00Z"},
		{ID: "child-1", Project: "proj", Machine: "new-host", Agent: "claude",
			SourceSessionID: sourceID, MessageCount: 1, CreatedAt: "2026-01-01T00:00:00Z"},
	} {
		require.NoError(t, localDB.UpsertSession(s), "UpsertSession "+s.ID)
		require.NoError(t, localDB.InsertMessages([]db.Message{{
			SessionID: s.ID, Ordinal: 0, Role: "user", Content: "hi", ContentLength: 2,
		}}), "InsertMessages "+s.ID)
	}

	res, err := sync.Push(ctx, false, nil)
	require.NoError(t, err, "relationship to a per-session conflict must not fail the whole push")
	assert.Equal(t, 2, res.SkippedConflicts,
		"the conflicted session and the dependent relationship session are skipped")
	assert.Zero(t, res.Errors)

	var childRows int
	require.NoError(t, pg.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sessions WHERE id = $1`, "child-1").Scan(&childRows),
		"count child session")
	assert.Equal(t, 0, childRows, "dependent session must not be pushed with a foreign source link")

	var foreignRefs int
	require.NoError(t, pg.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sessions WHERE source_session_id = $1`,
		"new-host~conf-1").Scan(&foreignRefs), "count references to foreign prefixed row")
	assert.Equal(t, 0, foreignRefs, "no pushed row may point at the foreign prefixed session")
}

func TestPushSkipsRelationshipsToAlreadyPrefixedOwnershipConflict(t *testing.T) {
	pgURL := testPGURL(t)

	const schema = "agentsview_already_prefixed_conflict_relationship_test"
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
		machine:    "new-host",
		schema:     schema,
		schemaDone: true,
	}

	const conflictedID = "new-host~conf-1"
	_, err = pg.ExecContext(ctx, `
		INSERT INTO sessions (id, machine, owner_marker, project, agent, created_at)
		VALUES ($1, $2, $3, $4, $5, NOW())`,
		conflictedID, "other-host", "other-owner", "proj", "claude")
	require.NoError(t, err, "insert foreign already-prefixed row")

	for _, s := range []db.Session{
		{ID: conflictedID, Project: "proj", Machine: "new-host", Agent: "claude",
			MessageCount: 1, CreatedAt: "2026-01-01T00:00:00Z"},
		{ID: "child-1", Project: "proj", Machine: "new-host", Agent: "claude",
			SourceSessionID: conflictedID, MessageCount: 1, CreatedAt: "2026-01-01T00:00:00Z"},
	} {
		require.NoError(t, localDB.UpsertSession(s), "UpsertSession "+s.ID)
		require.NoError(t, localDB.InsertMessages([]db.Message{{
			SessionID: s.ID, Ordinal: 0, Role: "user", Content: "hi", ContentLength: 2,
		}}), "InsertMessages "+s.ID)
	}

	res, err := sync.Push(ctx, false, nil)
	require.NoError(t, err, "already-prefixed relationship conflict must not fail the whole push")
	assert.Equal(t, 2, res.SkippedConflicts,
		"the conflicted already-prefixed session and its dependent are skipped")
	assert.Zero(t, res.Errors)

	var childRows int
	require.NoError(t, pg.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sessions WHERE id = $1`, "child-1").Scan(&childRows),
		"count child session")
	assert.Equal(t, 0, childRows, "dependent session must not be pushed with a foreign source link")

	var foreignRefs int
	require.NoError(t, pg.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sessions WHERE source_session_id = $1`,
		conflictedID).Scan(&foreignRefs), "count references to foreign already-prefixed row")
	assert.Equal(t, 0, foreignRefs, "no pushed row may point at the foreign already-prefixed session")
}
