//go:build pgtest

package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/artifact"
	"go.kenn.io/agentsview/internal/db"
)

func openArtifactTestRepository(t *testing.T, ctx context.Context) *artifact.Repository {
	t.Helper()
	repository, err := artifact.OpenRepository(ctx, t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, repository.Close()) })
	return repository
}

func importExportedArtifactCheckpoint(
	t *testing.T,
	ctx context.Context,
	database *db.DB,
	repository *artifact.Repository,
	sourceOrigin string,
	localOrigin string,
	export artifact.ExportResult,
) artifact.ImportResult {
	t.Helper()
	ref, err := artifact.NewRef(sourceOrigin, artifact.KindCheckpoints,
		fmt.Sprintf("cp-%010d.json", export.CheckpointSequence))
	require.NoError(t, err)
	entry, err := repository.Content().Stat(ctx, ref)
	require.NoError(t, err)
	coordinator := artifact.NewStoreImportCoordinator(database, repository.Content(), localOrigin)
	require.NoError(t, coordinator.RecordChanged(ctx, entry))
	result, err := coordinator.Finalize(ctx)
	require.NoError(t, err)
	return result
}

func TestPushArtifactNativeAndImportedCopiesShareOriginIdentity(t *testing.T) {
	pgURL := testPGURL(t)
	ctx := context.Background()
	const originA = "origin-a1b2c3"
	const originB = "origin-b4c5d6"
	const nativeID = "native-id"
	const canonicalID = originA + "~" + nativeID
	const childID = "child-id"
	const canonicalChildID = originA + "~" + childID

	for _, tc := range []struct {
		name          string
		schema        string
		importerFirst bool
	}{
		{
			name:   "origin pushes first",
			schema: "agentsview_artifact_identity_origin_first_test",
		},
		{
			name:          "importer pushes first",
			schema:        "agentsview_artifact_identity_importer_first_test",
			importerFirst: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pg, err := Open(pgURL, tc.schema, true)
			require.NoError(t, err, "Open")
			t.Cleanup(func() { require.NoError(t, pg.Close()) })
			_, err = pg.Exec(`DROP SCHEMA IF EXISTS ` + tc.schema + ` CASCADE`)
			require.NoError(t, err, "drop schema")
			require.NoError(t, EnsureSchema(ctx, pg, tc.schema), "EnsureSchema")

			originDB, err := db.Open(filepath.Join(t.TempDir(), "origin.db"))
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, originDB.Close()) })
			require.NoError(t, artifact.AdoptOrigin(originDB, originA))
			require.NoError(t, originDB.UpsertSession(db.Session{
				ID: nativeID, Project: "alpha", Machine: "local", Agent: "claude",
				MessageCount: 1, UserMessageCount: 1,
				CreatedAt: "2026-01-01T00:00:00Z",
			}))
			require.NoError(t, originDB.ReplaceSessionMessages(nativeID, []db.Message{{
				SessionID: nativeID, Ordinal: 0, Role: "user",
				Content: "hello", ContentLength: 5,
			}}))
			parentID := nativeID
			require.NoError(t, originDB.UpsertSession(db.Session{
				ID: childID, Project: "alpha", Machine: "local", Agent: "claude",
				MessageCount: 1, UserMessageCount: 1,
				CreatedAt: "2026-01-01T00:00:01Z", ParentSessionID: &parentID,
			}))
			require.NoError(t, originDB.ReplaceSessionMessages(childID, []db.Message{{
				SessionID: childID, Ordinal: 0, Role: "user",
				Content: "child", ContentLength: 5,
			}}))

			artifactRepository := openArtifactTestRepository(t, ctx)
			exportResult, err := artifact.ExportToStore(ctx, originDB, artifactRepository.Content(), artifact.ExportOptions{
				Origin: originA,
				Full:   true,
			})
			require.NoError(t, err)
			importerDB, err := db.Open(filepath.Join(t.TempDir(), "importer.db"))
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, importerDB.Close()) })
			require.NoError(t, artifact.AdoptOrigin(importerDB, originB))
			importResult := importExportedArtifactCheckpoint(
				t, ctx, importerDB, artifactRepository, originA, originB, exportResult,
			)
			require.Equal(t, 2, importResult.Sessions)
			require.Equal(t, 2, importResult.Messages)

			// Advance the origin after the importer captured its artifact snapshot.
			// An origin-first push must not be rolled back when that stale replica
			// subsequently pushes the same canonical owner marker.
			require.NoError(t, originDB.UpsertSession(db.Session{
				ID: nativeID, Project: "fresh-origin", Machine: "local", Agent: "claude",
				MessageCount: 1, UserMessageCount: 1,
				CreatedAt: "2026-01-01T00:00:00Z",
			}))
			require.NoError(t, originDB.ReplaceSessionMessages(nativeID, []db.Message{{
				SessionID: nativeID, Ordinal: 0, Role: "user",
				Content: "fresh", ContentLength: 5,
			}}))

			originSync := &Sync{
				pg: pg, local: originDB, machine: "host-a",
				schema: tc.schema, schemaDone: true,
			}
			importerSync := &Sync{
				pg: pg, local: importerDB, machine: "host-b",
				schema: tc.schema, schemaDone: true,
			}
			pushers := []*Sync{originSync, importerSync}
			if tc.importerFirst {
				pushers[0], pushers[1] = pushers[1], pushers[0]
			}
			for _, pusher := range pushers {
				result, pushErr := pusher.Push(ctx, false, nil)
				require.NoError(t, pushErr)
				assert.Zero(t, result.Errors)
				assert.Zero(t, result.SkippedConflicts)
			}

			var id, machine, ownerMarker string
			err = pg.QueryRowContext(ctx, `
				SELECT id, machine, owner_marker
				FROM sessions
				WHERE id IN ($1, $2)
			`, nativeID, canonicalID).Scan(&id, &machine, &ownerMarker)
			require.NoError(t, err)
			assert.Equal(t, canonicalID, id)
			assert.Equal(t, originA, machine)
			assert.Equal(t, artifactOwnerMarkerPrefix+originA, ownerMarker)
			var project, content string
			require.NoError(t, pg.QueryRowContext(ctx, `
				SELECT s.project, m.content
				FROM sessions s
				JOIN messages m ON m.session_id = s.id
				WHERE s.id = $1 AND m.ordinal = 0
			`, canonicalID).Scan(&project, &content))
			assert.Equal(t, "fresh-origin", project)
			assert.Equal(t, "fresh", content)
			var parent string
			require.NoError(t, pg.QueryRowContext(ctx, `
				SELECT parent_session_id FROM sessions WHERE id = $1
			`, canonicalChildID).Scan(&parent))
			assert.Equal(t, canonicalID, parent,
				"artifact relationships must resolve through the canonical origin identity")

			for _, pusher := range []*Sync{importerSync, originSync} {
				result, pushErr := pusher.Push(ctx, true, nil)
				require.NoError(t, pushErr)
				assert.Zero(t, result.Errors)
				assert.Zero(t, result.SkippedConflicts)
			}
			var count int
			require.NoError(t, pg.QueryRowContext(ctx, `
				SELECT COUNT(*) FROM sessions WHERE id IN ($1, $2)
			`, nativeID, canonicalID).Scan(&count))
			assert.Equal(t, 1, count,
				"native and imported copies must keep one PG row across repeated pushes")
		})
	}
}

