//go:build pgtest

package postgres

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/export"
)

func TestPushMirrorsSessionProjectIdentitySnapshotsByArchiveGeneration(
	t *testing.T,
) {
	pgURL := testPGURL(t)
	const schema = "agentsview_push_project_snapshot_test"
	pg, err := Open(pgURL, schema, true)
	require.NoError(t, err)
	defer pg.Close()
	ctx := context.Background()
	_, err = pg.Exec(`DROP SCHEMA IF EXISTS ` + schema + ` CASCADE`)
	require.NoError(t, err)
	require.NoError(t, EnsureSchema(ctx, pg, schema))

	local, err := db.Open(filepath.Join(t.TempDir(), "local.db"))
	require.NoError(t, err)
	defer local.Close()
	require.NoError(t, local.UpsertSession(db.Session{
		ID: "snapshot-session", Project: "app", Machine: "laptop", Agent: "codex",
	}))
	require.NoError(t, local.UpsertProjectIdentityObservation(ctx,
		export.ProjectIdentityObservation{
			SessionID: "snapshot-session", Project: "app", Machine: "laptop",
			RootPath: "/workspace/app", GitRemote: "https://github.com/acme/app.git",
			GitRemoteName: "origin", RepositoryPath: "/workspace/app/.git",
			WorktreeRootPath:     "/workspace/app",
			WorktreeRelationship: export.WorktreeMain,
			CheckoutState:        export.CheckoutDetached,
			RemoteResolution:     export.ProjectResolutionResolved,
			ObservedAt:           time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC),
		},
	))
	archiveID, err := local.GetArchiveID(ctx)
	require.NoError(t, err)
	generation, err := local.GetDatabaseID(ctx)
	require.NoError(t, err)

	syncer := &Sync{
		pg: pg, local: local, machine: "laptop", schema: schema, schemaDone: true,
	}
	require.NoError(t, syncer.syncProjectIdentityObservations(ctx, false))

	var gotArchive, gotGeneration, gotSession, gotRemote string
	var gotRelationship export.WorktreeRelationship
	var gotCheckout export.CheckoutState
	err = pg.QueryRowContext(ctx, `
		SELECT source_archive_id, source_database_generation,
			source_session_id, git_remote, worktree_relationship, checkout_state
		FROM source_session_project_identity_snapshots
		WHERE source_session_id = $1`, "snapshot-session",
	).Scan(
		&gotArchive, &gotGeneration, &gotSession, &gotRemote,
		&gotRelationship, &gotCheckout,
	)
	require.NoError(t, err)
	assert.Equal(t, archiveID, gotArchive)
	assert.Equal(t, generation, gotGeneration)
	assert.Equal(t, "snapshot-session", gotSession)
	assert.Equal(t, "https://github.com/acme/app.git", gotRemote)
	assert.Equal(t, export.WorktreeMain, gotRelationship)
	assert.Equal(t, export.CheckoutDetached, gotCheckout)

	_, err = pg.ExecContext(ctx, `
		UPDATE source_session_project_identity_snapshots
		SET git_remote = 'sentinel'
		WHERE source_session_id = $1`, "snapshot-session")
	require.NoError(t, err)
	require.NoError(t, syncer.syncProjectIdentityObservations(ctx, false))
	require.NoError(t, pg.QueryRowContext(ctx, `
		SELECT git_remote FROM source_session_project_identity_snapshots
		WHERE source_session_id = $1`, "snapshot-session").Scan(&gotRemote))
	assert.Equal(t, "sentinel", gotRemote,
		"unchanged local revision should skip PG publication")
	require.NoError(t, syncer.syncProjectIdentityObservations(ctx, true))
	require.NoError(t, pg.QueryRowContext(ctx, `
		SELECT git_remote FROM source_session_project_identity_snapshots
		WHERE source_session_id = $1`, "snapshot-session").Scan(&gotRemote))
	assert.Equal(t, "https://github.com/acme/app.git", gotRemote,
		"forced publication should rebuild PG identity rows")

	require.NoError(t, local.DeleteSession("snapshot-session"))
	require.NoError(t, syncer.syncProjectIdentityObservations(ctx, false))
	var snapshotCount int
	require.NoError(t, pg.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM source_session_project_identity_snapshots
		WHERE source_archive_id = $1`, archiveID,
	).Scan(&snapshotCount))
	assert.Zero(t, snapshotCount)
}

func TestFilteredThenUnfilteredIdentityPublicationIncludesExcludedProject(
	t *testing.T,
) {
	pgURL := testPGURL(t)
	const schema = "agentsview_filtered_identity_revision_test"
	cleanNamedPGSchema(t, pgURL, schema)
	t.Cleanup(func() { cleanNamedPGSchema(t, pgURL, schema) })
	ctx := context.Background()
	local, err := db.Open(filepath.Join(t.TempDir(), "local.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, local.Close()) })
	for _, project := range []string{"alpha", "beta"} {
		sessionID := "identity-" + project
		require.NoError(t, local.UpsertSession(db.Session{
			ID: sessionID, Project: project, Machine: "laptop", Agent: "codex",
		}))
		require.NoError(t, local.UpsertProjectIdentityObservation(ctx,
			export.ProjectIdentityObservation{
				SessionID: sessionID, Project: project, Machine: "laptop",
				RootPath:         "/workspace/" + project,
				GitRemote:        "https://example.com/" + project + ".git",
				RemoteResolution: export.ProjectResolutionResolved,
				ObservedAt:       time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC),
			},
		))
	}
	filtered, err := New(
		pgURL, schema, local, "laptop", true,
		SyncOptions{Projects: []string{"alpha"}},
	)
	require.NoError(t, err)
	require.NoError(t, filtered.EnsureSchema(ctx))
	require.NoError(t, filtered.syncProjectIdentityObservations(ctx, false))
	_, err = filtered.pg.ExecContext(ctx, `
		UPDATE source_project_identity_observations
		SET git_remote_name = 'sentinel'
		WHERE project = 'alpha'`)
	require.NoError(t, err)
	require.NoError(t, local.UpsertProjectIdentityObservation(ctx,
		export.ProjectIdentityObservation{
			SessionID: "identity-beta", Project: "beta", Machine: "laptop",
			RootPath:         "/workspace/beta",
			GitRemote:        "https://example.com/beta.git",
			GitRemoteName:    "upstream",
			RemoteResolution: export.ProjectResolutionResolved,
			ObservedAt:       time.Date(2026, 7, 11, 13, 0, 0, 0, time.UTC),
		},
	))
	require.NoError(t, filtered.syncProjectIdentityObservations(ctx, false))
	var alphaRemoteName string
	require.NoError(t, filtered.pg.QueryRowContext(ctx, `
		SELECT git_remote_name FROM source_project_identity_observations
		WHERE project = 'alpha'`).Scan(&alphaRemoteName))
	assert.Equal(t, "sentinel", alphaRemoteName,
		"out-of-scope changes must not republish the filtered identity scope")
	require.NoError(t, filtered.Close())

	unfiltered, err := New(
		pgURL, schema, local, "laptop", true, SyncOptions{},
	)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, unfiltered.Close()) })
	require.NoError(t, unfiltered.EnsureSchema(ctx))
	require.NoError(t, unfiltered.syncProjectIdentityObservations(ctx, false))
	var betaSnapshots int
	require.NoError(t, unfiltered.pg.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM source_session_project_identity_snapshots
		WHERE project = $1`, "beta").Scan(&betaSnapshots))
	assert.Equal(t, 1, betaSnapshots)
}

func TestIdentityPublicationUpdatesOnlyChangedRowsAndAppliesTombstones(
	t *testing.T,
) {
	pgURL := testPGURL(t)
	const schema = "agentsview_identity_delta_test"
	cleanNamedPGSchema(t, pgURL, schema)
	t.Cleanup(func() { cleanNamedPGSchema(t, pgURL, schema) })
	ctx := context.Background()
	local, err := db.Open(filepath.Join(t.TempDir(), "local.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, local.Close()) })
	observedAt := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	for _, project := range []string{"alpha", "beta"} {
		sessionID := "identity-" + project
		require.NoError(t, local.UpsertSession(db.Session{
			ID: sessionID, Project: project, Machine: "laptop", Agent: "codex",
		}))
		require.NoError(t, local.UpsertProjectIdentityObservation(ctx,
			export.ProjectIdentityObservation{
				SessionID: sessionID, Project: project, Machine: "laptop",
				RootPath:         "/workspace/" + project,
				GitRemote:        "https://example.com/" + project + ".git",
				GitRemoteName:    "origin",
				RemoteResolution: export.ProjectResolutionResolved,
				ObservedAt:       observedAt,
			},
		))
	}
	require.NoError(t, local.UpsertSession(db.Session{
		ID: "identity-gamma", Project: "gamma", Machine: "laptop", Agent: "codex",
	}))
	require.NoError(t, local.UpsertProjectIdentityObservation(ctx,
		export.ProjectIdentityObservation{
			SessionID: "identity-gamma", Project: "gamma", Machine: "laptop",
			RootPath:         "/workspace/gamma",
			RemoteResolution: export.ProjectResolutionUnknown,
			ObservedAt:       observedAt,
		},
	))

	syncer, err := New(pgURL, schema, local, "laptop", true, SyncOptions{})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, syncer.Close()) })
	require.NoError(t, syncer.EnsureSchema(ctx))
	require.NoError(t, syncer.syncProjectIdentityObservations(ctx, false))
	_, err = syncer.pg.ExecContext(ctx, `
		UPDATE source_project_identity_observations
		SET git_remote_name = 'sentinel'
		WHERE project = 'beta'`)
	require.NoError(t, err)

	require.NoError(t, local.UpsertProjectIdentityObservation(ctx,
		export.ProjectIdentityObservation{
			SessionID: "identity-alpha", Project: "alpha", Machine: "laptop",
			RootPath:         "/workspace/alpha",
			GitRemote:        "https://example.com/alpha.git",
			GitRemoteName:    "upstream",
			RemoteResolution: export.ProjectResolutionResolved,
			ObservedAt:       observedAt.Add(time.Hour),
		},
	))
	require.NoError(t, local.UpsertProjectIdentityObservation(ctx,
		export.ProjectIdentityObservation{
			SessionID: "identity-gamma", Project: "gamma", Machine: "laptop",
			RootPath:         "/workspace/gamma",
			GitRemote:        "https://example.com/gamma.git",
			GitRemoteName:    "origin",
			RemoteResolution: export.ProjectResolutionResolved,
			ObservedAt:       observedAt.Add(time.Hour),
		},
	))
	require.NoError(t, syncer.syncProjectIdentityObservations(ctx, false))

	var alphaRemoteName, betaRemoteName string
	require.NoError(t, syncer.pg.QueryRowContext(ctx, `
		SELECT git_remote_name FROM source_project_identity_observations
		WHERE project = 'alpha'`).Scan(&alphaRemoteName))
	require.NoError(t, syncer.pg.QueryRowContext(ctx, `
		SELECT git_remote_name FROM source_project_identity_observations
		WHERE project = 'beta'`).Scan(&betaRemoteName))
	assert.Equal(t, "upstream", alphaRemoteName)
	assert.Equal(t, "sentinel", betaRemoteName,
		"incremental publication must not rewrite unchanged rows")
	var gammaFallbacks, gammaRemotes int
	require.NoError(t, syncer.pg.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM source_project_identity_observations
		WHERE project = 'gamma' AND git_remote = ''`).Scan(&gammaFallbacks))
	require.NoError(t, syncer.pg.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM source_project_identity_observations
		WHERE project = 'gamma' AND git_remote = $1`,
		"https://example.com/gamma.git").Scan(&gammaRemotes))
	assert.Zero(t, gammaFallbacks)
	assert.Equal(t, 1, gammaRemotes)

	require.NoError(t, local.DeleteSession("identity-alpha"))
	require.NoError(t, syncer.syncProjectIdentityObservations(ctx, false))
	var snapshotCount int
	require.NoError(t, syncer.pg.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM source_session_project_identity_snapshots
		WHERE source_session_id = $1`, "identity-alpha").Scan(&snapshotCount))
	assert.Zero(t, snapshotCount)
}

// TestPushSystemFingerprintCollisionRegression verifies that the fast-path
// in pushMessages correctly detects a change when the is_system flags are
// reclassified between two ordinal sets that previously collided under the
// two-component (SUM, SUM-of-squares) fingerprint: {0,4,5} and {1,2,6}
// both produce sum=9, sumSq=41.
//
// Steps:
//  1. Push a session with 7 messages where ordinals {0,4,5} are system.
//  2. Without changing content lengths, reclassify to {1,2,6} as system.
//  3. Push again with full=false.
//  4. Confirm PG now reflects the updated is_system values.
func TestPushSystemFingerprintCollisionRegression(t *testing.T) {
	pgURL := testPGURL(t)

	const schema = "agentsview_push_sysfingerprint_test"
	pg, err := Open(pgURL, schema, true)
	require.NoError(t, err, "Open")
	defer pg.Close()

	ctx := context.Background()
	_, err = pg.Exec(`DROP SCHEMA IF EXISTS ` + schema + ` CASCADE`)
	require.NoError(t, err, "drop schema")
	require.NoError(t, EnsureSchema(ctx, pg, schema), "EnsureSchema")

	// Local SQLite DB.
	localDB, err := db.Open(
		filepath.Join(t.TempDir(), "local.db"),
	)
	require.NoError(t, err, "db.Open")
	defer localDB.Close()

	sync := &Sync{
		pg:      pg,
		local:   localDB,
		machine: "test-machine",
		schema:  schema,
		// Mark schema done so Push skips EnsureSchema.
		schemaDone: true,
	}

	const sessID = "fp-collision-001"
	sess := db.Session{
		ID:           sessID,
		Project:      "test-proj",
		Machine:      "test-machine",
		Agent:        "claude",
		MessageCount: 7,
		CreatedAt:    "2026-01-01T00:00:00Z",
	}
	require.NoError(t, localDB.UpsertSession(sess), "UpsertSession")

	// First set: system ordinals {0,4,5}.
	firstSet := map[int]bool{0: true, 4: true, 5: true}
	msgs := make([]db.Message, 7)
	for i := range 7 {
		msgs[i] = db.Message{
			SessionID:     sessID,
			Ordinal:       i,
			Role:          "user",
			Content:       "x",
			ContentLength: 1,
			IsSystem:      firstSet[i],
		}
	}
	require.NoError(t, localDB.InsertMessages(msgs), "InsertMessages (first set)")

	// First push.
	_, err = sync.Push(ctx, false, nil)
	require.NoError(t, err, "Push (first)")

	// Verify PG reflects system ordinals {0,4,5}.
	checkIsSystem(t, pg, sessID, firstSet, 7)

	// Switch to {1,2,6} — same sum(ordinal)=9, same sum(ordinal²)=41,
	// but the string fingerprint differs ("0,4,5" vs "1,2,6").
	// Replace local messages with updated is_system flags.
	secondSet := map[int]bool{1: true, 2: true, 6: true}
	for i := range 7 {
		msgs[i].IsSystem = secondSet[i]
	}
	require.NoError(t, localDB.ReplaceSessionMessages(sessID, msgs),
		"ReplaceSessionMessages (second set)")

	// Force re-evaluation by clearing both the watermark and the cached
	// session-level boundary fingerprints. The session-level fingerprint
	// does not include is_system flags (only metadata like MessageCount),
	// so the boundary cache must be cleared for the incremental push to
	// reach pushMessages and compare the message-level string fingerprint.
	require.NoError(t, localDB.SetSyncState("last_push_at", ""),
		"clearing last_push_at")
	require.NoError(t, localDB.SetSyncState(lastPushBoundaryStateKey, ""),
		"clearing boundary state")

	// Second push — must NOT skip due to fingerprint match.
	_, err = sync.Push(ctx, false, nil)
	require.NoError(t, err, "Push (second)")

	// Verify PG now reflects updated system ordinals {1,2,6}.
	checkIsSystem(t, pg, sessID, secondSet, 7)
}

func TestPushMessageContentHashRewriteRegression(t *testing.T) {
	pgURL := testPGURL(t)

	const schema = "agentsview_push_contenthash_test"
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
	const sessID = "content-hash-rewrite-001"
	sess := db.Session{
		ID:               sessID,
		Project:          "test-proj",
		Machine:          "test-machine",
		Agent:            "shelley",
		MessageCount:     2,
		UserMessageCount: 1,
		CreatedAt:        "2026-01-01T00:00:00Z",
	}
	require.NoError(t, localDB.UpsertSession(sess), "UpsertSession")
	msgs := []db.Message{
		{
			SessionID:     sessID,
			Ordinal:       1,
			Role:          "user",
			Content:       "question",
			ContentLength: len("question"),
		},
		{
			SessionID:     sessID,
			Ordinal:       2,
			Role:          "assistant",
			Content:       "answer aaaa",
			ContentLength: len("answer aaaa"),
		},
	}
	require.NoError(t, localDB.InsertMessages(msgs),
		"InsertMessages first content")

	_, err = sync.Push(ctx, false, nil)
	require.NoError(t, err, "Push first content")
	assertPGMessageContent(t, pg, sessID, 2, "answer aaaa")

	msgs[1].Content = "answer bbbb"
	msgs[1].ContentLength = len("answer bbbb")
	require.NoError(t, localDB.ReplaceSessionMessages(sessID, msgs),
		"ReplaceSessionMessages rewritten content")
	require.NoError(t, localDB.SetSyncState("last_push_at", ""),
		"clearing last_push_at")
	require.NoError(t, localDB.SetSyncState(lastPushBoundaryStateKey, ""),
		"clearing boundary state")

	_, err = sync.Push(ctx, false, nil)
	require.NoError(t, err, "Push rewritten content")
	assertPGMessageContent(t, pg, sessID, 2, "answer bbbb")
}

func TestPushMessageFlagsRewriteRegression(t *testing.T) {
	pgURL := testPGURL(t)

	const schema = "agentsview_push_msgflags_test"
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

	const sessID = "message-flags-rewrite-001"
	sess := db.Session{
		ID:               sessID,
		Project:          "test-proj",
		Machine:          "test-machine",
		Agent:            "shelley",
		MessageCount:     2,
		UserMessageCount: 1,
		CreatedAt:        "2026-01-01T00:00:00Z",
	}
	require.NoError(t, localDB.UpsertSession(sess), "UpsertSession")
	msgs := []db.Message{
		{
			SessionID:     sessID,
			Ordinal:       1,
			Role:          "user",
			Content:       "question",
			ContentLength: len("question"),
		},
		{
			SessionID:     sessID,
			Ordinal:       2,
			Role:          "assistant",
			Content:       "answer",
			ContentLength: len("answer"),
		},
	}
	require.NoError(t, localDB.InsertMessages(msgs),
		"InsertMessages first metadata")

	_, err = sync.Push(ctx, false, nil)
	require.NoError(t, err, "Push first metadata")
	assertPGMessageThinking(t, pg, sessID, 2, false, "")

	msgs[1].ThinkingText = "private chain of thought"
	msgs[1].HasThinking = true
	require.NoError(t, localDB.ReplaceSessionMessages(sessID, msgs),
		"ReplaceSessionMessages rewritten metadata")
	require.NoError(t, localDB.SetSyncState("last_push_at", ""),
		"clearing last_push_at")
	require.NoError(t, localDB.SetSyncState(lastPushBoundaryStateKey, ""),
		"clearing boundary state")

	_, err = sync.Push(ctx, false, nil)
	require.NoError(t, err, "Push rewritten metadata")
	assertPGMessageThinking(t, pg, sessID, 2, true,
		"private chain of thought")
}

func TestPushMessageNanosecondTimestampNoRewriteRegression(t *testing.T) {
	pgURL := testPGURL(t)

	const schema = "agentsview_push_msgtime_nanos_test"
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

	const sessID = "message-nanotime-rewrite-001"
	sess := db.Session{
		ID:               sessID,
		Project:          "test-proj",
		Machine:          "test-machine",
		Agent:            "shelley",
		MessageCount:     1,
		UserMessageCount: 1,
		CreatedAt:        "2026-01-01T00:00:00Z",
	}
	require.NoError(t, localDB.UpsertSession(sess), "UpsertSession")
	require.NoError(t, localDB.InsertMessages([]db.Message{{
		SessionID:     sessID,
		Ordinal:       1,
		Role:          "user",
		Content:       "question",
		ContentLength: len("question"),
		Timestamp:     "2026-01-01T00:00:00.123456789Z",
	}}), "InsertMessages")

	_, err = sync.Push(ctx, false, nil)
	require.NoError(t, err, "Push first timestamp")
	assertPGMessageTimestamp(t, pg, sessID, 1,
		"2026-01-01T00:00:00.123456Z")

	ctidBefore := pgMessageCTID(t, pg, sessID, 1)
	require.NoError(t, localDB.SetSyncState("last_push_at", ""),
		"clearing last_push_at")
	require.NoError(t, localDB.SetSyncState(lastPushBoundaryStateKey, ""),
		"clearing boundary state")

	_, err = sync.Push(ctx, false, nil)
	require.NoError(t, err, "Push same timestamp")
	assert.Equal(t, ctidBefore, pgMessageCTID(t, pg, sessID, 1),
		"microsecond-equivalent timestamps should hit the fast path")
}

// TestPushSessionTerminationStatus verifies that pushSession round-trips
// the termination_status column to PG: a non-nil value writes the string,
// and a subsequent push with nil clears the column back to NULL via the
// ON CONFLICT DO UPDATE path.
func TestPushSessionTerminationStatus(t *testing.T) {
	pgURL := testPGURL(t)

	const schema = "agentsview_push_termstatus_test"
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
	markerID, err := sync.pushMarkerID()
	require.NoError(t, err, "pushMarkerID")

	pending := "tool_call_pending"
	sess := db.Session{
		ID:               "term-test-1",
		Project:          "p",
		Machine:          "test-machine",
		Agent:            "claude",
		MessageCount:     1,
		UserMessageCount: 1,
		// CreatedAt must be parseable by ParseSQLiteTimestamp;
		// PG's NOT NULL on created_at would otherwise reject NULL.
		CreatedAt:         "2024-01-01T00:00:00Z",
		TerminationStatus: &pending,
	}

	pushOnce := func(s db.Session) {
		t.Helper()
		tx, err := pg.BeginTx(ctx, nil)
		require.NoError(t, err, "BeginTx")
		if err := sync.pushSession(ctx, tx, s, markerID, nil); err != nil {
			_ = tx.Rollback()
			t.Fatalf("pushSession: %v", err)
		}
		require.NoError(t, tx.Commit(), "Commit")
	}

	pushOnce(sess)

	var got *string
	require.NoError(t, pg.QueryRow(
		`SELECT termination_status FROM sessions WHERE id = $1`,
		sess.ID,
	).Scan(&got), "read back")
	require.NotNil(t, got)
	assert.Equal(t, "tool_call_pending", *got)

	// Update to NULL and verify ON CONFLICT clears it.
	sess.TerminationStatus = nil
	pushOnce(sess)

	require.NoError(t, pg.QueryRow(
		`SELECT termination_status FROM sessions WHERE id = $1`,
		sess.ID,
	).Scan(&got), "read back 2")
	assert.Nil(t, got)
}

func TestPushSessionPreservesSourceMachine(t *testing.T) {
	pgURL := testPGURL(t)

	const schema = "agentsview_push_source_machine_test"
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
		machine:    "push-host",
		schema:     schema,
		schemaDone: true,
	}

	remoteSession := db.Session{
		ID:           "remote-source-machine-1",
		Project:      "proj",
		Machine:      "remote-host",
		Agent:        "claude",
		MessageCount: 1,
		CreatedAt:    "2026-01-01T00:00:00Z",
	}

	tx, err := pg.BeginTx(ctx, nil)
	require.NoError(t, err, "BeginTx")
	markerID, err := sync.pushMarkerID()
	require.NoError(t, err, "pushMarkerID")
	require.NoError(t, sync.pushSession(ctx, tx, remoteSession, markerID, nil), "pushSession")
	require.NoError(t, tx.Commit(), "Commit")

	var got string
	require.NoError(t, pg.QueryRow(
		`SELECT machine FROM sessions WHERE id = $1`,
		remoteSession.ID,
	).Scan(&got), "read back machine")
	assert.Equal(t, "remote-host", got)
}

func TestPushPreservesPGServeLocalCurationFields(t *testing.T) {
	pgURL := testPGURL(t)

	const schema = "agentsview_push_preserve_pg_curation_test"
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

	const sessID = "pg-curation-preserve-001"
	firstMessage := "source before"
	sess := db.Session{
		ID:               sessID,
		Project:          "test-proj",
		Machine:          "test-machine",
		Agent:            "claude",
		FirstMessage:     &firstMessage,
		MessageCount:     1,
		UserMessageCount: 1,
		CreatedAt:        "2026-01-01T00:00:00Z",
	}
	require.NoError(t, localDB.UpsertSession(sess), "UpsertSession")
	require.NoError(t, localDB.InsertMessages([]db.Message{{
		SessionID:     sessID,
		Ordinal:       0,
		Role:          "user",
		Content:       firstMessage,
		ContentLength: len(firstMessage),
	}}), "InsertMessages")

	_, err = sync.Push(ctx, false, nil)
	require.NoError(t, err, "Push before local PG curation")

	store, err := NewStore(pgURL, schema, true)
	require.NoError(t, err, "NewStore")
	defer store.Close()

	renamed := "Renamed in PG serve"
	require.NoError(t, store.RenameSession(sessID, &renamed),
		"RenameSession")
	require.NoError(t, store.SoftDeleteSession(sessID),
		"SoftDeleteSession")

	updatedFirstMessage := "source after"
	sess.FirstMessage = &updatedFirstMessage
	require.NoError(t, localDB.UpsertSession(sess),
		"UpsertSession updated source row")
	require.NoError(t, localDB.SetSyncState("last_push_at", ""),
		"clearing last_push_at")
	require.NoError(t, localDB.SetSyncState(lastPushBoundaryStateKey, ""),
		"clearing boundary state")

	_, err = sync.Push(ctx, false, nil)
	require.NoError(t, err, "Push after local PG curation")

	var gotDisplayName sql.NullString
	var gotDeleted bool
	var gotFirstMessage sql.NullString
	require.NoError(t, pg.QueryRow(
		`SELECT display_name, deleted_at IS NOT NULL, first_message
		 FROM sessions WHERE id = $1`,
		sessID,
	).Scan(&gotDisplayName, &gotDeleted, &gotFirstMessage),
		"reading pushed curation row")
	require.True(t, gotDisplayName.Valid, "display_name should persist")
	assert.Equal(t, renamed, gotDisplayName.String)
	assert.True(t, gotDeleted, "PG-local trash state should persist")
	require.True(t, gotFirstMessage.Valid, "first_message should remain visible")
	assert.Equal(t, updatedFirstMessage, gotFirstMessage.String,
		"source-owned fields should still update")
}

func TestPushPreservesPGServePermanentDeletes(t *testing.T) {
	pgURL := testPGURL(t)

	const schema = "agentsview_push_preserve_pg_deletes_test"
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

	const deleteIfTrashedID = "pg-delete-preserve-001"
	const emptyTrashID = "pg-delete-preserve-002"
	sourceRows := []db.Session{
		{
			ID:               deleteIfTrashedID,
			Project:          "test-proj",
			Machine:          "test-machine",
			Agent:            "claude",
			MessageCount:     1,
			UserMessageCount: 1,
			CreatedAt:        "2026-01-01T00:00:00Z",
		},
		{
			ID:               emptyTrashID,
			Project:          "test-proj",
			Machine:          "test-machine",
			Agent:            "claude",
			MessageCount:     1,
			UserMessageCount: 1,
			CreatedAt:        "2026-01-01T00:00:01Z",
		},
	}
	for _, sess := range sourceRows {
		require.NoError(t, localDB.UpsertSession(sess),
			"UpsertSession "+sess.ID)
		require.NoError(t, localDB.InsertMessages([]db.Message{{
			SessionID:     sess.ID,
			Ordinal:       0,
			Role:          "user",
			Content:       sess.ID,
			ContentLength: len(sess.ID),
		}}), "InsertMessages "+sess.ID)
	}

	_, err = sync.Push(ctx, false, nil)
	require.NoError(t, err, "Push before PG permanent deletes")

	store, err := NewStore(pgURL, schema, true)
	require.NoError(t, err, "NewStore")
	defer store.Close()

	require.NoError(t, store.SoftDeleteSession(deleteIfTrashedID),
		"SoftDeleteSession deleteIfTrashedID")
	deleted, err := store.DeleteSessionIfTrashed(deleteIfTrashedID)
	require.NoError(t, err, "DeleteSessionIfTrashed")
	assert.EqualValues(t, 1, deleted)

	deletedCount, err := store.SoftDeleteSessions([]string{emptyTrashID})
	require.NoError(t, err, "SoftDeleteSessions")
	assert.Equal(t, 1, deletedCount)
	emptied, err := store.EmptyTrash()
	require.NoError(t, err, "EmptyTrash")
	assert.Equal(t, 1, emptied)

	for _, sess := range sourceRows {
		updated := sess
		updated.MessageCount = 2
		require.NoError(t, localDB.UpsertSession(updated),
			"UpsertSession updated "+sess.ID)
		require.NoError(t, localDB.ReplaceSessionMessages(sess.ID, []db.Message{
			{
				SessionID:     sess.ID,
				Ordinal:       0,
				Role:          "user",
				Content:       sess.ID + "-updated",
				ContentLength: len(sess.ID + "-updated"),
			},
			{
				SessionID:     sess.ID,
				Ordinal:       1,
				Role:          "assistant",
				Content:       "reply",
				ContentLength: len("reply"),
			},
		}), "ReplaceSessionMessages "+sess.ID)
	}

	_, err = sync.Push(ctx, true, nil)
	require.NoError(t, err, "full Push after PG permanent deletes")

	for _, sessionID := range []string{deleteIfTrashedID, emptyTrashID} {
		var count int
		require.NoError(t, pg.QueryRow(
			`SELECT COUNT(*) FROM sessions WHERE id = $1`,
			sessionID,
		).Scan(&count), "count deleted session "+sessionID)
		assert.Zero(t, count, "permanently deleted session should stay absent")
	}

	var excludedCount int
	require.NoError(t, pg.QueryRow(
		`SELECT COUNT(*) FROM excluded_sessions
		 WHERE id IN ($1, $2)`,
		deleteIfTrashedID, emptyTrashID,
	).Scan(&excludedCount), "count pg excluded sessions")
	assert.Equal(t, 2, excludedCount)
}

func TestPushPurgesRowsForPGExcludedSessions(t *testing.T) {
	pgURL := testPGURL(t)

	const schema = "agentsview_push_excluded_purge_test"
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

	const sessionID = "pg-excluded-purge-001"
	sess := db.Session{
		ID:               sessionID,
		Project:          "test-proj",
		Machine:          "test-machine",
		Agent:            "claude",
		MessageCount:     1,
		UserMessageCount: 1,
		CreatedAt:        "2026-01-01T00:00:00Z",
	}
	require.NoError(t, localDB.UpsertSession(sess), "UpsertSession")
	require.NoError(t, localDB.InsertMessages([]db.Message{{
		SessionID:     sessionID,
		Ordinal:       0,
		Role:          "user",
		Content:       "hello",
		ContentLength: len("hello"),
	}}), "InsertMessages")

	_, err = sync.Push(ctx, false, nil)
	require.NoError(t, err, "initial Push")

	_, err = pg.Exec(
		`INSERT INTO excluded_sessions (id) VALUES ($1)`,
		sessionID,
	)
	require.NoError(t, err, "insert excluded session")

	updated := sess
	updated.MessageCount = 2
	require.NoError(t, localDB.UpsertSession(updated), "update local session")
	require.NoError(t, localDB.ReplaceSessionMessages(sessionID, []db.Message{
		{
			SessionID:     sessionID,
			Ordinal:       0,
			Role:          "user",
			Content:       "hello updated",
			ContentLength: len("hello updated"),
		},
		{
			SessionID:     sessionID,
			Ordinal:       1,
			Role:          "assistant",
			Content:       "reply",
			ContentLength: len("reply"),
		},
	}), "ReplaceSessionMessages")

	res, err := sync.Push(ctx, true, nil)
	require.NoError(t, err, "full Push with excluded row")
	assert.Zero(t, res.SessionsPushed)

	var count int
	require.NoError(t, pg.QueryRow(
		`SELECT COUNT(*) FROM sessions WHERE id = $1`,
		sessionID,
	).Scan(&count), "count purged session")
	assert.Zero(t, count, "excluded session row should be purged")
}

func TestPushPurgesOwnedLegacyTraeRowsDuringNamespaceMigration(t *testing.T) {
	pgURL := testPGURL(t)

	const schema = "agentsview_push_trae_legacy_purge_test"
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
	store := &Store{pg: pg}

	const legacyID = "trae:collision"
	legacy := db.Session{
		ID:               legacyID,
		Project:          "test-proj",
		Machine:          "test-machine",
		Agent:            "trae",
		SourceSessionID:  "collision",
		MessageCount:     1,
		UserMessageCount: 1,
		CreatedAt:        "2026-01-01T00:00:00Z",
	}
	require.NoError(t, localDB.UpsertSession(legacy), "UpsertSession legacy")
	require.NoError(t, localDB.InsertMessages([]db.Message{{
		SessionID:     legacyID,
		Ordinal:       0,
		Role:          "user",
		Content:       "legacy",
		ContentLength: len("legacy"),
	}}), "InsertMessages legacy")

	_, err = sync.Push(ctx, false, nil)
	require.NoError(t, err, "initial Push")
	starred, err := store.StarSession(legacyID)
	require.NoError(t, err, "StarSession legacy")
	require.True(t, starred)
	pinNote := "legacy pin"
	_, err = store.PinMessage(legacyID, 0, &pinNote)
	require.NoError(t, err, "PinMessage legacy")

	const namespacedID = "trae:globalStorage:collision"
	namespaced := legacy
	namespaced.ID = namespacedID
	require.NoError(t, localDB.UpsertSession(namespaced), "UpsertSession namespaced")
	require.NoError(t, localDB.InsertMessages([]db.Message{{
		SessionID:     namespacedID,
		Ordinal:       0,
		Role:          "user",
		Content:       "namespaced",
		ContentLength: len("namespaced"),
	}}), "InsertMessages namespaced")

	_, err = sync.Push(ctx, true, nil)
	require.NoError(t, err, "full Push after namespace migration")

	var count int
	require.NoError(t, pg.QueryRow(
		`SELECT COUNT(*) FROM sessions WHERE id = $1`,
		legacyID,
	).Scan(&count), "count legacy trae session")
	assert.Zero(t, count, "legacy trae row should be purged")

	require.NoError(t, pg.QueryRow(
		`SELECT COUNT(*) FROM sessions WHERE id = $1`,
		namespacedID,
	).Scan(&count), "count namespaced trae session")
	assert.Equal(t, 1, count, "namespaced trae row should remain")

	require.NoError(t, pg.QueryRow(
		`SELECT COUNT(*) FROM starred_sessions WHERE session_id = $1`,
		namespacedID,
	).Scan(&count), "count migrated trae star")
	assert.Equal(t, 1, count, "legacy trae star should move to namespaced row")

	var gotNote sql.NullString
	require.NoError(t, pg.QueryRow(
		`SELECT note FROM pinned_messages WHERE session_id = $1`,
		namespacedID,
	).Scan(&gotNote), "read migrated trae pin")
	require.True(t, gotNote.Valid)
	assert.Equal(t, pinNote, gotNote.String)
}

func TestPushPurgesOwnedHostPrefixedLegacyTraeRowsDuringNamespaceMigration(t *testing.T) {
	pgURL := testPGURL(t)

	const schema = "agentsview_push_trae_host_legacy_purge_test"
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
	store := &Store{pg: pg}
	legacyRev := "legacy-rev"

	const legacyID = "laptop~trae:collision"
	legacy := db.Session{
		ID:                 legacyID,
		Project:            "test-proj",
		Machine:            "test-machine",
		Agent:              "trae",
		SourceSessionID:    "collision",
		TranscriptRevision: &legacyRev,
		MessageCount:       1,
		UserMessageCount:   1,
		CreatedAt:          "2026-01-01T00:00:00Z",
	}
	require.NoError(t, localDB.UpsertSession(legacy), "UpsertSession legacy")
	require.NoError(t, localDB.InsertMessages([]db.Message{{
		SessionID:     legacyID,
		Ordinal:       0,
		Role:          "user",
		Content:       "legacy",
		ContentLength: len("legacy"),
	}}), "InsertMessages legacy")

	_, err = sync.Push(ctx, false, nil)
	require.NoError(t, err, "initial Push")
	starred, err := store.StarSession(legacyID)
	require.NoError(t, err, "StarSession legacy")
	require.True(t, starred)
	pinNote := "legacy host pin"
	_, err = store.PinMessage(legacyID, 0, &pinNote)
	require.NoError(t, err, "PinMessage legacy")

	const namespacedID = "laptop~trae:globalStorage:collision"
	namespaced := legacy
	namespaced.ID = namespacedID
	namespaced.TranscriptRevision = &legacyRev
	require.NoError(t, localDB.UpsertSession(namespaced), "UpsertSession namespaced")
	require.NoError(t, localDB.InsertMessages([]db.Message{{
		SessionID:     namespacedID,
		Ordinal:       0,
		Role:          "user",
		Content:       "namespaced",
		ContentLength: len("namespaced"),
	}}), "InsertMessages namespaced")

	_, err = sync.Push(ctx, true, nil)
	require.NoError(t, err, "full Push after namespace migration")

	var count int
	require.NoError(t, pg.QueryRow(
		`SELECT COUNT(*) FROM sessions WHERE id = $1`,
		legacyID,
	).Scan(&count), "count legacy trae session")
	assert.Zero(t, count, "host-prefixed legacy trae row should be purged")

	require.NoError(t, pg.QueryRow(
		`SELECT COUNT(*) FROM starred_sessions WHERE session_id = $1`,
		namespacedID,
	).Scan(&count), "count migrated trae star")
	assert.Equal(t, 1, count)
}

func TestPushMigratesLegacyTraeStateToMatchingNamespaceOnly(t *testing.T) {
	pgURL := testPGURL(t)

	const schema = "agentsview_push_trae_collision_guard_test"
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
	store := &Store{pg: pg}
	workspaceRev := "workspace-rev"
	globalRev := "global-rev"

	const legacyID = "trae:collision"
	legacy := db.Session{
		ID:                 legacyID,
		Project:            "test-proj",
		Machine:            "test-machine",
		Agent:              "trae",
		SourceSessionID:    "collision",
		TranscriptRevision: &workspaceRev,
		MessageCount:       1,
		UserMessageCount:   1,
		CreatedAt:          "2026-01-01T00:00:00Z",
	}
	require.NoError(t, localDB.UpsertSession(legacy), "UpsertSession legacy")
	require.NoError(t, localDB.InsertMessages([]db.Message{{
		SessionID:     legacyID,
		Ordinal:       0,
		Role:          "user",
		Content:       "workspace body",
		ContentLength: len("workspace body"),
	}}), "InsertMessages legacy")

	_, err = sync.Push(ctx, false, nil)
	require.NoError(t, err, "initial Push")
	starred, err := store.StarSession(legacyID)
	require.NoError(t, err, "StarSession legacy")
	require.True(t, starred)
	pinNote := "workspace pin"
	_, err = store.PinMessage(legacyID, 0, &pinNote)
	require.NoError(t, err, "PinMessage legacy")

	workspace := legacy
	workspace.ID = "trae:workspaceStorage:collision"
	workspace.TranscriptRevision = &workspaceRev
	global := legacy
	global.ID = "trae:globalStorage:collision"
	global.TranscriptRevision = &globalRev
	require.NoError(t, localDB.UpsertSession(workspace), "UpsertSession workspace")
	require.NoError(t, localDB.UpsertSession(global), "UpsertSession global")
	require.NoError(t, localDB.InsertMessages([]db.Message{{
		SessionID:     workspace.ID,
		Ordinal:       0,
		Role:          "user",
		Content:       "workspace body",
		ContentLength: len("workspace body"),
	}, {
		SessionID:     global.ID,
		Ordinal:       0,
		Role:          "user",
		Content:       "global body",
		ContentLength: len("global body"),
	}}), "InsertMessages namespaced")

	_, err = sync.Push(ctx, true, nil)
	require.NoError(t, err, "full Push after split namespace migration")

	var count int
	require.NoError(t, pg.QueryRow(
		`SELECT COUNT(*) FROM starred_sessions WHERE session_id = $1`,
		global.ID,
	).Scan(&count), "count migrated trae star on global")
	assert.Zero(t, count, "global namespace must not steal legacy curation")

	require.NoError(t, pg.QueryRow(
		`SELECT COUNT(*) FROM starred_sessions WHERE session_id = $1`,
		workspace.ID,
	).Scan(&count), "count migrated trae star on workspace")
	assert.Equal(t, 1, count, "matching namespace receives legacy curation")
}

func TestPushSessionSkipsPGExcludedSession(t *testing.T) {
	pgURL := testPGURL(t)

	const schema = "agentsview_push_session_excluded_test"
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

	const sessionID = "pg-excluded-push-session-001"
	_, err = pg.Exec(
		`INSERT INTO excluded_sessions (id) VALUES ($1)`,
		sessionID,
	)
	require.NoError(t, err, "insert excluded session")

	tx, err := pg.BeginTx(ctx, nil)
	require.NoError(t, err, "BeginTx")
	markerID, err := sync.pushMarkerID()
	require.NoError(t, err, "pushMarkerID")
	err = sync.pushSession(ctx, tx, db.Session{
		ID:        sessionID,
		Project:   "test-proj",
		Machine:   "test-machine",
		Agent:     "claude",
		CreatedAt: "2026-01-01T00:00:00Z",
	}, markerID, nil)
	require.ErrorIs(t, err, errSessionExcluded)
	require.NoError(t, tx.Rollback(), "Rollback")

	var count int
	require.NoError(t, pg.QueryRow(
		`SELECT COUNT(*) FROM sessions WHERE id = $1`,
		sessionID,
	).Scan(&count), "count skipped session")
	assert.Zero(t, count)
}

func TestPushUpdatesSourceCurationFieldsWithoutPGOverride(t *testing.T) {
	pgURL := testPGURL(t)

	const schema = "agentsview_push_source_curation_update_test"
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

	const renamedID = "pg-source-curation-rename-001"
	sourceNameOne := "Source name one"
	sourceNameTwo := "Source name two"
	renamed := db.Session{
		ID:               renamedID,
		Project:          "test-proj",
		Machine:          "test-machine",
		Agent:            "claude",
		MessageCount:     1,
		UserMessageCount: 1,
		CreatedAt:        "2026-01-01T00:00:00Z",
	}
	require.NoError(t, localDB.UpsertSession(renamed), "UpsertSession renamed")
	require.NoError(t, localDB.RenameSession(renamedID, &sourceNameOne),
		"RenameSession renamed initial source name")
	require.NoError(t, localDB.InsertMessages([]db.Message{{
		SessionID:     renamedID,
		Ordinal:       0,
		Role:          "user",
		Content:       renamedID,
		ContentLength: len(renamedID),
	}}), "InsertMessages renamed")

	const restoredID = "pg-source-curation-restore-001"
	restored := db.Session{
		ID:               restoredID,
		Project:          "test-proj",
		Machine:          "test-machine",
		Agent:            "claude",
		MessageCount:     1,
		UserMessageCount: 1,
		CreatedAt:        "2026-01-01T00:00:01Z",
	}
	require.NoError(t, localDB.UpsertSession(restored), "UpsertSession restored")
	require.NoError(t, localDB.InsertMessages([]db.Message{{
		SessionID:     restoredID,
		Ordinal:       0,
		Role:          "user",
		Content:       restoredID,
		ContentLength: len(restoredID),
	}}), "InsertMessages restored")
	require.NoError(t, localDB.SoftDeleteSession(restoredID),
		"SoftDeleteSession restored")

	_, err = sync.Push(ctx, false, nil)
	require.NoError(t, err, "Push initial source curation")

	require.NoError(t, localDB.RenameSession(renamedID, &sourceNameTwo),
		"RenameSession renamed source update")
	restoredCount, err := localDB.RestoreSession(restoredID)
	require.NoError(t, err, "RestoreSession restoredID")
	assert.EqualValues(t, 1, restoredCount)

	_, err = sync.Push(ctx, false, nil)
	require.NoError(t, err, "Push source curation update")

	var gotDisplayName sql.NullString
	require.NoError(t, pg.QueryRow(
		`SELECT display_name FROM sessions WHERE id = $1`,
		renamedID,
	).Scan(&gotDisplayName), "read renamed display_name")
	require.True(t, gotDisplayName.Valid, "display_name should remain populated")
	assert.Equal(t, sourceNameTwo, gotDisplayName.String)

	var deletedAt sql.NullTime
	require.NoError(t, pg.QueryRow(
		`SELECT deleted_at FROM sessions WHERE id = $1`,
		restoredID,
	).Scan(&deletedAt), "read restored deleted_at")
	assert.False(t, deletedAt.Valid, "source restore should clear deleted_at")
}

func TestPushPreservesLegacyPGCurationWithoutSourceBaseline(t *testing.T) {
	pgURL := testPGURL(t)

	const schema = "agentsview_push_legacy_source_curation_test"
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

	const renamedID = "pg-legacy-curation-rename-001"
	sourceNameOne := "Source name one"
	sourceNameTwo := "Source name two"
	renamed := db.Session{
		ID:               renamedID,
		Project:          "test-proj",
		Machine:          "test-machine",
		Agent:            "claude",
		MessageCount:     1,
		UserMessageCount: 1,
		CreatedAt:        "2026-01-01T00:00:00Z",
	}
	require.NoError(t, localDB.UpsertSession(renamed), "UpsertSession renamed")
	require.NoError(t, localDB.RenameSession(renamedID, &sourceNameOne),
		"RenameSession renamed initial source name")
	require.NoError(t, localDB.InsertMessages([]db.Message{{
		SessionID:     renamedID,
		Ordinal:       0,
		Role:          "user",
		Content:       renamedID,
		ContentLength: len(renamedID),
	}}), "InsertMessages renamed")

	const trashedID = "pg-legacy-curation-trash-001"
	trashed := db.Session{
		ID:               trashedID,
		Project:          "test-proj",
		Machine:          "test-machine",
		Agent:            "claude",
		MessageCount:     1,
		UserMessageCount: 1,
		CreatedAt:        "2026-01-01T00:00:01Z",
	}
	require.NoError(t, localDB.UpsertSession(trashed), "UpsertSession trashed")
	require.NoError(t, localDB.InsertMessages([]db.Message{{
		SessionID:     trashedID,
		Ordinal:       0,
		Role:          "user",
		Content:       trashedID,
		ContentLength: len(trashedID),
	}}), "InsertMessages trashed")

	_, err = sync.Push(ctx, false, nil)
	require.NoError(t, err, "Push initial legacy source rows")

	pgName := "PG curated name"
	_, err = pg.Exec(
		`UPDATE sessions
		 SET display_name = $2,
		     source_display_name = NULL
		 WHERE id = $1`,
		renamedID, pgName,
	)
	require.NoError(t, err, "simulate legacy PG rename")

	_, err = pg.Exec(
		`UPDATE sessions
		 SET deleted_at = NOW(),
		     source_deleted_at = NULL
		 WHERE id = $1`,
		trashedID,
	)
	require.NoError(t, err, "simulate legacy PG trash")

	require.NoError(t, localDB.RenameSession(renamedID, &sourceNameTwo),
		"RenameSession renamed source update")

	_, err = sync.Push(ctx, false, nil)
	require.NoError(t, err, "Push legacy source curation update")

	var gotDisplayName sql.NullString
	var gotSourceDisplayName sql.NullString
	require.NoError(t, pg.QueryRow(
		`SELECT display_name, source_display_name
		 FROM sessions WHERE id = $1`,
		renamedID,
	).Scan(&gotDisplayName, &gotSourceDisplayName),
		"read renamed legacy curation state")
	require.True(t, gotDisplayName.Valid, "legacy display_name should remain populated")
	assert.Equal(t, pgName, gotDisplayName.String)
	require.True(t, gotSourceDisplayName.Valid,
		"legacy source_display_name should capture the source baseline")
	assert.Equal(t, sourceNameTwo, gotSourceDisplayName.String)

	var deletedAt sql.NullTime
	var sourceDeletedAt sql.NullTime
	require.NoError(t, pg.QueryRow(
		`SELECT deleted_at, source_deleted_at
		 FROM sessions WHERE id = $1`,
		trashedID,
	).Scan(&deletedAt, &sourceDeletedAt),
		"read trashed legacy curation state")
	assert.True(t, deletedAt.Valid,
		"legacy PG delete should remain in place without a source baseline")
	assert.False(t, sourceDeletedAt.Valid,
		"active source row should keep a null source_deleted_at baseline")
}

// TestPushSyncsUsageEventsForZeroMessageSession verifies that a session
// carrying token/cost accounting as a usage_event but no transcript
// messages still has its usage_event pushed to PG. This is the shape of a
// hermes state.db-only session: parseHermesStateSession emits a single
// usage_event (model + tokens) with MessageCount 0. The session row (and
// its aggregate token columns) pushes via pushSession, but pushMessages
// must not skip usage_event syncing just because the message count is 0 --
// otherwise the dashboard shows tokens with a $0 cost.
func TestPushSyncsUsageEventsForZeroMessageSession(t *testing.T) {
	pgURL := testPGURL(t)

	const schema = "agentsview_push_zeromsg_usage_test"
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

	const sessID = "hermes:zero-msg-001"
	started := "2026-05-26T10:00:00Z"
	sess := db.Session{
		ID:                   sessID,
		Project:              "hermes-proj",
		Machine:              "test-machine",
		Agent:                "hermes",
		MessageCount:         0,
		StartedAt:            &started,
		CreatedAt:            started,
		TotalOutputTokens:    500000,
		HasTotalOutputTokens: true,
	}
	require.NoError(t, localDB.UpsertSession(sess), "UpsertSession")

	// gpt-5.5 usage event with NULL cost so it is priced from the catalog.
	require.NoError(t, localDB.ReplaceSessionUsageEvents(sessID, []db.UsageEvent{{
		SessionID:    sessID,
		Source:       "session",
		Model:        "gpt-5.5",
		InputTokens:  1000000,
		OutputTokens: 500000,
		CostUSD:      nil,
		OccurredAt:   started,
		DedupKey:     "session:" + sessID,
	}}), "ReplaceSessionUsageEvents")

	_, err = sync.Push(ctx, false, nil)
	require.NoError(t, err, "Push")

	// The usage_event must reach PG even though the session has no messages.
	var pgUsageCount int
	require.NoError(t, pg.QueryRow(
		`SELECT COUNT(*) FROM usage_events WHERE session_id = $1`,
		sessID,
	).Scan(&pgUsageCount), "count pg usage_events")
	assert.Equal(t, 1, pgUsageCount,
		"usage_event for a zero-message session was not pushed")

	// And the read side prices it from the gpt-5.5 catalog rate:
	// input 5/Mtok, output 30/Mtok -> 1.0*5 + 0.5*30 = 20.
	store, err := NewStore(pgURL, schema, true)
	require.NoError(t, err, "NewStore")
	defer store.Close()

	result, err := store.GetDailyUsage(ctx, db.UsageFilter{
		From:     "2026-05-26",
		To:       "2026-05-26",
		Timezone: "UTC",
	})
	require.NoError(t, err, "GetDailyUsage")
	assert.InDelta(t, 20.0, result.Totals.TotalCost, 1e-9,
		"gpt-5.5 usage should be priced from the catalog")
}

func TestPushSyncsCursorUsageEventsIntoPGDailyUsage(t *testing.T) {
	pgURL := testPGURL(t)

	const schema = "agentsview_cursor_usage_pg_test"
	pg, err := Open(pgURL, schema, true)
	require.NoError(t, err, "Open")
	defer pg.Close()

	ctx := context.Background()
	_, err = pg.Exec(`DROP SCHEMA IF EXISTS ` + schema + ` CASCADE`)
	require.NoError(t, err, "drop schema")
	require.NoError(t, EnsureSchema(ctx, pg, schema), "EnsureSchema")

	localDB, err := db.Open(filepath.Join(t.TempDir(), "sessions.db"))
	require.NoError(t, err, "open local db")
	defer localDB.Close()
	require.NoError(t, localDB.UpsertModelPricing([]db.ModelPricing{{
		ModelPattern:         "claude-4.6-opus-high-thinking",
		InputPerMTok:         5.0,
		OutputPerMTok:        25.0,
		CacheCreationPerMTok: 6.25,
		CacheReadPerMTok:     0.5,
	}}), "UpsertModelPricing")
	require.NoError(t, localDB.InsertCursorUsageEvents([]db.CursorUsageEvent{{
		OccurredAt:       "2026-05-14T10:05:00Z",
		Model:            "claude-4.6-opus-high-thinking",
		Kind:             "USAGE_EVENT_KIND_USAGE_BASED",
		InputTokens:      1234,
		OutputTokens:     567,
		CacheWriteTokens: 12,
		CacheReadTokens:  34,
		ChargedCents:     15.66,
		CursorTokenFee:   3.32,
		UserID:           "152683922",
		UserEmail:        "member@example.com",
		IsHeadless:       false,
	}}), "InsertCursorUsageEvents")

	sync := &Sync{
		local:      localDB,
		pg:         pg,
		machine:    "test-machine",
		schema:     schema,
		schemaDone: true,
	}

	_, err = sync.Push(ctx, false, nil)
	require.NoError(t, err, "Push")

	assert.Equal(t, 1, pgTableCount(t, ctx, pg, "cursor_usage_events"))

	store, err := NewStore(pgURL, schema, true)
	require.NoError(t, err, "NewStore")
	defer store.Close()

	result, err := store.GetDailyUsage(ctx, db.UsageFilter{
		From:       "2026-05-14",
		To:         "2026-05-14",
		Timezone:   "UTC",
		Breakdowns: true,
	})
	require.NoError(t, err, "GetDailyUsage")
	require.Len(t, result.Daily, 1, "daily entries")
	assert.Equal(t, 1234, result.Daily[0].InputTokens)
	assert.Equal(t, 567, result.Daily[0].OutputTokens)
	assert.Equal(t, 12, result.Daily[0].CacheCreationTokens)
	assert.Equal(t, 34, result.Daily[0].CacheReadTokens)
	assert.InDelta(t, 0.1566, result.Daily[0].TotalCost, 1e-9)
	assert.Empty(t, result.Projects, "cursor-only usage should not emit project identities")
	assert.NotContains(t, result.Projects, "")
	assert.Equal(t, 0, result.SessionCounts.Total)
	assert.Empty(t, result.SessionCounts.ByAgent)
	assert.Empty(t, result.SessionCounts.ByProject)
	require.Len(t, result.Daily[0].AgentBreakdowns, 1)
	assert.Equal(t, "cursor", result.Daily[0].AgentBreakdowns[0].Agent)
}

func TestPushCursorUsageEventsPreservesRowsFromOtherMachines(t *testing.T) {
	pgURL := testPGURL(t)

	const schema = "agentsview_cursor_usage_append_only_pg_test"
	pg, err := Open(pgURL, schema, true)
	require.NoError(t, err, "Open")
	defer pg.Close()

	ctx := context.Background()
	_, err = pg.Exec(`DROP SCHEMA IF EXISTS ` + schema + ` CASCADE`)
	require.NoError(t, err, "drop schema")
	require.NoError(t, EnsureSchema(ctx, pg, schema), "EnsureSchema")

	_, err = pg.ExecContext(ctx, `
		INSERT INTO cursor_usage_events (
			occurred_at, model, kind,
			input_tokens, output_tokens,
			cache_write_tokens, cache_read_tokens,
			charged_cents, cursor_token_fee,
			user_id, user_email, is_headless, dedup_key
		) VALUES (
			'2026-05-14T09:05:00Z'::timestamptz,
			'claude-4.6-opus-high-thinking',
			'USAGE_EVENT_KIND_USAGE_BASED',
			10, 20, 0, 30,
			1.25, 0.25,
			'other-user', 'other@example.com', false, 'other-machine-row'
		)`)
	require.NoError(t, err, "seed existing pg row")

	localDB, err := db.Open(filepath.Join(t.TempDir(), "sessions.db"))
	require.NoError(t, err, "open local db")
	defer localDB.Close()
	require.NoError(t, localDB.InsertCursorUsageEvents([]db.CursorUsageEvent{{
		OccurredAt:       "2026-05-14T10:05:00Z",
		Model:            "claude-4.6-opus-high-thinking",
		Kind:             "USAGE_EVENT_KIND_USAGE_BASED",
		InputTokens:      1234,
		OutputTokens:     567,
		CacheWriteTokens: 12,
		CacheReadTokens:  34,
		ChargedCents:     15.66,
		CursorTokenFee:   3.32,
		UserID:           "152683922",
		UserEmail:        "member@example.com",
		IsHeadless:       false,
		DedupKey:         "local-machine-row",
	}}), "InsertCursorUsageEvents")

	sync := &Sync{
		local:      localDB,
		pg:         pg,
		machine:    "test-machine",
		schema:     schema,
		schemaDone: true,
	}

	_, err = sync.Push(ctx, false, nil)
	require.NoError(t, err, "Push")

	assert.Equal(t, 2, pgTableCount(t, ctx, pg, "cursor_usage_events"))
}

// checkIsSystem asserts that PG contains exactly wantTotal rows for the
// session with ordinals 0..wantTotal-1, and that each row's is_system
// matches wantSystem. Tracking the exact ordinal set prevents false
// positives from wrong-but-equal-count row sets.
func checkIsSystem(
	t *testing.T,
	pg *sql.DB,
	sessID string,
	wantSystem map[int]bool,
	wantTotal int,
) {
	t.Helper()
	rows, err := pg.Query(
		`SELECT ordinal, is_system FROM messages
		 WHERE session_id = $1 ORDER BY ordinal`,
		sessID,
	)
	require.NoError(t, err, "querying PG messages")
	defer rows.Close()
	seen := make(map[int]bool, wantTotal)
	for rows.Next() {
		var ordinal int
		var isSystem bool
		require.NoError(t, rows.Scan(&ordinal, &isSystem), "scanning row")
		seen[ordinal] = true
		want := wantSystem[ordinal]
		assert.Equal(t, want, isSystem, "ordinal %d is_system", ordinal)
	}
	require.NoError(t, rows.Err(), "rows error")
	assert.Len(t, seen, wantTotal,
		"PG has %d message rows for session %s, want %d",
		len(seen), sessID, wantTotal)
	// Verify every expected ordinal was present (no gaps or substitutions).
	for i := range wantTotal {
		assert.True(t, seen[i], "ordinal %d missing from PG messages", i)
	}
}

func assertPGMessageContent(
	t *testing.T,
	pg *sql.DB,
	sessionID string,
	ordinal int,
	want string,
) {
	t.Helper()
	var got string
	require.NoError(t, pg.QueryRow(
		`SELECT content FROM messages
		  WHERE session_id = $1 AND ordinal = $2`,
		sessionID, ordinal,
	).Scan(&got), "read pg message content")
	assert.Equal(t, want, got)
}

func assertPGMessageThinking(
	t *testing.T,
	pg *sql.DB,
	sessionID string,
	ordinal int,
	wantHasThinking bool,
	wantThinkingText string,
) {
	t.Helper()
	var gotHasThinking bool
	var gotThinkingText string
	require.NoError(t, pg.QueryRow(
		`SELECT has_thinking, thinking_text FROM messages
		  WHERE session_id = $1 AND ordinal = $2`,
		sessionID, ordinal,
	).Scan(&gotHasThinking, &gotThinkingText), "read pg message thinking")
	assert.Equal(t, wantHasThinking, gotHasThinking)
	assert.Equal(t, wantThinkingText, gotThinkingText)
}

func assertPGMessageTimestamp(
	t *testing.T,
	pg *sql.DB,
	sessionID string,
	ordinal int,
	want string,
) {
	t.Helper()
	var gotTime sql.NullTime
	require.NoError(t, pg.QueryRow(
		`SELECT timestamp FROM messages
		  WHERE session_id = $1 AND ordinal = $2`,
		sessionID, ordinal,
	).Scan(&gotTime), "read pg message timestamp")
	require.True(t, gotTime.Valid, "timestamp should be non-NULL")
	assert.Equal(t, want, FormatISO8601(gotTime.Time))
}

func pgMessageCTID(
	t *testing.T,
	pg *sql.DB,
	sessionID string,
	ordinal int,
) string {
	t.Helper()
	var ctid string
	require.NoError(t, pg.QueryRow(
		`SELECT ctid::text FROM messages
		  WHERE session_id = $1 AND ordinal = $2`,
		sessionID, ordinal,
	).Scan(&ctid), "read pg message ctid")
	return ctid
}

// TestPushMessagesSanitizesNULBytes verifies that a message whose
// model and source fields carry NUL bytes (observed in production:
// the Antigravity gen_metadata heuristic persisted a raw protobuf
// fragment as the model name) pushes to PG without the whole-session
// rollback caused by SQLSTATE 22021 (invalid byte sequence for
// encoding "UTF8": 0x00). Model and source fields come from
// third-party session files, so the push boundary must be defensive
// regardless of any single parser fix.
func TestPushMessagesSanitizesNULBytes(t *testing.T) {
	pgURL := testPGURL(t)

	const schema = "agentsview_push_nul_test"
	pg, err := Open(pgURL, schema, true)
	require.NoError(t, err, "Open")
	defer pg.Close()

	ctx := context.Background()
	_, err = pg.Exec(`DROP SCHEMA IF EXISTS ` + schema + ` CASCADE`)
	require.NoError(t, err, "drop schema")
	require.NoError(t, EnsureSchema(ctx, pg, schema), "EnsureSchema")

	localDB, err := db.Open(
		filepath.Join(t.TempDir(), "local.db"),
	)
	require.NoError(t, err, "db.Open")
	defer localDB.Close()

	sync := &Sync{
		pg:         pg,
		local:      localDB,
		machine:    "test-machine",
		schema:     schema,
		schemaDone: true,
	}

	// The exact 10-byte protobuf fragment persisted as a model
	// name by the pre-fix Antigravity parser (hex
	// 080020022A0201024001, contains 0x00).
	badModel := "\x08\x00\x20\x02\x2a\x02\x01\x02\x40\x01"

	const sessID = "nul-bytes-001"
	sess := db.Session{
		ID:           sessID,
		Project:      "test-proj",
		Machine:      "test-machine",
		Agent:        "antigravity",
		MessageCount: 1,
		CreatedAt:    "2026-01-01T00:00:00Z",
		Cwd:          "/tmp/with\x00nul",
		GitBranch:    "main\x00",
	}
	require.NoError(t, localDB.UpsertSession(sess), "UpsertSession")

	msgs := []db.Message{{
		SessionID:     sessID,
		Ordinal:       0,
		Role:          "assistant",
		Content:       "thinking summary",
		ContentLength: 16,
		Model:         badModel,
		SourceUUID:    "uuid\x00tail",
	}}
	require.NoError(t, localDB.InsertMessages(msgs), "InsertMessages")

	res, err := sync.Push(ctx, false, nil)
	require.NoError(t, err, "Push")
	assert.Zero(t, res.Errors, "push should report no failed sessions")

	var model, sourceUUID string
	err = pg.QueryRow(
		`SELECT model, source_uuid FROM messages
		 WHERE session_id = $1 AND ordinal = 0`, sessID,
	).Scan(&model, &sourceUUID)
	require.NoError(t, err, "querying pushed message")
	assert.Equal(t, sanitizePG(badModel), model, "model stripped of NUL")
	assert.NotContains(t, model, "\x00")
	assert.Equal(t, "uuidtail", sourceUUID, "source_uuid stripped of NUL")

	// Second push with sync state cleared: the local token
	// fingerprint (sanitized, see db.SanitizeUTF8) must match the
	// PG-readback fingerprint despite the NUL bytes still stored
	// locally, so the metadata fast path skips the rewrite. ctid
	// changes on DELETE+reinsert, so an unchanged ctid proves the
	// row was left alone.
	var ctidBefore string
	require.NoError(t, pg.QueryRow(
		`SELECT ctid::text FROM messages
		 WHERE session_id = $1 AND ordinal = 0`, sessID,
	).Scan(&ctidBefore), "reading ctid before second push")

	require.NoError(t, localDB.SetSyncState("last_push_at", ""),
		"clearing last_push_at")
	require.NoError(t,
		localDB.SetSyncState(lastPushBoundaryStateKey, ""),
		"clearing boundary state")

	res, err = sync.Push(ctx, false, nil)
	require.NoError(t, err, "Push (second)")
	assert.Zero(t, res.Errors, "second push should report no failures")

	var ctidAfter string
	require.NoError(t, pg.QueryRow(
		`SELECT ctid::text FROM messages
		 WHERE session_id = $1 AND ordinal = 0`, sessID,
	).Scan(&ctidAfter), "reading ctid after second push")
	assert.Equal(t, ctidBefore, ctidAfter,
		"fast path should skip rewriting a NUL-field session")
}

// TestPushIncrementalWithOnlyForeignMachineSessions verifies reset detection
// does not misfire when every local session carries a machine value other than
// s.machine (e.g. orphan-copied sessions kept from a previous machine name).
// The push marker, not a per-machine row count, drives reset detection, so the
// second incremental push is a no-op rather than a forced full re-push.
func TestPushIncrementalWithOnlyForeignMachineSessions(t *testing.T) {
	pgURL := testPGURL(t)

	const schema = "agentsview_push_foreign_machine_test"
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

	const sessID = "foreign-machine-001"
	sess := db.Session{
		ID:           sessID,
		Project:      "proj",
		Machine:      "old-host",
		Agent:        "claude",
		MessageCount: 1,
		CreatedAt:    "2026-01-01T00:00:00Z",
	}
	require.NoError(t, localDB.UpsertSession(sess), "UpsertSession")
	require.NoError(t, localDB.InsertMessages([]db.Message{{
		SessionID:     sessID,
		Ordinal:       0,
		Role:          "assistant",
		Content:       "hello",
		ContentLength: 5,
	}}), "InsertMessages")

	res, err := sync.Push(ctx, false, nil)
	require.NoError(t, err, "first Push")
	assert.Zero(t, res.Errors, "first push should report no failures")

	var machine string
	require.NoError(t, pg.QueryRow(
		`SELECT machine FROM sessions WHERE id = $1`, sessID,
	).Scan(&machine), "reading pushed machine")
	require.Equal(t, "old-host", machine, "source machine preserved")

	var ctidBefore string
	require.NoError(t, pg.QueryRow(
		`SELECT ctid::text FROM messages
		 WHERE session_id = $1 AND ordinal = 0`, sessID,
	).Scan(&ctidBefore), "reading ctid before second push")

	// Second incremental push with sync state intact. Reset detection counts
	// machine = "new-host" plus "old-host", finds the row, and the fingerprint
	// fast path skips the session entirely; the unchanged ctid proves the
	// message row was left alone instead of being rewritten by a forced full.
	res, err = sync.Push(ctx, false, nil)
	require.NoError(t, err, "second Push")
	assert.Zero(t, res.Errors, "second push should report no failures")
	assert.Zero(t, res.SessionsPushed,
		"unchanged foreign-machine session should not be re-pushed")

	var ctidAfter string
	require.NoError(t, pg.QueryRow(
		`SELECT ctid::text FROM messages
		 WHERE session_id = $1 AND ordinal = 0`, sessID,
	).Scan(&ctidAfter), "reading ctid after second push")
	assert.Equal(t, ctidBefore, ctidAfter,
		"second incremental push must not rewrite the session")
}

// TestPushDetectsResetWhenCompetingMachineRowsExist verifies that a PG reset is
// detected even when another pusher has repopulated rows under a machine value
// this host also writes. The local session carries Machine "remote-host" (as a
// remote host's sessions synced in over SSH would); after the first push the PG
// rows and this host's push marker are removed and a competing "remote-host"
// row is inserted, simulating the remote host re-pushing first after a shared
// PG reset. A machine-count check would see the competing row and skip the full
// push, leaving this host's session missing; the push marker is per-pusher, so
// the reset is detected and the session is re-pushed.
func TestPushDetectsResetWhenCompetingMachineRowsExist(t *testing.T) {
	pgURL := testPGURL(t)

	const schema = "agentsview_push_reset_competing_test"
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
		machine:    "this-host",
		schema:     schema,
		schemaDone: true,
	}

	const sessID = "remote-host~sess-1"
	require.NoError(t, localDB.UpsertSession(db.Session{
		ID:           sessID,
		Project:      "proj",
		Machine:      "remote-host",
		Agent:        "claude",
		MessageCount: 1,
		CreatedAt:    "2026-01-01T00:00:00Z",
	}), "UpsertSession")
	require.NoError(t, localDB.InsertMessages([]db.Message{{
		SessionID:     sessID,
		Ordinal:       0,
		Role:          "assistant",
		Content:       "hello",
		ContentLength: 5,
	}}), "InsertMessages")

	res, err := sync.Push(ctx, false, nil)
	require.NoError(t, err, "first Push")
	assert.Zero(t, res.Errors, "first push should report no failures")

	// Simulate a PG reset where the real remote host re-pushed first: drop this
	// host's rows and its push marker, then insert a competing "remote-host"
	// row under a different id.
	_, err = pg.Exec(`DELETE FROM sessions WHERE id = $1`, sessID)
	require.NoError(t, err, "delete pushed session")
	_, err = pg.Exec(
		`DELETE FROM sync_metadata WHERE key LIKE 'push_marker:%'`,
	)
	require.NoError(t, err, "delete push marker")
	_, err = pg.Exec(
		`INSERT INTO sessions (id, machine, project, agent, created_at)
		 VALUES ('remote-host-native-1', 'remote-host', 'proj', 'claude', NOW())`,
	)
	require.NoError(t, err, "insert competing remote-host row")

	res, err = sync.Push(ctx, false, nil)
	require.NoError(t, err, "second Push")
	assert.Zero(t, res.Errors, "second push should report no failures")

	var exists bool
	require.NoError(t, pg.QueryRow(
		`SELECT EXISTS (SELECT 1 FROM sessions WHERE id = $1)`, sessID,
	).Scan(&exists), "checking re-pushed session")
	assert.True(t, exists,
		"reset must be detected and this host's session re-pushed")
}

// TestPushMarkerNotWrittenWhenResetRecoveryFails verifies the push marker is
// written only after a push finalizes. When a reset is detected but the
// recovery push fails before finalization, the marker must stay absent so the
// next push re-detects the reset; otherwise the local watermark would remain at
// the old value while PG holds a fresh marker, and reset-lost sessions would be
// skipped indefinitely.
func TestPushMarkerNotWrittenWhenResetRecoveryFails(t *testing.T) {
	pgURL := testPGURL(t)

	const schema = "agentsview_push_reset_recovery_fail_test"
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
		machine:    "this-host",
		schema:     schema,
		schemaDone: true,
	}

	const sessID = "reset-recovery-1"
	require.NoError(t, localDB.UpsertSession(db.Session{
		ID:           sessID,
		Project:      "proj",
		Machine:      "this-host",
		Agent:        "claude",
		MessageCount: 1,
		CreatedAt:    "2026-01-01T00:00:00Z",
	}), "UpsertSession")
	require.NoError(t, localDB.InsertMessages([]db.Message{{
		SessionID:     sessID,
		Ordinal:       0,
		Role:          "assistant",
		Content:       "hello",
		ContentLength: 5,
	}}), "InsertMessages")

	res, err := sync.Push(ctx, false, nil)
	require.NoError(t, err, "first Push")
	assert.Zero(t, res.Errors, "first push should report no failures")

	markerCount := func() int {
		var n int
		require.NoError(t, pg.QueryRow(
			`SELECT COUNT(*) FROM sync_metadata
			 WHERE key LIKE 'push_marker:%'`,
		).Scan(&n), "counting push markers")
		return n
	}
	require.Equal(t, 1, markerCount(), "marker present after first push")

	// Simulate a PG reset: drop this host's row and marker, keeping the local
	// watermark and boundary state so the session would otherwise be skipped.
	_, err = pg.Exec(`DELETE FROM sessions WHERE id = $1`, sessID)
	require.NoError(t, err, "delete pushed session")
	_, err = pg.Exec(
		`DELETE FROM sync_metadata WHERE key LIKE 'push_marker:%'`,
	)
	require.NoError(t, err, "delete push marker")
	require.Equal(t, 0, markerCount(), "marker cleared for reset simulation")

	// Sabotage the recovery push so it fails after reset detection but before
	// finalization: drop a model_pricing column syncModelPricing reads. The
	// reset branch re-runs EnsureSchema, but CREATE TABLE IF NOT EXISTS does
	// not re-add a column to an existing table, so the failure persists.
	_, err = pg.Exec(
		`ALTER TABLE model_pricing DROP COLUMN cache_read_per_mtok`,
	)
	require.NoError(t, err, "drop model_pricing column")

	_, err = sync.Push(ctx, false, nil)
	require.Error(t, err, "recovery push should fail at model pricing sync")
	assert.Equal(t, 0, markerCount(),
		"marker must not be written when recovery push fails")

	// Repair the column; the next push must re-detect the reset (marker still
	// absent) and re-push the session.
	_, err = pg.Exec(
		`ALTER TABLE model_pricing
		 ADD COLUMN cache_read_per_mtok DOUBLE PRECISION NOT NULL DEFAULT 0`,
	)
	require.NoError(t, err, "restore model_pricing column")

	res, err = sync.Push(ctx, false, nil)
	require.NoError(t, err, "recovery push after repair")
	assert.Zero(t, res.Errors, "repaired push should report no failures")

	var exists bool
	require.NoError(t, pg.QueryRow(
		`SELECT EXISTS (SELECT 1 FROM sessions WHERE id = $1)`, sessID,
	).Scan(&exists), "checking re-pushed session")
	assert.True(t, exists, "session must be re-pushed after reset recovery")
	assert.Equal(t, 1, markerCount(), "marker restored after successful push")
}

// TestPushUpdatesSentinelMachineWhenSyncMachineChanges verifies that a session
// stored with the "local" sentinel machine is re-pushed under the new fallback
// when Sync.machine changes, rather than being skipped by a fingerprint that
// ignored the resolved machine. The second push clears the local watermark so
// the session is re-evaluated; without the resolved machine in the fingerprint
// it would match and be skipped, leaving PG with the stale machine name.
func TestPushUpdatesSentinelMachineWhenSyncMachineChanges(t *testing.T) {
	pgURL := testPGURL(t)

	const schema = "agentsview_push_sentinel_machine_test"
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

	const sessID = "sentinel-machine-1"
	require.NoError(t, localDB.UpsertSession(db.Session{
		ID:           sessID,
		Project:      "proj",
		Machine:      "local",
		Agent:        "claude",
		MessageCount: 1,
		CreatedAt:    "2026-01-01T00:00:00Z",
	}), "UpsertSession")
	require.NoError(t, localDB.InsertMessages([]db.Message{{
		SessionID:     sessID,
		Ordinal:       0,
		Role:          "assistant",
		Content:       "hello",
		ContentLength: 5,
	}}), "InsertMessages")

	res, err := sync.Push(ctx, false, nil)
	require.NoError(t, err, "first Push")
	assert.Zero(t, res.Errors, "first push should report no failures")

	machine := func() string {
		var m string
		require.NoError(t, pg.QueryRow(
			`SELECT machine FROM sessions WHERE id = $1`, sessID,
		).Scan(&m), "reading machine")
		return m
	}
	require.Equal(t, "host-a", machine(), "sentinel pushed under host-a")

	// Rename: change the fallback machine and re-evaluate the session by
	// clearing the watermark, mirroring any path that re-lists it.
	sync.machine = "host-b"
	require.NoError(t, localDB.SetSyncState("last_push_at", ""),
		"clearing last_push_at")

	res, err = sync.Push(ctx, false, nil)
	require.NoError(t, err, "second Push")
	assert.Zero(t, res.Errors, "second push should report no failures")
	assert.Equal(t, "host-b", machine(),
		"sentinel machine must follow the new fallback")
}

func TestPushAdoptsOwnerlessRowsFromPreviousMarkerMachine(t *testing.T) {
	pgURL := testPGURL(t)

	const schema = "agentsview_push_legacy_marker_machine_test"
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

	const markerID = "legacy-marker-1"
	require.NoError(t, localDB.SetSyncState("pg_push_marker_id", markerID),
		"seed local push marker")

	sync := &Sync{
		pg:         pg,
		local:      localDB,
		machine:    "host-b",
		schema:     schema,
		schemaDone: true,
	}

	const sessID = "legacy-previous-machine-1"
	require.NoError(t, localDB.UpsertSession(db.Session{
		ID:           sessID,
		Project:      "proj",
		Machine:      "host-b",
		Agent:        "claude",
		MessageCount: 1,
		CreatedAt:    "2026-01-01T00:00:00Z",
	}), "UpsertSession")
	require.NoError(t, localDB.InsertMessages([]db.Message{{
		SessionID:     sessID,
		Ordinal:       0,
		Role:          "assistant",
		Content:       "hello",
		ContentLength: 5,
	}}), "InsertMessages")

	_, err = pg.ExecContext(ctx, `
		INSERT INTO sync_metadata (key, value)
		VALUES ($1, $2)
	`, pushMarkerKeyPrefix+markerID, "host-a")
	require.NoError(t, err, "seed previous marker machine")
	_, err = pg.ExecContext(ctx, `
		INSERT INTO sessions (
			id, machine, owner_marker, project, agent, created_at
		) VALUES ($1, $2, $3, $4, $5, NOW())
	`, sessID, "host-a", "", "proj", "claude")
	require.NoError(t, err, "seed ownerless legacy session")

	res, err := sync.Push(ctx, true, nil)
	require.NoError(t, err, "Push")
	assert.Zero(t, res.Errors, "push should report no failed sessions")
	assert.Zero(t, res.SkippedConflicts,
		"previous-marker-machine legacy row should be adopted")
	assert.Equal(t, 1, res.SessionsPushed,
		"legacy row should be counted as pushed")

	var machine, ownerMarker string
	require.NoError(t, pg.QueryRow(
		`SELECT machine, owner_marker FROM sessions WHERE id = $1`, sessID,
	).Scan(&machine, &ownerMarker), "reading adopted row")
	assert.Equal(t, "host-b", machine)
	assert.Equal(t, markerID, ownerMarker)

	var markerMachine string
	require.NoError(t, pg.QueryRow(
		`SELECT value FROM sync_metadata WHERE key = $1`,
		pushMarkerKeyPrefix+markerID,
	).Scan(&markerMachine), "reading marker machine")
	assert.Equal(t, "host-b", markerMachine)

	var aliases string
	require.NoError(t, pg.QueryRow(
		`SELECT value FROM sync_metadata WHERE key = $1`,
		pushMarkerMachineAliasesKeyPrefix+markerID,
	).Scan(&aliases), "reading marker aliases")
	assert.JSONEq(t, `["host-a"]`, aliases)

	const laterSessID = "legacy-previous-machine-2"
	require.NoError(t, localDB.UpsertSession(db.Session{
		ID:           laterSessID,
		Project:      "proj",
		Machine:      "host-b",
		Agent:        "claude",
		MessageCount: 1,
		CreatedAt:    "2026-01-01T00:00:00Z",
	}), "UpsertSession later legacy row")
	require.NoError(t, localDB.InsertMessages([]db.Message{{
		SessionID:     laterSessID,
		Ordinal:       0,
		Role:          "assistant",
		Content:       "later",
		ContentLength: 5,
	}}), "InsertMessages later legacy row")
	_, err = pg.ExecContext(ctx, `
		INSERT INTO sessions (
			id, machine, owner_marker, project, agent, created_at
		) VALUES ($1, $2, $3, $4, $5, NOW())
	`, laterSessID, "host-a", "", "proj", "claude")
	require.NoError(t, err, "seed later ownerless legacy session")

	res, err = sync.Push(ctx, true, nil)
	require.NoError(t, err, "second Push")
	assert.Zero(t, res.Errors, "second push should report no failed sessions")
	assert.Zero(t, res.SkippedConflicts,
		"preserved marker alias should adopt later legacy row")

	require.NoError(t, pg.QueryRow(
		`SELECT machine, owner_marker FROM sessions WHERE id = $1`,
		laterSessID,
	).Scan(&machine, &ownerMarker), "reading later adopted row")
	assert.Equal(t, "host-b", machine)
	assert.Equal(t, markerID, ownerMarker)
}

func TestFilteredPushAdoptsOwnerlessRowsFromLegacyUnscopedMarker(t *testing.T) {
	pgURL := testPGURL(t)

	const schema = "agentsview_push_filtered_legacy_marker_test"
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

	const markerID = "legacy-marker-filtered-1"
	require.NoError(t, localDB.SetSyncState("pg_push_marker_id", markerID),
		"seed local push marker")

	projects := []string{"proj"}
	scope := pushSyncStateScope("team-target", projects, nil)
	sync := &Sync{
		pg:              pg,
		local:           localDB,
		syncState:       newScopedSyncStateStore(localDB, scope, false),
		machine:         "host-b",
		schema:          schema,
		schemaDone:      true,
		syncStateTarget: scope,
		projects:        projects,
	}
	require.NoError(t, sync.effectiveSyncState().SetSyncState(
		"last_push_at", "2025-12-31T00:00:00.000Z",
	), "seed scoped local push state")

	const sessID = "legacy-filtered-previous-machine-1"
	require.NoError(t, localDB.UpsertSession(db.Session{
		ID:           sessID,
		Project:      "proj",
		Machine:      "host-b",
		Agent:        "claude",
		MessageCount: 1,
		CreatedAt:    "2026-01-01T00:00:00Z",
	}), "UpsertSession")
	require.NoError(t, localDB.InsertMessages([]db.Message{{
		SessionID:     sessID,
		Ordinal:       0,
		Role:          "assistant",
		Content:       "hello",
		ContentLength: 5,
	}}), "InsertMessages")

	_, err = pg.ExecContext(ctx, `
		INSERT INTO sync_metadata (key, value)
		VALUES ($1, $2)
	`, pushMarkerKeyPrefix+markerID, "host-a")
	require.NoError(t, err, "seed legacy unscoped marker machine")
	_, err = pg.ExecContext(ctx, `
		INSERT INTO sessions (
			id, machine, owner_marker, project, agent, created_at
		) VALUES ($1, $2, $3, $4, $5, NOW())
	`, sessID, "host-a", "", "proj", "claude")
	require.NoError(t, err, "seed ownerless legacy session")

	res, err := sync.Push(ctx, false, nil)
	require.NoError(t, err, "Push")
	assert.Zero(t, res.Errors, "push should report no failed sessions")
	assert.Zero(t, res.SkippedConflicts,
		"legacy unscoped marker machine should be used as an alias")
	assert.Equal(t, 1, res.SessionsPushed,
		"legacy row should be counted as pushed")

	var machine, ownerMarker string
	require.NoError(t, pg.QueryRow(
		`SELECT machine, owner_marker FROM sessions WHERE id = $1`, sessID,
	).Scan(&machine, &ownerMarker), "reading adopted row")
	assert.Equal(t, "host-b", machine)
	assert.Equal(t, markerID, ownerMarker)

	var markerMachine string
	require.NoError(t, pg.QueryRow(
		`SELECT value FROM sync_metadata WHERE key = $1`,
		sync.pushMarkerMetadataKey(pushMarkerKeyPrefix, markerID),
	).Scan(&markerMachine), "reading scoped marker machine")
	assert.Equal(t, "host-b", markerMachine)

	var aliases string
	require.NoError(t, pg.QueryRow(
		`SELECT value FROM sync_metadata WHERE key = $1`,
		sync.pushMarkerMetadataKey(
			pushMarkerMachineAliasesKeyPrefix, markerID,
		),
	).Scan(&aliases), "reading scoped marker aliases")
	assert.JSONEq(t, `["host-a"]`, aliases)
}

func TestPushReportsSkippedConflicts(t *testing.T) {
	pgURL := testPGURL(t)

	const schema = "agentsview_push_skipped_conflicts_test"
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

	const sessID = "conflict-001"
	require.NoError(t, localDB.UpsertSession(db.Session{
		ID:           sessID,
		Project:      "proj",
		Machine:      "machine-b",
		Agent:        "claude",
		MessageCount: 1,
		CreatedAt:    "2026-01-01T00:00:00Z",
	}), "UpsertSession")
	require.NoError(t, localDB.InsertMessages([]db.Message{{
		SessionID:     sessID,
		Ordinal:       0,
		Role:          "assistant",
		Content:       "hello",
		ContentLength: 5,
	}}), "InsertMessages")

	_, err = pg.ExecContext(ctx, `
		INSERT INTO sessions (
			id, machine, owner_marker, project, agent, created_at
		) VALUES ($1, $2, $3, $4, $5, NOW())
	`, sessID, "machine-a", "other-owner", "proj", "claude")
	require.NoError(t, err, "insert conflicting owner")

	res, err := sync.Push(ctx, false, nil)
	require.NoError(t, err, "Push")
	assert.Zero(t, res.Errors, "push should not report failed sessions")
	assert.Zero(t, res.SessionsPushed, "conflicting session should not be counted as pushed")
	assert.Equal(t, 1, res.SkippedConflicts, "skipped conflicts should be observable in PushResult")
}