func TestPushSSHShapedSessionDoesNotAdoptArtifactOwnership(t *testing.T) {
	pgURL := testPGURL(t)
	ctx := context.Background()
	const schema = "agentsview_artifact_provenance_guard_test"
	const remoteOrigin = "origin-a1b2c3"
	const localOrigin = "origin-b4c5d6"
	const sessionID = remoteOrigin + "~native-id"

	pg, err := Open(pgURL, schema, true)
	require.NoError(t, err, "Open")
	t.Cleanup(func() { require.NoError(t, pg.Close()) })
	_, err = pg.Exec(`DROP SCHEMA IF EXISTS ` + schema + ` CASCADE`)
	require.NoError(t, err, "drop schema")
	require.NoError(t, EnsureSchema(ctx, pg, schema), "EnsureSchema")

	localDB, err := db.Open(filepath.Join(t.TempDir(), "local.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, localDB.Close()) })
	require.NoError(t, artifact.AdoptOrigin(localDB, localOrigin))
	require.NoError(t, localDB.UpsertSession(db.Session{
		ID: sessionID, Project: "ssh-copy", Machine: remoteOrigin, Agent: "claude",
		MessageCount: 1, UserMessageCount: 1,
		CreatedAt: "2026-01-01T00:00:00Z",
	}))
	require.NoError(t, localDB.ReplaceSessionMessages(sessionID, []db.Message{{
		SessionID: sessionID, Ordinal: 0, Role: "user",
		Content: "ssh copy", ContentLength: 8,
	}}))

	_, err = pg.ExecContext(ctx, `
		INSERT INTO sessions (
			id, machine, owner_marker, project, agent, created_at
		) VALUES ($1, $2, $3, $4, $5, NOW())
	`, sessionID, remoteOrigin, artifactOwnerMarkerPrefix+remoteOrigin,
		"genuine-artifact", "claude")
	require.NoError(t, err, "seed genuine artifact-owned row")

	syncer := &Sync{
		pg: pg, local: localDB, machine: "ssh-importer",
		schema: schema, schemaDone: true,
	}
	result, err := syncer.Push(ctx, false, nil)
	require.NoError(t, err)
	assert.Equal(t, 1, result.SkippedConflicts,
		"an SSH-shaped row without artifact import provenance must retain legacy collision protection")
	assert.Zero(t, result.SessionsPushed)

	var project, ownerMarker string
	require.NoError(t, pg.QueryRowContext(ctx, `
		SELECT project, owner_marker FROM sessions WHERE id = $1
	`, sessionID).Scan(&project, &ownerMarker))
	assert.Equal(t, "genuine-artifact", project)
	assert.Equal(t, artifactOwnerMarkerPrefix+remoteOrigin, ownerMarker)
}

func TestPushArtifactOriginAdoptionReusesLegacyBareRows(t *testing.T) {
	pgURL := testPGURL(t)
	ctx := context.Background()
	const schema = "agentsview_artifact_origin_upgrade_test"
	const originA = "origin-a1b2c3"
	const originB = "origin-b4c5d6"
	const parentID = "native-parent"
	const childID = "native-child"
	const canonicalParentID = originA + "~" + parentID
	const canonicalChildID = originA + "~" + childID
	const stateScope = "work"

	pg, err := Open(pgURL, schema, true)
	require.NoError(t, err, "Open")
	t.Cleanup(func() { require.NoError(t, pg.Close()) })
	_, err = pg.Exec(`DROP SCHEMA IF EXISTS ` + schema + ` CASCADE`)
	require.NoError(t, err, "drop schema")
	require.NoError(t, EnsureSchema(ctx, pg, schema), "EnsureSchema")

	originDB, err := db.Open(filepath.Join(t.TempDir(), "origin.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, originDB.Close()) })
	require.NoError(t, originDB.UpsertSession(db.Session{
		ID: parentID, Project: "alpha", Machine: "local", Agent: "claude",
		MessageCount: 1, UserMessageCount: 1,
		CreatedAt: "2026-01-01T00:00:00Z",
	}))
	require.NoError(t, originDB.ReplaceSessionMessages(parentID, []db.Message{{
		SessionID: parentID, Ordinal: 0, Role: "user",
		Content: "parent", ContentLength: 6, SourceUUID: "parent-source",
	}}))
	parentRef := parentID
	require.NoError(t, originDB.UpsertSession(db.Session{
		ID: childID, Project: "alpha", Machine: "local", Agent: "claude",
		MessageCount: 1, UserMessageCount: 1,
		CreatedAt: "2026-01-01T00:00:01Z", ParentSessionID: &parentRef,
	}))
	require.NoError(t, originDB.ReplaceSessionMessages(childID, []db.Message{{
		SessionID: childID, Ordinal: 0, Role: "user",
		Content: "child", ContentLength: 5, SourceUUID: "child-source",
	}}))

	originSync := &Sync{
		pg: pg, local: originDB, machine: "host-a",
		schema: schema, schemaDone: true,
		syncState:       newScopedSyncStateStore(originDB, stateScope, false),
		syncStateTarget: stateScope,
	}
	first, err := originSync.Push(ctx, false, nil)
	require.NoError(t, err)
	require.Equal(t, 2, first.SessionsPushed)
	watermark, err := originDB.GetSyncState("last_push_at:" + stateScope)
	require.NoError(t, err)
	require.NotEmpty(t, watermark, "precondition: initial push established incremental state")
	// Simulate a pusher upgraded from a build predating identity-mode state.
	require.NoError(t, originDB.SetSyncState(
		"pg_artifact_identity_v1:"+stateScope, "",
	))

	_, err = pg.ExecContext(ctx,
		`UPDATE sessions SET display_name = 'PG title' WHERE id = $1`,
		parentID,
	)
	require.NoError(t, err, "seed PG-local display name")
	_, err = pg.ExecContext(ctx,
		`INSERT INTO starred_sessions (session_id) VALUES ($1)`,
		parentID,
	)
	require.NoError(t, err, "seed PG-local star")
	_, err = pg.ExecContext(ctx, `
		INSERT INTO pinned_messages (
			session_id, message_id, ordinal, source_uuid, note
		)
		SELECT $1, ordinal, ordinal, COALESCE(source_uuid, ''), 'PG pin'
		FROM messages WHERE session_id = $1 AND ordinal = 0
	`, parentID)
	require.NoError(t, err, "seed PG-local pin")

	before, err := originDB.GetSessionFull(ctx, parentID)
	require.NoError(t, err)
	require.NotNil(t, before)
	require.NoError(t, artifact.AdoptOrigin(originDB, originA))
	afterAdopt, err := originDB.GetSessionFull(ctx, parentID)
	require.NoError(t, err)
	require.NotNil(t, afterAdopt)
	assert.Equal(t, before.LocalModifiedAt, afterAdopt.LocalModifiedAt,
		"origin adoption must not need to mutate session timestamps")

	upgraded, err := originSync.Push(ctx, false, nil)
	require.NoError(t, err)
	assert.Equal(t, 2, upgraded.SessionsPushed,
		"identity-mode change must revisit unchanged sessions")
	assert.Zero(t, upgraded.Errors)
	assert.Zero(t, upgraded.SkippedConflicts)

	var stableBareRows int
	require.NoError(t, pg.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM sessions
		WHERE id IN ($1, $2) AND machine = $3 AND owner_marker = $4
	`, parentID, childID, originA, artifactOwnerMarkerPrefix+originA).Scan(&stableBareRows))
	assert.Equal(t, 2, stableBareRows,
		"same-owner legacy bare rows must upgrade in place to stable artifact ownership")
	var canonicalRows int
	require.NoError(t, pg.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM sessions WHERE id IN ($1, $2)
	`, canonicalParentID, canonicalChildID).Scan(&canonicalRows))
	assert.Zero(t, canonicalRows, "upgrade must not migrate primary keys or duplicate rows")
	assert.Equal(t, parentID, pgParentSessionID(t, ctx, pg, childID))
	assertPGArtifactUpgradeCuration(t, ctx, pg, parentID)
	mode, err := originDB.GetSyncState("pg_artifact_identity_v1:" + stateScope)
	require.NoError(t, err)
	assert.Equal(t, artifactOwnerMarkerPrefix+originA, mode,
		"successful push must persist the target-scoped identity mode")

	artifactRepository := openArtifactTestRepository(t, ctx)
	exportResult, err := artifact.ExportToStore(ctx, originDB, artifactRepository.Content(), artifact.ExportOptions{
		Origin: originA,
		Full:   true,
	})
	require.NoError(t, err)
	importerDB, err := db.Open(filepath.Join(t.TempDir(), "importer.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, importerDB.Close()) })
	require.NoError(t, artifact.AdoptOrigin(importerDB, originB))
	artifactImportResult := importExportedArtifactCheckpoint(
		t, ctx, importerDB, artifactRepository, originA, originB, exportResult,
	)
	require.Equal(t, 2, artifactImportResult.Sessions)
	require.Equal(t, 2, artifactImportResult.Messages)

	importerSync := &Sync{
		pg: pg, local: importerDB, machine: "host-b",
		schema: schema, schemaDone: true,
		syncState:       newScopedSyncStateStore(importerDB, stateScope, false),
		syncStateTarget: stateScope,
	}
	importResult, err := importerSync.Push(ctx, false, nil)
	require.NoError(t, err)
	assert.Zero(t, importResult.Errors)
	assert.Zero(t, importResult.SkippedConflicts)
	require.NoError(t, pg.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM sessions
		WHERE id IN ($1, $2, $3, $4)
	`, parentID, childID, canonicalParentID, canonicalChildID).Scan(&stableBareRows))
	assert.Equal(t, 2, stableBareRows,
		"an imported copy must reuse the stable legacy bare aliases")
	assert.Equal(t, parentID, pgParentSessionID(t, ctx, pg, childID))
	assertPGArtifactUpgradeCuration(t, ctx, pg, parentID)

	_, err = pg.ExecContext(ctx, `
		UPDATE sessions SET parent_session_id = $1 WHERE id = $2
	`, canonicalParentID, childID)
	require.NoError(t, err, "seed stale canonical relationship")
	time.Sleep(5 * time.Millisecond)
	name := "Imported child"
	require.NoError(t, importerDB.RenameSession(canonicalChildID, &name))
	replicaResult, err := importerSync.Push(ctx, false, nil)
	require.NoError(t, err)
	assert.Zero(t, replicaResult.SessionsPushed,
		"an imported replica must not update an existing canonical alias")
	assert.Zero(t, replicaResult.SkippedConflicts)
	assert.Equal(t, canonicalParentID, pgParentSessionID(t, ctx, pg, childID),
		"a stale replica must leave the canonical row unchanged")

	originName := "Origin child"
	require.NoError(t, originDB.RenameSession(childID, &originName))
	fallbackResult, err := originSync.Push(ctx, false, nil)
	require.NoError(t, err)
	assert.Equal(t, 1, fallbackResult.SessionsPushed,
		"the modified origin child should use committed-PG relationship fallback")
	assert.Zero(t, fallbackResult.SkippedConflicts)
	assert.Equal(t, parentID, pgParentSessionID(t, ctx, pg, childID),
		"origin relationship fallback must resolve the stable bare parent alias")
	assertPGArtifactUpgradeCuration(t, ctx, pg, parentID)
}

func TestPushArtifactOriginAdoptionConvergesImporterFirst(t *testing.T) {
	pgURL := testPGURL(t)
	ctx := context.Background()
	const schema = "agentsview_artifact_origin_upgrade_importer_first_test"
	const originA = "origin-a1b2c3"
	const originB = "origin-b4c5d6"
	const parentID = "native-parent"
	const childID = "native-child"
	const canonicalParentID = originA + "~" + parentID
	const canonicalChildID = originA + "~" + childID
	const referenceID = "pg-reference-holder"
	const stateScope = "work"
	const sourceDisplayName = "Source title"
	const localDisplayName = "PG title"
	const pinCreatedAt = "2026-01-04T00:00:00Z"
	const updatedSourceDisplay = "Origin renamed child"
	const canonicalChildDeletedAt = "2026-01-05T00:00:00Z"

	pg, err := Open(pgURL, schema, true)
	require.NoError(t, err, "Open")
	t.Cleanup(func() { require.NoError(t, pg.Close()) })
	_, err = pg.Exec(`DROP SCHEMA IF EXISTS ` + schema + ` CASCADE`)
	require.NoError(t, err, "drop schema")
	require.NoError(t, EnsureSchema(ctx, pg, schema), "EnsureSchema")

	originDB, err := db.Open(filepath.Join(t.TempDir(), "origin.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, originDB.Close()) })
	require.NoError(t, originDB.UpsertSession(db.Session{
		ID: parentID, Project: "alpha", Machine: "local", Agent: "claude",
		MessageCount: 1, UserMessageCount: 1,
		CreatedAt: "2026-01-01T00:00:00Z",
	}))
	require.NoError(t, originDB.ReplaceSessionMessages(parentID, []db.Message{{
		SessionID: parentID, Ordinal: 0, Role: "user",
		Content: "parent", ContentLength: 6, SourceUUID: "parent-source",
	}}))
	parentRef := parentID
	require.NoError(t, originDB.UpsertSession(db.Session{
		ID: childID, Project: "alpha", Machine: "local", Agent: "claude",
		MessageCount: 1, UserMessageCount: 0, HasToolCalls: true,
		CreatedAt:       "2026-01-01T00:00:01Z",
		ParentSessionID: &parentRef,
		SourceSessionID: parentID,
	}))
	require.NoError(t, originDB.ReplaceSessionMessages(childID, []db.Message{{
		SessionID: childID, Ordinal: 0, Role: "assistant",
		Content: "child", ContentLength: 5, SourceUUID: "child-source",
		HasToolUse: true,
		ToolCalls: []db.ToolCall{{
			ToolName: "subagent", Category: "Task", ToolUseID: "call-parent",
			SubagentSessionID: parentID,
			ResultEvents: []db.ToolResultEvent{{
				ToolUseID: "call-parent", SubagentSessionID: parentID,
				Source: "tool_result", Status: "completed", Content: "done",
				ContentLength: 4,
			}},
		}},
	}}))

	originSync := &Sync{
		pg: pg, local: originDB, machine: "host-a",
		schema: schema, schemaDone: true,
		syncState:       newScopedSyncStateStore(originDB, stateScope, false),
		syncStateTarget: stateScope,
	}
	legacyResult, err := originSync.Push(ctx, false, nil)
	require.NoError(t, err)
	require.Equal(t, 2, legacyResult.SessionsPushed)
	require.NoError(t, originDB.SetSyncState(
		"pg_artifact_identity_v1:"+stateScope, "",
	))

	_, err = pg.ExecContext(ctx, `
		UPDATE sessions
		SET display_name = $1,
			source_display_name = $2
		WHERE id = $3
	`, localDisplayName, sourceDisplayName, parentID)
	require.NoError(t, err, "seed PG-local session curation")
	_, err = pg.ExecContext(ctx, `
		INSERT INTO starred_sessions (session_id, created_at)
		VALUES ($1, '2026-01-04T00:00:00Z'::timestamptz)
	`, parentID)
	require.NoError(t, err, "seed PG-local star")
	_, err = pg.ExecContext(ctx, `
		INSERT INTO pinned_messages (
			session_id, message_id, ordinal, source_uuid, note, created_at
		)
		VALUES ($1, 0, 0, 'parent-source', 'PG pin', $2::timestamptz)
	`, parentID, pinCreatedAt)
	require.NoError(t, err, "seed PG-local pin")

	require.NoError(t, artifact.AdoptOrigin(originDB, originA))
	artifactRepository := openArtifactTestRepository(t, ctx)
	exportResult, err := artifact.ExportToStore(ctx, originDB, artifactRepository.Content(), artifact.ExportOptions{
		Origin: originA,
		Full:   true,
	})
	require.NoError(t, err)
	importerDB, err := db.Open(filepath.Join(t.TempDir(), "importer.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, importerDB.Close()) })
	require.NoError(t, artifact.AdoptOrigin(importerDB, originB))
	importResult := importExportedArtifactCheckpoint(
		t, ctx, importerDB, artifactRepository, originA, originB, exportResult,
	)
	require.Equal(t, 2, importResult.Sessions)
	require.Equal(t, 2, importResult.Messages)

	importerSync := &Sync{
		pg: pg, local: importerDB, machine: "host-b",
		schema: schema, schemaDone: true,
		syncState:       newScopedSyncStateStore(importerDB, stateScope, false),
		syncStateTarget: stateScope,
	}
	importerResult, err := importerSync.Push(ctx, false, nil)
	require.NoError(t, err)
	assert.Zero(t, importerResult.Errors)
	assert.Zero(t, importerResult.SkippedConflicts)
	assertPGArtifactNativeRows(t, ctx, pg, parentID, canonicalParentID, 2)
	assertPGArtifactNativeRows(t, ctx, pg, childID, canonicalChildID, 2)
	_, err = pg.ExecContext(ctx, `
		UPDATE sessions
		SET deleted_at = $1::timestamptz,
			source_deleted_at = NULL
		WHERE id = $2
	`, canonicalChildDeletedAt, canonicalChildID)
	require.NoError(t, err, "seed newer canonical PG curation")
	require.NoError(t, originDB.RenameSession(
		childID, new(updatedSourceDisplay),
	))
	require.NoError(t, originDB.SoftDeleteSession(parentID))
	updatedParent, err := originDB.GetSessionFull(ctx, parentID)
	require.NoError(t, err)
	require.NotNil(t, updatedParent)
	require.NotNil(t, updatedParent.DeletedAt)
	updatedSourceDeletedAt, ok := ParseSQLiteTimestamp(*updatedParent.DeletedAt)
	require.True(t, ok, "parse source soft-delete timestamp")
	wantSourceDeletedAt := updatedSourceDeletedAt.UTC().Format(time.RFC3339)

	_, err = pg.ExecContext(ctx, `
		INSERT INTO sessions (
			id, machine, owner_marker, project, agent, created_at,
			parent_session_id, source_session_id
		) VALUES ($1, 'pg-only', 'pg-only-owner', 'alpha', 'claude', NOW(), $2, $2)
	`, referenceID, parentID)
	require.NoError(t, err, "seed PG-only session relationships")
	_, err = pg.ExecContext(ctx, `
		INSERT INTO messages (session_id, ordinal, role, content)
		VALUES ($1, 0, 'assistant', 'reference')
	`, referenceID)
	require.NoError(t, err, "seed PG-only message")
	_, err = pg.ExecContext(ctx, `
		INSERT INTO tool_calls (
			session_id, tool_name, category, call_index, tool_use_id,
			subagent_session_id, message_ordinal
		) VALUES ($1, 'subagent', 'Task', 0, 'pg-call', $2, 0)
	`, referenceID, parentID)
	require.NoError(t, err, "seed PG-only tool call relationship")
	_, err = pg.ExecContext(ctx, `
		INSERT INTO tool_result_events (
			session_id, tool_call_message_ordinal, call_index,
			tool_use_id, subagent_session_id, source, status,
			content, content_length, event_index
		) VALUES ($1, 0, 0, 'pg-call', $2, 'tool_result',
			'completed', 'done', 4, 0)
	`, referenceID, parentID)
	require.NoError(t, err, "seed PG-only tool result relationship")

	upgradeResult, err := originSync.Push(ctx, false, nil)
	require.NoError(t, err)
	assert.Equal(t, 2, upgradeResult.SessionsPushed,
		"identity-mode change must revisit unchanged origin sessions")
	assert.Zero(t, upgradeResult.Errors)
	assert.Zero(t, upgradeResult.SkippedConflicts)
	assertPGArtifactNativeRows(t, ctx, pg, parentID, canonicalParentID, 1)
	assertPGArtifactNativeRows(t, ctx, pg, childID, canonicalChildID, 1)
	assertPGArtifactStableOwner(t, ctx, pg, canonicalParentID, originA)
	assertPGArtifactStableOwner(t, ctx, pg, canonicalChildID, originA)
	assertPGArtifactUpgradeCurationDetails(
		t, ctx, pg, canonicalParentID,
		localDisplayName, sourceDisplayName,
		wantSourceDeletedAt, wantSourceDeletedAt, pinCreatedAt,
	)
	assertPGArtifactUpdatedSourceDisplay(
		t, ctx, pg, canonicalChildID,
		updatedSourceDisplay, canonicalChildDeletedAt,
	)
	assertPGArtifactUpgradeRelationships(
		t, ctx, pg, canonicalParentID, canonicalChildID, referenceID,
	)

	for _, pusher := range []*Sync{importerSync, originSync} {
		repeated, pushErr := pusher.Push(ctx, true, nil)
		require.NoError(t, pushErr)
		assert.Zero(t, repeated.Errors)
		assert.Zero(t, repeated.SkippedConflicts)
	}
	assertPGArtifactNativeRows(t, ctx, pg, parentID, canonicalParentID, 1)
	assertPGArtifactNativeRows(t, ctx, pg, childID, canonicalChildID, 1)
	assertPGArtifactStableOwner(t, ctx, pg, canonicalParentID, originA)
	assertPGArtifactStableOwner(t, ctx, pg, canonicalChildID, originA)
	assertPGArtifactUpgradeCurationAfterRepeatedPushes(
		t, ctx, pg, canonicalParentID,
		localDisplayName, wantSourceDeletedAt, pinCreatedAt,
	)
	assertPGArtifactUpgradeRelationships(
		t, ctx, pg, canonicalParentID, canonicalChildID, referenceID,
	)
}

func TestPushArtifactOriginAdoptionIgnoresForeignBareAlias(t *testing.T) {
	pgURL := testPGURL(t)
	ctx := context.Background()
	const schema = "agentsview_artifact_origin_upgrade_proof_test"
	const origin = "origin-a1b2c3"
	const nativeID = "native-id"
	const canonicalID = origin + "~" + nativeID

	pg, err := Open(pgURL, schema, true)
	require.NoError(t, err, "Open")
	t.Cleanup(func() { require.NoError(t, pg.Close()) })
	_, err = pg.Exec(`DROP SCHEMA IF EXISTS ` + schema + ` CASCADE`)
	require.NoError(t, err, "drop schema")
	require.NoError(t, EnsureSchema(ctx, pg, schema), "EnsureSchema")

	localDB, err := db.Open(filepath.Join(t.TempDir(), "local.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, localDB.Close()) })
	require.NoError(t, artifact.AdoptOrigin(localDB, origin))
	require.NoError(t, localDB.UpsertSession(db.Session{
		ID: nativeID, Project: "local-project", Machine: "local", Agent: "claude",
		MessageCount: 1, UserMessageCount: 1,
		CreatedAt: "2026-01-01T00:00:00Z",
	}))
	require.NoError(t, localDB.ReplaceSessionMessages(nativeID, []db.Message{{
		SessionID: nativeID, Ordinal: 0, Role: "user",
		Content: "local", ContentLength: 5,
	}}))

	_, err = pg.ExecContext(ctx, `
		INSERT INTO sessions (
			id, machine, owner_marker, project, agent, created_at
		) VALUES
			($1, $2, $3, 'canonical-before', 'claude', NOW()),
			($4, 'legacy-host', 'different-random-marker',
				'legacy-before', 'claude', NOW())
	`, canonicalID, origin, artifactOwnerMarkerPrefix+origin, nativeID)
	require.NoError(t, err, "seed unproven duplicate pair")

	syncer := &Sync{
		pg: pg, local: localDB, machine: "host-a",
		schema: schema, schemaDone: true,
	}
	result, err := syncer.Push(ctx, true, nil)
	require.NoError(t, err)
	assert.Zero(t, result.Errors)
	assert.Zero(t, result.SkippedConflicts)
	assert.Equal(t, 1, result.SessionsPushed)

	rows, err := pg.QueryContext(ctx, `
		SELECT id, project FROM sessions
		WHERE id IN ($1, $2) ORDER BY id
	`, nativeID, canonicalID)
	require.NoError(t, err)
	defer rows.Close()
	projects := map[string]string{}
	for rows.Next() {
		var id, project string
		require.NoError(t, rows.Scan(&id, &project))
		projects[id] = project
	}
	require.NoError(t, rows.Err())
	assert.Equal(t, map[string]string{
		nativeID:    "legacy-before",
		canonicalID: "local-project",
	}, projects, "a foreign bare collision must not block the stable canonical row")
}

func assertPGArtifactNativeRows(
	t *testing.T, ctx context.Context, pg *sql.DB,
	bareID, canonicalID string, want int,
) {
	t.Helper()
	var count int
	require.NoError(t, pg.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM sessions WHERE id IN ($1, $2)
	`, bareID, canonicalID).Scan(&count))
	assert.Equal(t, want, count)
}

func assertPGArtifactStableOwner(
	t *testing.T, ctx context.Context, pg *sql.DB,
	sessionID, origin string,
) {
	t.Helper()
	var machine, ownerMarker string
	require.NoError(t, pg.QueryRowContext(ctx, `
		SELECT machine, owner_marker FROM sessions WHERE id = $1
	`, sessionID).Scan(&machine, &ownerMarker))
	assert.Equal(t, origin, machine)
	assert.Equal(t, artifactOwnerMarkerPrefix+origin, ownerMarker)
}

func assertPGArtifactUpgradeCurationDetails(
	t *testing.T, ctx context.Context, pg *sql.DB, sessionID,
	wantDisplay, wantSourceDisplay, wantDeleted, wantSourceDeleted,
	wantPinCreated string,
) {
	t.Helper()
	assertPGArtifactSessionCuration(
		t, ctx, pg, sessionID,
		wantDisplay, wantSourceDisplay, wantDeleted, wantSourceDeleted,
	)

	var stars int
	require.NoError(t, pg.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM starred_sessions WHERE session_id = $1
	`, sessionID).Scan(&stars))
	assert.Equal(t, 1, stars)

	var ordinal, messageID int
	var sourceUUID, note string
	var createdAt time.Time
	require.NoError(t, pg.QueryRowContext(ctx, `
		SELECT message_id, ordinal, source_uuid, note, created_at
		FROM pinned_messages WHERE session_id = $1
	`, sessionID).Scan(
		&messageID, &ordinal, &sourceUUID, &note, &createdAt,
	))
	assert.Equal(t, 0, messageID)
	assert.Equal(t, 0, ordinal)
	assert.Equal(t, "parent-source", sourceUUID)
	assert.Equal(t, "PG pin", note)
	assert.Equal(t, wantPinCreated, createdAt.UTC().Format(time.RFC3339))
}

func assertPGArtifactSessionCuration(
	t *testing.T, ctx context.Context, pg *sql.DB, sessionID,
	wantDisplay, wantSourceDisplay, wantDeleted, wantSourceDeleted string,
) {
	t.Helper()
	var displayName, sourceDisplayName string
	var deletedAt, sourceDeletedAt sql.NullTime
	require.NoError(t, pg.QueryRowContext(ctx, `
		SELECT display_name, source_display_name, deleted_at, source_deleted_at
		FROM sessions WHERE id = $1
	`, sessionID).Scan(
		&displayName, &sourceDisplayName, &deletedAt, &sourceDeletedAt,
	))
	assert.Equal(t, wantDisplay, displayName)
	assert.Equal(t, wantSourceDisplay, sourceDisplayName)
	if assert.True(t, deletedAt.Valid, "deleted_at") {
		assert.Equal(t, wantDeleted, deletedAt.Time.UTC().Format(time.RFC3339))
	}
	if assert.True(t, sourceDeletedAt.Valid, "source_deleted_at") {
		assert.Equal(t, wantSourceDeleted,
			sourceDeletedAt.Time.UTC().Format(time.RFC3339))
	}
}

func assertPGArtifactUpdatedSourceDisplay(
	t *testing.T, ctx context.Context, pg *sql.DB, sessionID,
	wantDisplay, wantDeleted string,
) {
	t.Helper()
	var displayName, sourceDisplayName sql.NullString
	var deletedAt time.Time
	var sourceDeletedAt sql.NullTime
	require.NoError(t, pg.QueryRowContext(ctx, `
		SELECT display_name, source_display_name, deleted_at, source_deleted_at
		FROM sessions WHERE id = $1
	`, sessionID).Scan(
		&displayName, &sourceDisplayName, &deletedAt, &sourceDeletedAt,
	))
	if assert.True(t, displayName.Valid, "display_name") {
		assert.Equal(t, wantDisplay, displayName.String)
	}
	if assert.True(t, sourceDisplayName.Valid, "source_display_name") {
		assert.Equal(t, wantDisplay, sourceDisplayName.String)
	}
	assert.Equal(t, wantDeleted, deletedAt.UTC().Format(time.RFC3339))
	assert.False(t, sourceDeletedAt.Valid,
		"canonical delete override must retain the current source baseline")
}

func assertPGArtifactUpgradeCurationAfterRepeatedPushes(
	t *testing.T, ctx context.Context, pg *sql.DB, sessionID,
	wantDisplay, wantDeleted, wantPinCreated string,
) {
	t.Helper()
	var displayName string
	var deletedAt time.Time
	require.NoError(t, pg.QueryRowContext(ctx, `
		SELECT display_name, deleted_at
		FROM sessions WHERE id = $1
	`, sessionID).Scan(&displayName, &deletedAt))
	assert.Equal(t, wantDisplay, displayName)
	assert.Equal(t, wantDeleted, deletedAt.UTC().Format(time.RFC3339))

	var stars int
	require.NoError(t, pg.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM starred_sessions WHERE session_id = $1
	`, sessionID).Scan(&stars))
	assert.Equal(t, 1, stars)

	var ordinal, messageID int
	var sourceUUID, note string
	var createdAt time.Time
	require.NoError(t, pg.QueryRowContext(ctx, `
		SELECT message_id, ordinal, source_uuid, note, created_at
		FROM pinned_messages WHERE session_id = $1
	`, sessionID).Scan(
		&messageID, &ordinal, &sourceUUID, &note, &createdAt,
	))
	assert.Equal(t, 0, messageID)
	assert.Equal(t, 0, ordinal)
	assert.Equal(t, "parent-source", sourceUUID)
	assert.Equal(t, "PG pin", note)
	assert.Equal(t, wantPinCreated, createdAt.UTC().Format(time.RFC3339))
}

func assertPGArtifactUpgradeRelationships(
	t *testing.T, ctx context.Context, pg *sql.DB,
	canonicalParentID, canonicalChildID, referenceID string,
) {
	t.Helper()
	var parentID, sourceID string
	require.NoError(t, pg.QueryRowContext(ctx, `
		SELECT parent_session_id, source_session_id
		FROM sessions WHERE id = $1
	`, canonicalChildID).Scan(&parentID, &sourceID))
	assert.Equal(t, canonicalParentID, parentID)
	assert.Equal(t, canonicalParentID, sourceID)
	require.NoError(t, pg.QueryRowContext(ctx, `
		SELECT parent_session_id, source_session_id
		FROM sessions WHERE id = $1
	`, referenceID).Scan(&parentID, &sourceID))
	assert.Equal(t, canonicalParentID, parentID)
	assert.Equal(t, canonicalParentID, sourceID)

	var toolCallSubagentID string
	require.NoError(t, pg.QueryRowContext(ctx, `
		SELECT subagent_session_id FROM tool_calls WHERE session_id = $1
	`, referenceID).Scan(&toolCallSubagentID))
	assert.Equal(t, canonicalParentID, toolCallSubagentID, "tool_calls")
	var toolResultSubagentID string
	require.NoError(t, pg.QueryRowContext(ctx, `
		SELECT subagent_session_id
		FROM tool_result_events WHERE session_id = $1
	`, referenceID).Scan(&toolResultSubagentID))
	assert.Equal(t, canonicalParentID, toolResultSubagentID, "tool_result_events")
}

func pgParentSessionID(
	t *testing.T, ctx context.Context, pg *sql.DB, sessionID string,
) string {
	t.Helper()
	var parentID string
	require.NoError(t, pg.QueryRowContext(ctx, `
		SELECT COALESCE(parent_session_id, '') FROM sessions WHERE id = $1
	`, sessionID).Scan(&parentID))
	return parentID
}

func assertPGArtifactUpgradeCuration(
	t *testing.T, ctx context.Context, pg *sql.DB, sessionID string,
) {
	t.Helper()
	var displayName string
	require.NoError(t, pg.QueryRowContext(ctx, `
		SELECT COALESCE(display_name, '') FROM sessions WHERE id = $1
	`, sessionID).Scan(&displayName))
	assert.Equal(t, "PG title", displayName)
	var stars, pins int
	require.NoError(t, pg.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM starred_sessions WHERE session_id = $1
	`, sessionID).Scan(&stars))
	require.NoError(t, pg.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM pinned_messages
		WHERE session_id = $1 AND note = 'PG pin'
	`, sessionID).Scan(&pins))
	assert.Equal(t, 1, stars)
	assert.Equal(t, 1, pins)
}

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
// local id by a push that predated the collision: the parent's tool call was
// pushed while "sub-1" was still unclaimed, and the subagent session only
// later collided with a foreign owner and moved under "machine-b~sub-1". The
// local rows never change, so both the session candidacy fingerprint and the
// message fast path would otherwise skip the parent and never rewrite the
// link; only the resolved subagent id distinguishes it from PG.
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

	// The parent references "sub-1" before any session by that id exists in
	// PG or locally, so the first push writes the tool-call link at its bare
	// local id.
	require.NoError(t, localDB.UpsertSession(db.Session{
		ID: "parent-1", Project: "proj", Machine: "machine-b", Agent: "claude",
		MessageCount: 1, CreatedAt: "2026-01-01T00:00:00Z",
	}), "UpsertSession parent-1")
	require.NoError(t, localDB.InsertMessages([]db.Message{{
		SessionID: "parent-1", Ordinal: 0, Role: "assistant",
		Content: "spawning", HasToolUse: true,
		ToolCalls: []db.ToolCall{{
			ToolName: "subagent", Category: "Task", SubagentSessionID: "sub-1",
		}},
	}}), "InsertMessages parent-1")

	_, err = sync.Push(ctx, false, nil)
	require.NoError(t, err, "first Push")

	subagent := func() string {
		var s string
		require.NoError(t, pg.QueryRowContext(ctx,
			`SELECT subagent_session_id FROM tool_calls WHERE session_id = $1`,
			"parent-1").Scan(&s), "read parent-1 subagent link")
		return s
	}
	require.Equal(t, "sub-1", subagent(),
		"first push keeps the bare link while the id is unclaimed")

	// A foreign machine claims the bare "sub-1" id, then machine-b's subagent
	// session appears locally and is pushed under the "machine-b~sub-1"
	// prefix. The parent's local row is unchanged, so its PG link now points
	// at the foreign machine's session.
	_, err = pg.ExecContext(ctx, `
		INSERT INTO sessions (
			id, machine, owner_marker, project, agent, created_at
		) VALUES ($1, $2, $3, $4, $5, NOW())
	`, "sub-1", "machine-a", "foreign-owner", "proj", "claude")
	require.NoError(t, err, "insert foreign-owned subagent session")

	require.NoError(t, localDB.UpsertSession(db.Session{
		ID: "sub-1", Project: "proj", Machine: "machine-b", Agent: "claude",
		MessageCount: 1, CreatedAt: "2026-01-01T00:00:00Z",
	}), "UpsertSession sub-1")
	require.NoError(t, localDB.InsertMessages([]db.Message{{
		SessionID: "sub-1", Ordinal: 0, Role: "user", Content: "hi", ContentLength: 2,
	}}), "InsertMessages sub-1")

	const prefixedSub = "machine-b~sub-1"
	_, err = sync.Push(ctx, false, nil)
	require.NoError(t, err, "second Push")
	var n int
	require.NoError(t, pg.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sessions WHERE id = $1 AND machine = $2`,
		prefixedSub, "machine-b").Scan(&n), "count prefixed sub-1")
	require.Equal(t, 1, n, "machine-b's subagent stored under prefixed id")
	require.Equal(t, "sub-1", subagent(),
		"precondition: parent was not a candidate, so its link is stale")

	// Re-list the parent without touching its message content, so only the
	// resolved subagent id distinguishes it from what was last pushed.
	require.NoError(t, localDB.BumpLocalModifiedAt("parent-1"),
		"mark parent-1 modified")

	res, err := sync.Push(ctx, false, nil)
	require.NoError(t, err, "third Push")
	assert.Zero(t, res.Errors, "third push should report no failures")
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

	identity := pushedSessionIdentity{Machine: "new-host"}
	got, err := sync.resolveOwnedPushIdentityID(
		ctx, "sess-x", identity, markerID, []string{"old-host"},
	)
	require.NoError(t, err)
	assert.Equal(t, "old-host~sess-x", got,
		"a renamed pusher must reuse the row it owns under the old machine prefix")

	// Without the old machine name, the resolver cannot find the owned row and
	// would mint a duplicate under the new prefix -- the blind spot being fixed.
	got, err = sync.resolveOwnedPushIdentityID(ctx, "sess-x", identity, markerID, nil)
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
