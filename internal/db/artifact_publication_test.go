package db

import (
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// seedArtifactOrigin marks database as owning an artifact origin. The
// session triggers and enqueueArtifactExportTx that populate
// artifact_export_queue are gated on this pg_sync_state row (see
// artifactSessionQueueTriggerCreatesSQL and enqueueArtifactExportTx); tests
// that exercise queue/trigger behavior must call this before any session
// mutation they expect to enqueue.
func seedArtifactOrigin(t *testing.T, database *DB) {
	t.Helper()
	require.NoError(t, database.SetSyncState("artifact_origin_id", "desktop-a1b2c3"))
}

func TestArtifactPublicationChangesRejectMismatchedPersistedOrigin(t *testing.T) {
	for _, tc := range []struct {
		name          string
		removeOrigin  bool
		requestOrigin string
	}{
		{
			name:          "different origin",
			requestOrigin: "origin-d4e5f6",
		},
		{
			name:          "missing origin",
			removeOrigin:  true,
			requestOrigin: "origin-a1b2c3",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)

			database := testDB(t)
			ctx := t.Context()
			require.NoError(database.AdoptArtifactOrigin("origin-a1b2c3"))
			require.NoError(database.UpsertSession(Session{
				ID: "session", Project: "project", Machine: "local", Agent: "claude",
			}))
			claims, err := database.PendingArtifactExports(ctx, 10)
			require.NoError(err)
			require.Len(claims, 1)
			if tc.removeOrigin {
				require.NoError(database.Update(func(tx *sql.Tx) error {
					_, err := tx.ExecContext(ctx,
						`DELETE FROM pg_sync_state WHERE key = 'artifact_origin_id'`,
					)
					return err
				}))
			}

			revision, changed, err := database.ApplyArtifactPublicationChanges(
				ctx, tc.requestOrigin, []ArtifactPublicationChange{{
					SessionID: "session", Generation: claims[0].Generation,
					ManifestHash: "manifest", SourceFingerprint: "fingerprint",
				}},
			)
			require.ErrorIs(err, ErrArtifactOriginMismatch)
			assert.Zero(revision)
			assert.False(changed)

			var publications []ArtifactPublication
			_, streamErr := database.StreamArtifactPublications(
				ctx, tc.requestOrigin, func(row ArtifactPublication) error {
					publications = append(publications, row)
					return nil
				},
			)
			require.NoError(streamErr)
			assert.Empty(publications)
			pending, pendingErr := database.PendingArtifactExports(ctx, 10)
			require.NoError(pendingErr)
			require.Equal(claims, pending)
		})
	}
}

func TestArtifactPublicationOriginValidationFollowsWriterReservation(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	path := filepath.Join(t.TempDir(), "archive.db")
	exporter, err := Open(path)
	require.NoError(err)
	t.Cleanup(func() { require.NoError(exporter.Close()) })
	adopter, err := Open(path)
	require.NoError(err)
	t.Cleanup(func() { require.NoError(adopter.Close()) })
	require.NoError(exporter.AdoptArtifactOrigin("origin-a1b2c3"))
	require.NoError(exporter.UpsertSession(Session{
		ID: "session", Project: "project", Machine: "local", Agent: "claude",
	}))
	claims, err := exporter.PendingArtifactExports(t.Context(), 10)
	require.NoError(err)
	require.Len(claims, 1)

	originalPopulate := populateArtifactOriginQueueTx
	adoptionStarted := make(chan struct{})
	releaseAdoption := make(chan struct{})
	populateArtifactOriginQueueTx = func(tx *sql.Tx, origin string, requeue bool) error {
		close(adoptionStarted)
		<-releaseAdoption
		return originalPopulate(tx, origin, requeue)
	}
	originalBeforeLock := beforeApplyArtifactPublicationLock
	applyAtLock := make(chan struct{})
	releaseApply := make(chan struct{})
	beforeApplyArtifactPublicationLock = func() {
		close(applyAtLock)
		<-releaseApply
	}
	t.Cleanup(func() {
		populateArtifactOriginQueueTx = originalPopulate
		beforeApplyArtifactPublicationLock = originalBeforeLock
	})

	adoptDone := make(chan error, 1)
	go func() {
		adoptDone <- adopter.AdoptArtifactOrigin("origin-d4e5f6")
	}()
	<-adoptionStarted

	type applyResult struct {
		revision int64
		changed  bool
		err      error
	}
	applyDone := make(chan applyResult, 1)
	go func() {
		revision, changed, err := exporter.ApplyArtifactPublicationChanges(
			t.Context(), "origin-a1b2c3", []ArtifactPublicationChange{{
				SessionID: "session", Generation: claims[0].Generation,
				ManifestHash: "manifest", SourceFingerprint: "fingerprint",
			}},
		)
		applyDone <- applyResult{revision: revision, changed: changed, err: err}
	}()
	<-applyAtLock
	close(releaseAdoption)
	require.NoError(<-adoptDone)
	close(releaseApply)

	result := <-applyDone
	require.ErrorIs(result.err, ErrArtifactOriginMismatch)
	assert.Zero(result.revision)
	assert.False(result.changed)
	revision, err := exporter.StreamArtifactPublications(
		t.Context(), "origin-a1b2c3", func(ArtifactPublication) error {
			return errors.New("stale origin must not gain publication authority")
		},
	)
	require.NoError(err)
	assert.Zero(revision)
	pending, err := exporter.PendingArtifactExports(t.Context(), 10)
	require.NoError(err)
	require.Len(pending, 1)
	assert.Greater(pending[0].Generation, claims[0].Generation,
		"the only queue change comes from the committed origin adoption")
}

func TestArtifactExportRejectionFinalizesExactGeneration(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	database := testDB(t)
	ctx := t.Context()
	require.NoError(database.AdoptArtifactOrigin("origin-a1b2c3"))
	require.NoError(database.UpsertSession(Session{
		ID: "rejected", Project: "project", Machine: "local", Agent: "claude",
	}))
	claims, err := database.PendingArtifactExports(ctx, 10)
	require.NoError(err)
	require.Len(claims, 1)

	require.NoError(database.FinalizeArtifactExports(ctx, []ArtifactExportOutcome{{
		Item: claims[0], Rejection: "session message limit exceeded",
	}}))
	pending, err := database.PendingArtifactExports(ctx, 10)
	require.NoError(err)
	assert.Empty(pending)
	rejection, ok, err := database.GetArtifactExportRejection(ctx, "rejected")
	require.NoError(err)
	require.True(ok)
	assert.Equal(claims[0].Generation, rejection.Generation)
	assert.Equal("session message limit exceeded", rejection.Error)
	assert.NotEmpty(rejection.RejectedAt)

	require.NoError(database.ReplaceSessionMessages("rejected", []Message{{
		SessionID: "rejected", Ordinal: 0, Role: "user", Content: "changed",
	}}))
	pending, err = database.PendingArtifactExports(ctx, 10)
	require.NoError(err)
	require.Len(pending, 1)
	assert.Greater(pending[0].Generation, claims[0].Generation)
	_, ok, err = database.GetArtifactExportRejection(ctx, "rejected")
	require.NoError(err)
	assert.False(ok)
}

func TestArtifactExportRejectionCannotConsumeNewerGeneration(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	database := testDB(t)
	ctx := t.Context()
	require.NoError(database.AdoptArtifactOrigin("origin-a1b2c3"))
	require.NoError(database.UpsertSession(Session{
		ID: "rejected", Project: "project", Machine: "local", Agent: "claude",
	}))
	claims, err := database.PendingArtifactExports(ctx, 10)
	require.NoError(err)
	require.Len(claims, 1)
	require.NoError(database.UpsertSession(Session{
		ID: "rejected", Project: "changed", Machine: "local", Agent: "claude",
	}))

	err = database.FinalizeArtifactExports(ctx, []ArtifactExportOutcome{{
		Item: claims[0], Rejection: "stale rejection",
	}})
	require.ErrorIs(err, ErrArtifactExportClaimStale)
	pending, err := database.PendingArtifactExports(ctx, 10)
	require.NoError(err)
	require.Len(pending, 1)
	assert.Greater(pending[0].Generation, claims[0].Generation)
	_, ok, err := database.GetArtifactExportRejection(ctx, "rejected")
	require.NoError(err)
	assert.False(ok)
}

func TestArtifactCheckpointHeadAndRejectionsCommitAtomically(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	database := testDB(t)
	ctx := t.Context()
	require.NoError(database.AdoptArtifactOrigin("origin-a1b2c3"))
	for _, id := range []string{"accepted", "rejected"} {
		require.NoError(database.UpsertSession(Session{
			ID: id, Project: "project", Machine: "local", Agent: "claude",
		}))
	}
	claims, err := database.PendingArtifactExports(ctx, 10)
	require.NoError(err)
	require.Len(claims, 2)
	claimByID := map[string]ArtifactExportQueueItem{
		claims[0].SessionID: claims[0],
		claims[1].SessionID: claims[1],
	}
	head := ArtifactCheckpointHead{
		Origin: "origin-a1b2c3", Sequence: 1, PublicationRevision: 0,
		SessionMapSHA256: "map-one", CheckpointSHA256: "checkpoint-one",
		CheckpointSize: 17,
	}
	require.NoError(database.RecordArtifactCheckpointHeadOutcomes(
		ctx, head, []ArtifactExportOutcome{
			{Item: claimByID["accepted"]},
			{Item: claimByID["rejected"], Rejection: "message limit exceeded"},
		},
	))

	gotHead, ok, err := database.GetArtifactCheckpointHead(ctx, head.Origin)
	require.NoError(err)
	require.True(ok)
	assert.Equal(head, gotHead)
	pending, err := database.PendingArtifactExports(ctx, 10)
	require.NoError(err)
	assert.Empty(pending)
	rejection, ok, err := database.GetArtifactExportRejection(ctx, "rejected")
	require.NoError(err)
	require.True(ok)
	assert.Equal(claimByID["rejected"].Generation, rejection.Generation)

	for _, id := range []string{"accepted", "rejected"} {
		require.NoError(database.UpsertSession(Session{
			ID: id, Project: "retry", Machine: "local", Agent: "claude",
		}))
	}
	retryClaims, err := database.PendingArtifactExports(ctx, 10)
	require.NoError(err)
	require.Len(retryClaims, 2)
	retryByID := map[string]ArtifactExportQueueItem{
		retryClaims[0].SessionID: retryClaims[0],
		retryClaims[1].SessionID: retryClaims[1],
	}
	require.NoError(database.UpsertSession(Session{
		ID: "rejected", Project: "newer", Machine: "local", Agent: "claude",
	}))

	err = database.RecordArtifactCheckpointHeadOutcomes(
		ctx, ArtifactCheckpointHead{
			Origin: "origin-a1b2c3", Sequence: 2, PublicationRevision: 0,
			SessionMapSHA256: "map-two", CheckpointSHA256: "checkpoint-two",
			CheckpointSize: 23,
		}, []ArtifactExportOutcome{
			{Item: retryByID["accepted"]},
			{Item: retryByID["rejected"], Rejection: "stale rejection"},
		},
	)
	require.ErrorIs(err, ErrArtifactExportClaimStale)
	gotHead, ok, err = database.GetArtifactCheckpointHead(ctx, head.Origin)
	require.NoError(err)
	require.True(ok)
	assert.Equal(head, gotHead)
	pending, err = database.PendingArtifactExports(ctx, 10)
	require.NoError(err)
	require.Len(pending, 2)
	pendingByID := map[string]ArtifactExportQueueItem{
		pending[0].SessionID: pending[0],
		pending[1].SessionID: pending[1],
	}
	assert.Equal(retryByID["accepted"].Generation, pendingByID["accepted"].Generation)
	assert.Greater(pendingByID["rejected"].Generation, retryByID["rejected"].Generation)
	_, ok, err = database.GetArtifactExportRejection(ctx, "rejected")
	require.NoError(err)
	assert.False(ok)
}

func TestArtifactRequeueLifecycleClearsRejection(t *testing.T) {
	t.Run("origin adoption", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)

		database := testDB(t)
		ctx := t.Context()
		require.NoError(database.AdoptArtifactOrigin("origin-a1b2c3"))
		require.NoError(database.UpsertSession(Session{
			ID: "session", Project: "project", Machine: "local", Agent: "claude",
		}))
		claims, err := database.PendingArtifactExports(ctx, 10)
		require.NoError(err)
		require.Len(claims, 1)
		require.NoError(database.FinalizeArtifactExports(ctx, []ArtifactExportOutcome{{
			Item: claims[0], Rejection: "message limit exceeded",
		}}))

		require.NoError(database.AdoptArtifactOrigin("origin-d4e5f6"))
		pending, err := database.PendingArtifactExports(ctx, 10)
		require.NoError(err)
		require.Len(pending, 1)
		assert.Greater(pending[0].Generation, claims[0].Generation)
		_, ok, err := database.GetArtifactExportRejection(ctx, "session")
		require.NoError(err)
		assert.False(ok)
	})

	t.Run("resync", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)

		dir := t.TempDir()
		sourcePath := filepath.Join(dir, "source.db")
		source, err := Open(sourcePath)
		require.NoError(err)
		require.NoError(source.AdoptArtifactOrigin("origin-a1b2c3"))
		require.NoError(source.UpsertSession(Session{
			ID: "session", Project: "project", Machine: "local", Agent: "claude",
		}))
		claims, err := source.PendingArtifactExports(t.Context(), 10)
		require.NoError(err)
		require.Len(claims, 1)
		require.NoError(source.FinalizeArtifactExports(
			t.Context(), []ArtifactExportOutcome{{
				Item: claims[0], Rejection: "message limit exceeded",
			}},
		))
		require.NoError(source.Close())

		target, err := Open(filepath.Join(dir, "target.db"))
		require.NoError(err)
		t.Cleanup(func() { require.NoError(target.Close()) })
		require.NoError(target.CopySyncStateFrom(sourcePath))
		pending, err := target.PendingArtifactExports(t.Context(), 10)
		require.NoError(err)
		require.Len(pending, 1)
		assert.Greater(pending[0].Generation, claims[0].Generation)
		_, ok, err := target.GetArtifactExportRejection(t.Context(), "session")
		require.NoError(err)
		assert.False(ok)
	})
}

func TestArtifactExportQueueRejectionColumnMigration(t *testing.T) {
	require := require.New(t)

	path := filepath.Join(t.TempDir(), "legacy-artifact-queue.db")
	database, err := Open(path)
	require.NoError(err)
	require.NoError(database.AdoptArtifactOrigin("origin-a1b2c3"))
	require.NoError(database.UpsertSession(Session{
		ID: "preserved", Project: "project", Machine: "local", Agent: "claude",
	}))
	claims, err := database.PendingArtifactExports(t.Context(), 10)
	require.NoError(err)
	require.Len(claims, 1)
	require.NoError(database.Close())

	conn, err := sql.Open("sqlite3", makeDSN(path, false))
	require.NoError(err)
	_, err = conn.ExecContext(t.Context(), artifactSessionQueueTriggerDropsSQL)
	require.NoError(err)
	for _, column := range []string{"rejected_generation", "last_error", "rejected_at"} {
		_, err = conn.ExecContext(t.Context(), `ALTER TABLE artifact_export_queue DROP COLUMN `+column)
		require.NoError(err)
	}
	require.NoError(conn.Close())

	migrated, err := Open(path)
	require.NoError(err)
	t.Cleanup(func() { require.NoError(migrated.Close()) })
	rows, err := migrated.getReader().Query(
		`SELECT name FROM pragma_table_info('artifact_export_queue')
		 WHERE name IN ('rejected_generation', 'last_error', 'rejected_at')
		 ORDER BY name`,
	)
	require.NoError(err)
	defer rows.Close()
	var columns []string
	for rows.Next() {
		var column string
		require.NoError(rows.Scan(&column))
		columns = append(columns, column)
	}
	require.NoError(rows.Err())
	assert.Equal(t, []string{"last_error", "rejected_at", "rejected_generation"}, columns)
	pending, err := migrated.PendingArtifactExports(t.Context(), 10)
	require.NoError(err)
	require.Equal(claims, pending)
}

func TestArtifactPublicationAtomicLifecycle(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	database := testDB(t)
	ctx := t.Context()
	seedArtifactOrigin(t, database)
	require.NoError(database.UpsertSession(Session{
		ID: "session-a", Project: "project", Machine: "local", Agent: "claude",
	}))

	pending, err := database.PendingArtifactExports(ctx, 10)
	require.NoError(err)
	require.Len(pending, 1)
	require.Equal([]ArtifactExportQueueItem{{
		SessionID: "session-a", EnqueuedAt: pending[0].EnqueuedAt,
		Generation: pending[0].Generation,
	}}, pending)

	// Merely reading work models a failed export: nothing is acknowledged until
	// a checkpoint is durably created and its head is recorded.
	pendingAgain, err := database.PendingArtifactExports(ctx, 10)
	require.NoError(err)
	require.Equal(pending, pendingAgain)

	revision, changed, err := database.ApplyArtifactPublicationChanges(ctx, "desktop-a1b2c3", []ArtifactPublicationChange{{
		SessionID: "session-a", Generation: pending[0].Generation,
		ManifestHash: "manifest-a", SourceFingerprint: "source-a",
	}})
	require.NoError(err)
	assert.True(changed)
	assert.Equal(int64(1), revision)

	revision, changed, err = database.ApplyArtifactPublicationChanges(ctx, "desktop-a1b2c3", []ArtifactPublicationChange{{
		SessionID: "session-a", Generation: pending[0].Generation,
		ManifestHash: "manifest-a", SourceFingerprint: "source-a",
	}})
	require.NoError(err)
	assert.False(changed, "identical publication state must not force a checkpoint")

	head := ArtifactCheckpointHead{
		Origin: "desktop-a1b2c3", Sequence: 7, PublicationRevision: revision,
		SessionMapSHA256: "map-hash", CheckpointSHA256: "checkpoint-hash",
	}
	require.NoError(database.RecordArtifactCheckpointHead(ctx, head, pending))
	gotHead, ok, err := database.GetArtifactCheckpointHead(ctx, "desktop-a1b2c3")
	require.NoError(err)
	require.True(ok)
	assert.Equal(head, gotHead)
	pending, err = database.PendingArtifactExports(ctx, 10)
	require.NoError(err)
	assert.Empty(pending)

	require.NoError(database.UpsertSession(Session{
		ID: "session-a", Project: "project-2", Machine: "local", Agent: "claude",
	}))
	pending, err = database.PendingArtifactExports(ctx, 10)
	require.NoError(err)
	require.Len(pending, 1)
	_, changed, err = database.ApplyArtifactPublicationChanges(ctx, "desktop-a1b2c3", []ArtifactPublicationChange{{
		SessionID: "session-a", Generation: pending[0].Generation, Delete: true,
	}})
	require.NoError(err)
	assert.True(changed)
	staleHead, ok, err := database.GetArtifactCheckpointHead(ctx, "desktop-a1b2c3")
	require.NoError(err)
	require.True(ok)
	assert.Equal(revision, staleHead.PublicationRevision,
		"the retained head revision identifies it as stale")
	pending, err = database.PendingArtifactExports(ctx, 10)
	require.NoError(err)
	require.NoError(database.AcknowledgeArtifactExports(ctx, pending))
	var publications []ArtifactPublication
	_, err = database.StreamArtifactPublications(ctx, "desktop-a1b2c3", func(row ArtifactPublication) error {
		publications = append(publications, row)
		return nil
	})
	require.NoError(err)
	assert.Empty(publications)
}

func TestArtifactCheckpointHeadRejectsStalePublicationRevision(t *testing.T) {
	require := require.New(t)

	path := filepath.Join(t.TempDir(), "archive.db")
	first, err := Open(path)
	require.NoError(err)
	t.Cleanup(func() { require.NoError(first.Close()) })
	second, err := Open(path)
	require.NoError(err)
	t.Cleanup(func() { require.NoError(second.Close()) })
	seedArtifactOrigin(t, first)

	ctx := t.Context()
	require.NoError(first.UpsertSession(Session{
		ID: "session-a", Project: "project", Machine: "local", Agent: "claude",
	}))
	claimA, err := first.ArtifactExportClaims(ctx, []string{"session-a"})
	require.NoError(err)
	require.Len(claimA, 1)
	revisionA, changed, err := first.ApplyArtifactPublicationChanges(
		ctx, "desktop-a1b2c3", []ArtifactPublicationChange{{
			SessionID: "session-a", Generation: claimA[0].Generation,
			ManifestHash: "manifest-a", SourceFingerprint: "source-a",
		}},
	)
	require.NoError(err)
	require.True(changed)
	streamedRevisionA, err := first.StreamArtifactPublications(
		ctx, "desktop-a1b2c3", func(ArtifactPublication) error { return nil },
	)
	require.NoError(err)
	require.Equal(revisionA, streamedRevisionA)

	require.NoError(second.UpsertSession(Session{
		ID: "session-b", Project: "project", Machine: "local", Agent: "claude",
	}))
	claimB, err := second.ArtifactExportClaims(ctx, []string{"session-b"})
	require.NoError(err)
	require.Len(claimB, 1)
	revisionB, changed, err := second.ApplyArtifactPublicationChanges(
		ctx, "desktop-a1b2c3", []ArtifactPublicationChange{{
			SessionID: "session-b", Generation: claimB[0].Generation,
			ManifestHash: "manifest-b", SourceFingerprint: "source-b",
		}},
	)
	require.NoError(err)
	require.True(changed)
	require.Greater(revisionB, revisionA)
	streamedRevisionB, err := second.StreamArtifactPublications(
		ctx, "desktop-a1b2c3", func(ArtifactPublication) error { return nil },
	)
	require.NoError(err)
	require.Equal(revisionB, streamedRevisionB)
	require.NoError(second.RecordArtifactCheckpointHead(ctx, ArtifactCheckpointHead{
		Origin: "desktop-a1b2c3", Sequence: 1, PublicationRevision: revisionB,
		SessionMapSHA256: "map-b", CheckpointSHA256: "checkpoint-b", CheckpointSize: 12,
	}, claimB))

	err = first.RecordArtifactCheckpointHead(ctx, ArtifactCheckpointHead{
		Origin: "desktop-a1b2c3", Sequence: 2, PublicationRevision: streamedRevisionA,
		SessionMapSHA256: "map-a", CheckpointSHA256: "checkpoint-a", CheckpointSize: 12,
	}, claimA)
	require.ErrorIs(err, ErrArtifactExportClaimStale)
	head, ok, err := first.GetArtifactCheckpointHead(ctx, "desktop-a1b2c3")
	require.NoError(err)
	require.True(ok)
	require.Equal(1, head.Sequence)
	require.Equal(revisionB, head.PublicationRevision)
	pending, err := first.ArtifactExportClaims(ctx, []string{"session-a"})
	require.NoError(err)
	require.Equal(claimA, pending, "stale recording cannot consume pending work")

	retryRevision, changed, err := first.ApplyArtifactPublicationChanges(
		ctx, "desktop-a1b2c3", []ArtifactPublicationChange{{
			SessionID: "session-a", Generation: claimA[0].Generation,
			ManifestHash: "manifest-a", SourceFingerprint: "source-a",
		}},
	)
	require.NoError(err)
	require.False(changed)
	require.Equal(revisionB, retryRevision)
	require.NoError(first.RecordArtifactCheckpointHead(ctx, ArtifactCheckpointHead{
		Origin: "desktop-a1b2c3", Sequence: 3, PublicationRevision: retryRevision,
		SessionMapSHA256: "map-b", CheckpointSHA256: "checkpoint-c", CheckpointSize: 12,
	}, claimA))
}

func TestBootstrapArtifactExportQueueEnqueuesExistingLocalSessions(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	database := testDB(t)
	require.NoError(database.UpsertSession(Session{
		ID: "existing-local", Project: "project", Machine: "local", Agent: "claude",
	}))
	require.NoError(database.UpsertSession(Session{
		ID: "existing-peer", Project: "project", Machine: "peer-a1b2c3", Agent: "claude",
	}))
	require.NoError(database.UpsertSession(Session{
		ID: "existing-trash", Project: "project", Machine: "local", Agent: "claude",
	}))
	require.NoError(database.SoftDeleteSession("existing-trash"))

	require.NoError(database.BootstrapArtifactExportQueue())
	pending, err := database.PendingArtifactExports(t.Context(), 10)
	require.NoError(err)
	require.Len(pending, 1)
	assert.Equal("existing-local", pending[0].SessionID,
		"only the live locally-owned session is bootstrapped")
	assert.Equal(int64(1), pending[0].Generation)
	firstEnqueuedAt := pending[0].EnqueuedAt

	require.NoError(database.BootstrapArtifactExportQueue())
	pendingAgain, err := database.PendingArtifactExports(t.Context(), 10)
	require.NoError(err)
	require.Len(pendingAgain, 1, "repeated bootstrap must not duplicate queue rows")
	assert.Equal("existing-local", pendingAgain[0].SessionID)
	assert.Equal(int64(1), pendingAgain[0].Generation,
		"INSERT OR IGNORE leaves the existing generation untouched")
	assert.Equal(firstEnqueuedAt, pendingAgain[0].EnqueuedAt)
}

func TestArtifactPublicationOwnsConfiguredHostnameSessions(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	database := testDB(t)
	require.NoError(database.SetSyncState(
		"artifact_local_machine_name", "workstation.example",
	))
	seedArtifactOrigin(t, database)
	require.NoError(database.UpsertSession(Session{
		ID: "hostname-local", Project: "project",
		Machine: "workstation.example", Agent: "claude",
	}))
	require.NoError(database.UpsertSession(Session{
		ID: "foreign", Project: "project",
		Machine: "peer.example", Agent: "claude",
	}))

	pending, err := database.PendingArtifactExports(t.Context(), 10)
	require.NoError(err)
	require.Len(pending, 1)
	assert.Equal("hostname-local", pending[0].SessionID)
	owned, err := database.ListOwnedSessionIDsForExport(t.Context())
	require.NoError(err)
	assert.Equal([]string{"hostname-local"}, owned)

	clearArtifactExportQueue(t, database)
	require.NoError(database.ReplaceSessionUsageEvents(
		"hostname-local", []UsageEvent{{
			SessionID: "hostname-local", Source: "event",
			Model: "model", DedupKey: "hostname",
		}},
	))
	assert.Equal([]string{"hostname-local"}, artifactExportQueueIDs(t, database),
		"child-row fallback uses the configured local machine")
}

func TestBootstrapArtifactExportQueueOwnsConfiguredHostname(t *testing.T) {
	require := require.New(t)

	database := testDB(t)
	require.NoError(database.SetSyncState(
		"artifact_local_machine_name", "workstation.example",
	))
	require.NoError(database.UpsertSession(Session{
		ID: "existing-hostname", Project: "project",
		Machine: "workstation.example", Agent: "claude",
	}))

	require.NoError(database.BootstrapArtifactExportQueue())
	pending, err := database.PendingArtifactExports(t.Context(), 10)
	require.NoError(err)
	require.Len(pending, 1)
	assert.Equal(t, "existing-hostname", pending[0].SessionID)
}

func TestConfigureArtifactLocalMachineRequeuesExistingInstallationSession(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	database := testDB(t)
	require.NoError(database.UpsertSession(Session{
		ID: "existing-installation", Project: "project",
		Machine: "00000000-0000-4000-8000-000000000001", Agent: "claude",
	}))
	seedArtifactOrigin(t, database)
	assert.Empty(artifactExportQueueIDs(t, database))

	require.NoError(database.ConfigureArtifactLocalMachine("00000000-0000-4000-8000-000000000001"))
	pending, err := database.PendingArtifactExports(t.Context(), 10)
	require.NoError(err)
	require.Len(pending, 1)
	assert.Equal("existing-installation", pending[0].SessionID)
}

func TestArtifactQueueUsesAdoptedInstallationOwnership(t *testing.T) {
	parentAssert := assert.New(t)
	parentRequire := require.New(t)

	database := testDB(t)
	seedArtifactOrigin(t, database)
	parentRequire.NoError(database.SetSyncState("artifact_local_machine_name", "workstation.example"))
	for _, session := range []struct{ id, machine string }{
		{"hostname", "workstation.example"},
		{"installation", "00000000-0000-4000-8000-000000000001"},
		{"legacy", "local"},
		{"foreign", "peer.example"},
	} {
		parentRequire.NoError(database.UpsertSession(Session{
			ID: session.id, Project: "project", Machine: session.machine, Agent: "claude",
		}))
	}
	_, err := database.EnsureInstallationIdentity(t.Context(), "00000000-0000-4000-8000-000000000001")
	parentRequire.NoError(err)
	parentRequire.NoError(database.UpsertSession(Session{ID: "retired-key-peer", Project: "project", Machine: "workstation.example", Agent: "claude"}))
	want := []string{"hostname", "installation", "legacy"}
	parentAssert.Equal(want, artifactExportQueueIDs(t, database), "inserts")
	owned, err := database.ListOwnedSessionIDsForExport(t.Context())
	parentRequire.NoError(err)
	parentAssert.Equal(want, owned)

	for _, operation := range []struct {
		name string
		run  func() error
	}{
		{"bootstrap", database.BootstrapArtifactExportQueue},
		{"requeue", database.RequeueAllArtifactExports},
		{"origin", func() error { return database.AdoptArtifactOrigin("replacement-a1b2c3") }},
		{"update", func() error {
			for _, id := range []string{"hostname", "installation", "legacy", "foreign"} {
				if err := database.RenameSession(id, new("renamed")); err != nil {
					return err
				}
			}
			return nil
		}},
		{"child rows", func() error {
			for _, id := range []string{"hostname", "installation", "legacy", "foreign"} {
				if err := database.ReplaceSessionUsageEvents(id, []UsageEvent{{
					SessionID: id, Source: "event", Model: "model", DedupKey: id,
				}}); err != nil {
					return err
				}
			}
			return nil
		}},
		{"delete", func() error {
			for _, id := range []string{"hostname", "installation", "legacy", "foreign"} {
				if err := database.DeleteSession(id); err != nil {
					return err
				}
			}
			return nil
		}},
	} {
		t.Run(operation.name, func(t *testing.T) {
			require.NoError(t, database.Update(func(tx *sql.Tx) error {
				_, err := tx.ExecContext(t.Context(), `DELETE FROM artifact_export_queue`)
				return err
			}))
			require.NoError(t, operation.run())
			assert.Equal(t, want, artifactExportQueueIDs(t, database))
		})
	}
}

func TestEnsureArtifactOriginPublishesOriginWithBootstrapQueue(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	database := testDB(t)
	require.NoError(database.UpsertSession(Session{
		ID: "session-a", Project: "project", Machine: "local", Agent: "claude",
	}))

	originalPopulate := populateArtifactOriginQueueTx
	started := make(chan struct{})
	release := make(chan struct{})
	populateArtifactOriginQueueTx = func(tx *sql.Tx, origin string, requeue bool) error {
		close(started)
		<-release
		return originalPopulate(tx, origin, requeue)
	}
	t.Cleanup(func() { populateArtifactOriginQueueTx = originalPopulate })

	type ensureResult struct {
		origin string
		err    error
	}
	done := make(chan ensureResult, 1)
	go func() {
		origin, err := database.EnsureArtifactOrigin("desk-a1b2c3")
		done <- ensureResult{origin: origin, err: err}
	}()

	<-started
	visible, err := database.GetSyncState("artifact_origin_id")
	require.NoError(err)
	assert.Empty(visible,
		"the origin must remain invisible until queue population commits")
	close(release)

	result := <-done
	require.NoError(result.err)
	assert.Equal("desk-a1b2c3", result.origin)
	stored, err := database.GetSyncState("artifact_origin_id")
	require.NoError(err)
	assert.Equal("desk-a1b2c3", stored)
	pending, err := database.PendingArtifactExports(t.Context(), 10)
	require.NoError(err)
	require.Len(pending, 1)
	assert.Equal("session-a", pending[0].SessionID)
}

func TestEnsureArtifactOriginRequeuesExistingLedgerWhenOriginStateIsEmpty(t *testing.T) {
	for _, tt := range []struct {
		name        string
		clearOrigin func(*testing.T, *DB)
	}{
		{
			name: "missing row",
			clearOrigin: func(t *testing.T, database *DB) {
				t.Helper()
				require.NoError(t, database.Update(func(tx *sql.Tx) error {
					_, err := tx.ExecContext(t.Context(),
						`DELETE FROM pg_sync_state WHERE key = 'artifact_origin_id'`,
					)
					return err
				}))
			},
		},
		{
			name: "empty value",
			clearOrigin: func(t *testing.T, database *DB) {
				t.Helper()
				require.NoError(t, database.SetSyncState("artifact_origin_id", ""))
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)

			database := testDB(t)
			ctx := t.Context()
			for _, session := range []Session{
				{ID: "accepted", Project: "project", Machine: "local", Agent: "claude"},
				{ID: "rejected", Project: "project", Machine: "local", Agent: "claude"},
				{ID: "foreign", Project: "project", Machine: "peer-a1b2c3", Agent: "claude"},
				{ID: "deleted", Project: "project", Machine: "local", Agent: "claude"},
			} {
				require.NoError(database.UpsertSession(session))
			}
			require.NoError(database.SoftDeleteSession("deleted"))
			require.NoError(database.AdoptArtifactOrigin("origin-a1b2c3"))

			claims, err := database.PendingArtifactExports(ctx, 10)
			require.NoError(err)
			require.Len(claims, 2)
			claimByID := map[string]ArtifactExportQueueItem{
				claims[0].SessionID: claims[0],
				claims[1].SessionID: claims[1],
			}
			require.NoError(database.FinalizeArtifactExports(
				ctx, []ArtifactExportOutcome{
					{Item: claimByID["accepted"]},
					{Item: claimByID["rejected"], Rejection: "message limit exceeded"},
				},
			))
			drained, err := database.PendingArtifactExports(ctx, 10)
			require.NoError(err)
			require.Empty(drained)
			_, rejected, err := database.GetArtifactExportRejection(ctx, "rejected")
			require.NoError(err)
			require.True(rejected)

			tt.clearOrigin(t, database)
			origin, err := database.EnsureArtifactOrigin("origin-d4e5f6")
			require.NoError(err)
			assert.Equal("origin-d4e5f6", origin)

			pending, err := database.PendingArtifactExports(ctx, 10)
			require.NoError(err)
			require.Len(pending, 2)
			assert.ElementsMatch([]string{"accepted", "rejected"}, []string{
				pending[0].SessionID, pending[1].SessionID,
			})
			for _, item := range pending {
				assert.Greater(item.Generation, claimByID[item.SessionID].Generation)
			}
			_, rejected, err = database.GetArtifactExportRejection(ctx, "rejected")
			require.NoError(err)
			assert.False(rejected)
			excluded, err := database.ArtifactExportClaims(
				ctx, []string{"foreign", "deleted"},
			)
			require.NoError(err)
			assert.Empty(excluded)
		})
	}
}

func TestArtifactOriginTransactionRollsBackQueuePopulationFailure(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	database := testDB(t)
	require.NoError(database.AdoptArtifactOrigin("before-a1b2c3"))
	require.NoError(database.UpsertSession(Session{
		ID: "session-a", Project: "project", Machine: "local", Agent: "claude",
	}))
	pending, err := database.PendingArtifactExports(t.Context(), 10)
	require.NoError(err)
	require.Len(pending, 1)
	require.NoError(database.AcknowledgeArtifactExports(t.Context(), pending))

	injected := errors.New("queue population failed")
	originalPopulate := populateArtifactOriginQueueTx
	populateArtifactOriginQueueTx = func(*sql.Tx, string, bool) error { return injected }
	t.Cleanup(func() { populateArtifactOriginQueueTx = originalPopulate })

	err = database.AdoptArtifactOrigin("after-d4e5f6")
	require.ErrorIs(err, injected)
	stored, err := database.GetSyncState("artifact_origin_id")
	require.NoError(err)
	assert.Equal("before-a1b2c3", stored)
	after, err := database.PendingArtifactExports(t.Context(), 10)
	require.NoError(err)
	assert.Empty(after,
		"failed adoption must leave the previous origin's clean queue unchanged")
}

func TestEnsureArtifactOriginRollsBackQueuePopulationFailure(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	database := testDB(t)
	require.NoError(database.UpsertSession(Session{
		ID: "session-a", Project: "project", Machine: "local", Agent: "claude",
	}))

	injected := errors.New("queue population failed")
	originalPopulate := populateArtifactOriginQueueTx
	populateArtifactOriginQueueTx = func(*sql.Tx, string, bool) error { return injected }
	t.Cleanup(func() { populateArtifactOriginQueueTx = originalPopulate })

	origin, err := database.EnsureArtifactOrigin("desk-a1b2c3")
	require.ErrorIs(err, injected)
	assert.Empty(origin)
	stored, err := database.GetSyncState("artifact_origin_id")
	require.NoError(err)
	assert.Empty(stored)
	pending, err := database.PendingArtifactExports(t.Context(), 10)
	require.NoError(err)
	assert.Empty(pending,
		"failed initialization must leave the export gate and queue unchanged")
}

func TestEnsureArtifactOriginSerializesAcrossDatabaseHandles(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	path := filepath.Join(t.TempDir(), "shared.db")
	const handleCount = 8
	handles := make([]*DB, handleCount)
	for i := range handles {
		database, err := Open(path)
		require.NoError(err)
		handles[i] = database
	}
	t.Cleanup(func() {
		for _, database := range handles {
			require.NoError(database.Close())
		}
	})

	start := make(chan struct{})
	type result struct {
		origin string
		err    error
	}
	results := make(chan result, handleCount)
	var ready sync.WaitGroup
	ready.Add(handleCount)
	for i, database := range handles {
		go func() {
			ready.Done()
			<-start
			origin, err := database.EnsureArtifactOrigin(
				fmt.Sprintf("desk-%d-a1b2c3", i),
			)
			results <- result{origin: origin, err: err}
		}()
	}
	ready.Wait()
	close(start)

	var winner string
	for range handleCount {
		got := <-results
		require.NoError(got.err)
		require.NotEmpty(got.origin)
		if winner == "" {
			winner = got.origin
		}
		assert.Equal(winner, got.origin)
	}
	stored, err := handles[0].GetSyncState("artifact_origin_id")
	require.NoError(err)
	assert.Equal(winner, stored)
}

func TestArtifactExportClaimsSelectsExactPendingSessionSet(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	database := testDB(t)
	seedArtifactOrigin(t, database)
	for _, id := range []string{"alpha", "bravo", "charlie"} {
		require.NoError(database.UpsertSession(Session{
			ID: id, Project: "project", Machine: "local", Agent: "claude",
		}))
	}

	claims, err := database.ArtifactExportClaims(t.Context(),
		[]string{"charlie", "missing", "alpha", "charlie"})
	require.NoError(err)
	require.Len(claims, 2)
	assert.Equal("alpha", claims[0].SessionID)
	assert.Equal("charlie", claims[1].SessionID)
	assert.Positive(claims[0].Generation)
	assert.Positive(claims[1].Generation)
}

func TestArtifactPublicationStaleClaimRollsBackEntireBatch(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	database := testDB(t)
	ctx := t.Context()
	seedArtifactOrigin(t, database)
	for _, id := range []string{"alpha", "bravo"} {
		require.NoError(database.UpsertSession(Session{
			ID: id, Project: "project", Machine: "local", Agent: "claude",
		}))
	}
	claimed, err := database.PendingArtifactExports(ctx, 10)
	require.NoError(err)
	require.Len(claimed, 2)

	_, err = database.getWriter().Exec(
		`UPDATE sessions SET display_name = 'newer' WHERE id = 'bravo'`,
	)
	require.NoError(err)
	_, _, err = database.ApplyArtifactPublicationChanges(ctx, "desktop-a1b2c3", []ArtifactPublicationChange{
		{SessionID: "alpha", Generation: claimed[0].Generation, ManifestHash: "alpha", SourceFingerprint: "alpha"},
		{SessionID: "bravo", Generation: claimed[1].Generation, ManifestHash: "bravo", SourceFingerprint: "bravo"},
	})
	require.ErrorIs(err, ErrArtifactExportClaimStale)

	var publications []ArtifactPublication
	_, err = database.StreamArtifactPublications(ctx, "desktop-a1b2c3", func(row ArtifactPublication) error {
		publications = append(publications, row)
		return nil
	})
	require.NoError(err)
	assert.Empty(publications, "a stale claim must not partially mutate publication state")

	fresh, err := database.PendingArtifactExports(ctx, 10)
	require.NoError(err)
	require.Len(fresh, 2)
	_, _, err = database.ApplyArtifactPublicationChanges(ctx, "desktop-a1b2c3", []ArtifactPublicationChange{
		{SessionID: fresh[0].SessionID, Generation: fresh[0].Generation, ManifestHash: "fresh-0", SourceFingerprint: "fresh-0"},
		{SessionID: fresh[1].SessionID, Generation: fresh[1].Generation, ManifestHash: "fresh-1", SourceFingerprint: "fresh-1"},
	})
	require.NoError(err)
	publications = nil
	_, err = database.StreamArtifactPublications(ctx, "desktop-a1b2c3", func(row ArtifactPublication) error {
		publications = append(publications, row)
		return nil
	})
	require.NoError(err)
	assert.Len(publications, 2, "fresh claims remain applicable after a stale retry")
}

func TestArtifactCheckpointHeadStaleClaimRollsBackHead(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	database := testDB(t)
	ctx := t.Context()
	seedArtifactOrigin(t, database)
	require.NoError(database.UpsertSession(Session{
		ID: "racing", Project: "project", Machine: "local", Agent: "claude",
	}))
	claimed, err := database.PendingArtifactExports(ctx, 1)
	require.NoError(err)
	require.Len(claimed, 1)
	_, err = database.getWriter().Exec(
		`UPDATE sessions SET display_name = 'newer' WHERE id = 'racing'`,
	)
	require.NoError(err)

	err = database.RecordArtifactCheckpointHead(ctx, ArtifactCheckpointHead{
		Origin: "desktop-a1b2c3", Sequence: 1,
		SessionMapSHA256: "map-1", CheckpointSHA256: "checkpoint-1",
	}, claimed)
	require.ErrorIs(err, ErrArtifactExportClaimStale)
	_, ok, err := database.GetArtifactCheckpointHead(ctx, "desktop-a1b2c3")
	require.NoError(err)
	assert.False(ok, "stale acknowledgement must roll back the checkpoint head")
	pending, err := database.PendingArtifactExports(ctx, 1)
	require.NoError(err)
	require.Len(pending, 1)
	assert.Greater(pending[0].Generation, claimed[0].Generation)
}

func TestArtifactPublicationRowsStreamInCanonicalOrder(t *testing.T) {
	require := require.New(t)

	database := testDB(t)
	ctx := t.Context()
	seedArtifactOrigin(t, database)
	for _, id := range []string{"zulu", "alpha"} {
		require.NoError(database.UpsertSession(Session{
			ID: id, Project: "project", Machine: "local", Agent: "claude",
		}))
	}
	claimed, err := database.PendingArtifactExports(ctx, 10)
	require.NoError(err)
	claimGeneration := map[string]int64{}
	for _, item := range claimed {
		claimGeneration[item.SessionID] = item.Generation
	}
	_, changed, err := database.ApplyArtifactPublicationChanges(ctx, "desktop-a1b2c3", []ArtifactPublicationChange{
		{SessionID: "zulu", Generation: claimGeneration["zulu"], ManifestHash: "hash-z", SourceFingerprint: "source-z"},
		{SessionID: "alpha", Generation: claimGeneration["alpha"], ManifestHash: "hash-a", SourceFingerprint: "source-a"},
	})
	require.NoError(err)
	require.True(changed)

	var got []string
	_, err = database.StreamArtifactPublications(ctx, "desktop-a1b2c3", func(row ArtifactPublication) error {
		got = append(got, row.SessionID+"="+row.ManifestHash)
		return nil
	})
	require.NoError(err)
	assert.Equal(t, []string{"alpha=hash-a", "zulu=hash-z"}, got)
}

func TestArtifactPublicationPageUsesCanonicalKeysetCursor(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	database := testDB(t)
	ctx := t.Context()
	seedArtifactOrigin(t, database)
	for _, id := range []string{"zulu", "bravo", "alpha"} {
		require.NoError(database.UpsertSession(Session{
			ID: id, Project: "project", Machine: "local", Agent: "claude",
		}))
	}
	claimed, err := database.PendingArtifactExports(ctx, 10)
	require.NoError(err)
	claimGeneration := make(map[string]int64, len(claimed))
	for _, item := range claimed {
		claimGeneration[item.SessionID] = item.Generation
	}
	revision, changed, err := database.ApplyArtifactPublicationChanges(
		ctx,
		"desktop-a1b2c3",
		[]ArtifactPublicationChange{
			{SessionID: "zulu", Generation: claimGeneration["zulu"], ManifestHash: "hash-z", SourceFingerprint: "source-z"},
			{SessionID: "bravo", Generation: claimGeneration["bravo"], ManifestHash: "hash-b", SourceFingerprint: "source-b"},
			{SessionID: "alpha", Generation: claimGeneration["alpha"], ManifestHash: "hash-a", SourceFingerprint: "source-a"},
		},
	)
	require.NoError(err)
	require.True(changed)

	first, firstRevision, more, err := database.ArtifactPublicationPage(
		ctx, "desktop-a1b2c3", "", 2,
	)
	require.NoError(err)
	assert.Equal(revision, firstRevision)
	assert.True(more)
	require.Len(first, 2)
	assert.Equal([]string{"alpha", "bravo"}, []string{
		first[0].SessionID,
		first[1].SessionID,
	})

	second, secondRevision, more, err := database.ArtifactPublicationPage(
		ctx, "desktop-a1b2c3", first[1].SessionID, 2,
	)
	require.NoError(err)
	assert.Equal(revision, secondRevision)
	assert.False(more)
	require.Len(second, 1)
	assert.Equal("zulu", second[0].SessionID)
}

func TestArtifactCheckpointHeadRejectsRegressionWithoutAcknowledgingWork(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	database := testDB(t)
	ctx := t.Context()
	seedArtifactOrigin(t, database)
	require.NoError(database.RecordArtifactCheckpointHead(ctx, ArtifactCheckpointHead{
		Origin: "desktop-a1b2c3", Sequence: 7,
		SessionMapSHA256: "map-7", CheckpointSHA256: "checkpoint-7",
	}, nil))
	require.NoError(database.UpsertSession(Session{
		ID: "still-pending", Project: "project", Machine: "local", Agent: "claude",
	}))

	pendingBefore, err := database.PendingArtifactExports(ctx, 10)
	require.NoError(err)
	err = database.RecordArtifactCheckpointHead(ctx, ArtifactCheckpointHead{
		Origin: "desktop-a1b2c3", Sequence: 6,
		SessionMapSHA256: "map-6", CheckpointSHA256: "checkpoint-6",
	}, pendingBefore)
	require.Error(err)

	pending, err := database.PendingArtifactExports(ctx, 10)
	require.NoError(err)
	require.Len(pending, 1)
	assert.Equal("still-pending", pending[0].SessionID)
	head, ok, err := database.GetArtifactCheckpointHead(ctx, "desktop-a1b2c3")
	require.NoError(err)
	require.True(ok)
	assert.Equal(7, head.Sequence)
}

func TestArtifactPublicationAcknowledgementCannotConsumeNewerMutation(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	database := testDB(t)
	ctx := t.Context()
	seedArtifactOrigin(t, database)
	require.NoError(database.UpsertSession(Session{
		ID: "racing", Project: "project", Machine: "local", Agent: "claude",
	}))
	claimed, err := database.PendingArtifactExports(ctx, 1)
	require.NoError(err)
	require.Len(claimed, 1)

	// Both writes can share the same SQLite millisecond. Generation, not wall
	// time, is the compare-and-ack token.
	_, err = database.getWriter().Exec(
		`UPDATE sessions SET display_name = 'newer' WHERE id = 'racing'`,
	)
	require.NoError(err)
	newer, err := database.PendingArtifactExports(ctx, 1)
	require.NoError(err)
	require.Len(newer, 1)
	assert.Equal(claimed[0].EnqueuedAt, newer[0].EnqueuedAt)
	assert.Greater(newer[0].Generation, claimed[0].Generation)

	err = database.RecordArtifactCheckpointHead(ctx, ArtifactCheckpointHead{
		Origin: "desktop-a1b2c3", Sequence: 1,
		SessionMapSHA256: "map-1", CheckpointSHA256: "checkpoint-1",
	}, claimed)
	require.ErrorIs(err, ErrArtifactExportClaimStale)
	pending, err := database.PendingArtifactExports(ctx, 1)
	require.NoError(err)
	require.Equal(newer, pending, "stale acknowledgment must leave newer work pending")
}

func TestArtifactPublicationQueueGenerationSurvivesAcknowledgementABA(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	database := testDB(t)
	ctx := t.Context()
	seedArtifactOrigin(t, database)
	require.NoError(database.UpsertSession(Session{
		ID: "aba", Project: "project", Machine: "local", Agent: "claude",
	}))
	oldClaim, err := database.PendingArtifactExports(ctx, 1)
	require.NoError(err)
	require.Len(oldClaim, 1)
	_, _, err = database.ApplyArtifactPublicationChanges(ctx, "desktop-a1b2c3", []ArtifactPublicationChange{{
		SessionID: "aba", Generation: oldClaim[0].Generation,
		ManifestHash: "old-manifest", SourceFingerprint: "old-source",
	}})
	require.NoError(err)
	require.NoError(database.AcknowledgeArtifactExports(ctx, oldClaim))
	pending, err := database.PendingArtifactExports(ctx, 1)
	require.NoError(err)
	assert.Empty(pending)

	_, err = database.getWriter().Exec(
		`UPDATE sessions SET display_name = 'new mutation' WHERE id = 'aba'`,
	)
	require.NoError(err)
	newClaim, err := database.PendingArtifactExports(ctx, 1)
	require.NoError(err)
	require.Len(newClaim, 1)
	_, err = database.getWriter().Exec(
		`UPDATE artifact_export_queue SET enqueued_at = ? WHERE session_id = 'aba'`,
		oldClaim[0].EnqueuedAt,
	)
	require.NoError(err)
	newClaim, err = database.PendingArtifactExports(ctx, 1)
	require.NoError(err)
	require.Len(newClaim, 1)
	assert.Equal(oldClaim[0].EnqueuedAt, newClaim[0].EnqueuedAt,
		"the ABA guard cannot depend on timestamp precision")
	assert.Greater(newClaim[0].Generation, oldClaim[0].Generation)

	revision, _, err := database.ApplyArtifactPublicationChanges(ctx, "desktop-a1b2c3", []ArtifactPublicationChange{{
		SessionID: "aba", Generation: newClaim[0].Generation,
		ManifestHash: "new-manifest", SourceFingerprint: "new-source",
	}})
	require.NoError(err)
	require.NoError(database.RecordArtifactCheckpointHead(ctx, ArtifactCheckpointHead{
		Origin: "desktop-a1b2c3", Sequence: 1, PublicationRevision: revision,
		SessionMapSHA256: "map-1", CheckpointSHA256: "checkpoint-1",
	}, nil))

	_, _, err = database.ApplyArtifactPublicationChanges(ctx, "desktop-a1b2c3", []ArtifactPublicationChange{{
		SessionID: "aba", Generation: oldClaim[0].Generation,
		ManifestHash: "stale-manifest", SourceFingerprint: "stale-source",
	}})
	require.ErrorIs(err, ErrArtifactExportClaimStale)
	err = database.RecordArtifactCheckpointHead(ctx, ArtifactCheckpointHead{
		Origin: "desktop-a1b2c3", Sequence: 2,
		SessionMapSHA256: "stale-map", CheckpointSHA256: "stale-checkpoint",
	}, oldClaim)
	require.ErrorIs(err, ErrArtifactExportClaimStale)
	require.ErrorIs(database.AcknowledgeArtifactExports(ctx, oldClaim), ErrArtifactExportClaimStale)

	var publications []ArtifactPublication
	_, err = database.StreamArtifactPublications(ctx, "desktop-a1b2c3", func(row ArtifactPublication) error {
		publications = append(publications, row)
		return nil
	})
	require.NoError(err)
	require.Len(publications, 1)
	assert.Equal("new-manifest", publications[0].ManifestHash)
	head, ok, err := database.GetArtifactCheckpointHead(ctx, "desktop-a1b2c3")
	require.NoError(err)
	require.True(ok)
	assert.Equal(1, head.Sequence)
	pending, err = database.PendingArtifactExports(ctx, 1)
	require.NoError(err)
	assert.Equal(newClaim, pending)
}

func TestArtifactPublicationUsageOnlyAtomicBatchEnqueuesExactlyOnce(t *testing.T) {
	for _, machine := range []string{"local", "peer-a1b2c3"} {
		t.Run(machine, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)

			database := testDB(t)
			seedArtifactOrigin(t, database)
			session := Session{
				ID: "usage-batch", Project: "project", Machine: machine, Agent: "claude",
			}
			require.NoError(database.UpsertSession(session))
			if machine == "local" {
				claim, err := database.PendingArtifactExports(t.Context(), 1)
				require.NoError(err)
				require.NoError(database.AcknowledgeArtifactExports(t.Context(), claim))
			}
			stored, err := database.GetSessionFull(t.Context(), session.ID)
			require.NoError(err)
			require.NotNil(stored)

			_, err = database.WriteSessionBatchAtomic([]SessionBatchWrite{{
				Session: *stored,
				UsageEvents: []UsageEvent{{
					SessionID: session.ID, Source: "event", Model: "model",
					InputTokens: 10, OutputTokens: 2, DedupKey: "changed",
				}},
				ReplaceMessages: false,
			}})
			require.NoError(err)
			pending, err := database.PendingArtifactExports(t.Context(), 10)
			require.NoError(err)
			if machine != "local" {
				assert.Empty(pending)
				return
			}
			require.Len(pending, 1)
			assert.Equal(int64(2), pending[0].Generation,
				"usage-only batch advances the clean authority exactly once")
		})
	}
}

func TestArtifactClaimErrorsRemainDiscoverableWhenWrapped(t *testing.T) {
	assert.True(t, errors.Is(fmt.Errorf("retry: %w", ErrArtifactExportClaimStale), ErrArtifactExportClaimStale))
}

func TestArtifactCheckpointFloorReservationIsConcurrentAndNeverLowers(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	path := filepath.Join(t.TempDir(), "floor.db")
	first, err := Open(path)
	require.NoError(err)
	t.Cleanup(func() { require.NoError(first.Close()) })
	second, err := Open(path)
	require.NoError(err)
	t.Cleanup(func() { require.NoError(second.Close()) })

	const reservations = 24
	sequences := make(chan int, reservations)
	errs := make(chan error, reservations)
	var wg sync.WaitGroup
	for i := range reservations {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			database := first
			if i%2 == 1 {
				database = second
			}
			sequence, reserveErr := database.ReserveArtifactCheckpointSequence(
				t.Context(), "desktop-a1b2c3", 10,
			)
			if reserveErr != nil {
				errs <- reserveErr
				return
			}
			sequences <- sequence
		}(i)
	}
	wg.Wait()
	close(errs)
	for reserveErr := range errs {
		require.NoError(reserveErr)
	}
	close(sequences)
	var got []int
	for sequence := range sequences {
		got = append(got, sequence)
	}
	sort.Ints(got)
	want := make([]int, reservations)
	for i := range want {
		want[i] = 11 + i
	}
	assert.Equal(want, got)

	// Simulate a crash after the committed floor reservation but before the
	// checkpoint node is created, followed by a vault reset reporting no live
	// sequence. The durable floor consumes the missing sequence permanently.
	next, err := first.ReserveArtifactCheckpointSequence(t.Context(), "desktop-a1b2c3", 0)
	require.NoError(err)
	assert.Equal(11+reservations, next)
}

func TestArtifactCheckpointHeadAdvancesSequenceFloor(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	database := testDB(t)
	ctx := t.Context()
	require.NoError(database.RecordArtifactCheckpointHead(
		ctx, ArtifactCheckpointHead{
			Origin: "desktop-a1b2c3", Sequence: 7,
			SessionMapSHA256: "map", CheckpointSHA256: "checkpoint",
		}, nil,
	))

	floor, ok, err := database.GetArtifactCheckpointFloor(
		ctx, "desktop-a1b2c3",
	)
	require.NoError(err)
	require.True(ok)
	assert.Equal(7, floor)
	next, err := database.ReserveArtifactCheckpointSequence(
		ctx, "desktop-a1b2c3", 0,
	)
	require.NoError(err)
	assert.Equal(8, next)
}

func TestOpenBackfillsCheckpointFloorFromLegacyHeadWhenStoreIsEmpty(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	path := filepath.Join(t.TempDir(), "legacy-head.db")
	database, err := Open(path)
	require.NoError(err)
	require.NoError(database.RecordArtifactCheckpointHead(
		t.Context(), ArtifactCheckpointHead{
			Origin: "desktop-a1b2c3", Sequence: 7,
			SessionMapSHA256: "map", CheckpointSHA256: "checkpoint",
		}, nil,
	))
	_, err = database.getWriter().Exec(`
		DELETE FROM artifact_checkpoint_floors
		WHERE origin = 'desktop-a1b2c3'`)
	require.NoError(err)
	require.NoError(database.Close())

	reopened, err := Open(path)
	require.NoError(err)
	t.Cleanup(func() { require.NoError(reopened.Close()) })
	floor, ok, err := reopened.GetArtifactCheckpointFloor(
		t.Context(), "desktop-a1b2c3",
	)
	require.NoError(err)
	require.True(ok)
	assert.Equal(7, floor)
	// observedFloor=0 models recovery against an empty or reset artifact
	// store: the migrated database head remains the sequence authority.
	next, err := reopened.ReserveArtifactCheckpointSequence(
		t.Context(), "desktop-a1b2c3", 0,
	)
	require.NoError(err)
	assert.Equal(8, next)
}

func TestArtifactPublicationStateSurvivesFullResyncCopy(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	dir := t.TempDir()
	sourcePath := filepath.Join(dir, "source.db")
	source, err := Open(sourcePath)
	require.NoError(err)
	seedArtifactOrigin(t, source)

	ctx := t.Context()
	require.NoError(source.UpsertSession(Session{
		ID: "queued", Project: "project", Machine: "local", Agent: "claude",
	}))
	require.NoError(source.UpsertSession(Session{
		ID: "published", Project: "project", Machine: "local", Agent: "claude",
	}))
	claims, err := source.PendingArtifactExports(ctx, 10)
	require.NoError(err)
	var publishedClaim ArtifactExportQueueItem
	for _, claim := range claims {
		if claim.SessionID == "published" {
			publishedClaim = claim
		}
	}
	require.NotZero(publishedClaim.Generation)
	revision, _, err := source.ApplyArtifactPublicationChanges(ctx, "desktop-a1b2c3", []ArtifactPublicationChange{{
		SessionID: "published", Generation: publishedClaim.Generation,
		ManifestHash: "manifest", SourceFingerprint: "source",
	}})
	require.NoError(err)
	require.NoError(source.AcknowledgeArtifactExports(ctx, []ArtifactExportQueueItem{publishedClaim}))
	require.NoError(source.RecordArtifactCheckpointHead(ctx, ArtifactCheckpointHead{
		Origin: "desktop-a1b2c3", Sequence: 4, PublicationRevision: revision,
		SessionMapSHA256: "map", CheckpointSHA256: "checkpoint",
	}, nil))
	sequence, err := source.ReserveArtifactCheckpointSequence(ctx, "desktop-a1b2c3", 8)
	require.NoError(err)
	require.Equal(9, sequence)
	require.NoError(source.Close())

	target, err := Open(filepath.Join(dir, "target.db"))
	require.NoError(err)
	t.Cleanup(func() { require.NoError(target.Close()) })
	require.NoError(target.CopySyncStateFrom(sourcePath))

	// A resync re-verifies every copied session, so the acknowledged
	// "published" row is re-dirtied alongside the still-pending "queued" row.
	pending, err := target.PendingArtifactExports(ctx, 10)
	require.NoError(err)
	require.Len(pending, 2)
	assert.ElementsMatch([]string{"queued", "published"}, []string{
		pending[0].SessionID, pending[1].SessionID,
	})
	var publications []ArtifactPublication
	streamedRevision, err := target.StreamArtifactPublications(ctx, "desktop-a1b2c3", func(row ArtifactPublication) error {
		publications = append(publications, row)
		return nil
	})
	require.NoError(err)
	assert.Equal(revision, streamedRevision)
	require.Len(publications, 1)
	assert.Equal("published", publications[0].SessionID)
	head, ok, err := target.GetArtifactCheckpointHead(ctx, "desktop-a1b2c3")
	require.NoError(err)
	require.True(ok)
	assert.Equal(4, head.Sequence)
	next, err := target.ReserveArtifactCheckpointSequence(ctx, "desktop-a1b2c3", 0)
	require.NoError(err)
	assert.Equal(10, next)

	// Clean authority rows survive the swap and copied generations are advanced,
	// so an in-flight claim against the source database cannot become valid in
	// the replacement archive after the same session is dirtied again.
	require.NoError(target.UpsertSession(Session{
		ID: "published", Project: "project", Machine: "local", Agent: "claude",
	}))
	pending, err = target.PendingArtifactExports(ctx, 10)
	require.NoError(err)
	var republished ArtifactExportQueueItem
	for _, item := range pending {
		if item.SessionID == "published" {
			republished = item
		}
	}
	require.Greater(republished.Generation, publishedClaim.Generation)
	_, _, err = target.ApplyArtifactPublicationChanges(ctx, "desktop-a1b2c3", []ArtifactPublicationChange{{
		SessionID: "published", Generation: publishedClaim.Generation,
		ManifestHash: "stale", SourceFingerprint: "stale",
	}})
	require.ErrorIs(err, ErrArtifactExportClaimStale)
}

// TestCopySyncStateForcesPendingOnFreshCopiedQueueRows models the shipped
// resync flow: the old DB holds acknowledged (pending=0) queue rows, the fresh
// DB has no origin key so the triggers stay gated and the queue is empty, and
// CopySyncStateFrom must re-dirty every copied row so the exporter re-verifies
// the rebuilt archive. Covers the fresh-insert branch of the queue copy.
func TestCopySyncStateForcesPendingOnFreshCopiedQueueRows(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	dir := t.TempDir()
	sourcePath := filepath.Join(dir, "source.db")
	source, err := Open(sourcePath)
	require.NoError(err)
	seedArtifactOrigin(t, source)
	ctx := t.Context()

	for _, id := range []string{"sess-1", "sess-2"} {
		require.NoError(source.UpsertSession(Session{
			ID: id, Project: "project", Machine: "local", Agent: "claude",
		}))
	}
	claims, err := source.PendingArtifactExports(ctx, 10)
	require.NoError(err)
	require.Len(claims, 2)
	require.NoError(source.AcknowledgeArtifactExports(ctx, claims))
	drained, err := source.PendingArtifactExports(ctx, 10)
	require.NoError(err)
	require.Empty(drained, "old queue rows are acknowledged before the resync")
	oldGen := map[string]int64{}
	for _, claim := range claims {
		oldGen[claim.SessionID] = claim.Generation
	}
	require.NoError(source.Close())

	target, err := Open(filepath.Join(dir, "target.db"))
	require.NoError(err)
	t.Cleanup(func() { require.NoError(target.Close()) })
	for _, id := range []string{"sess-1", "sess-2"} {
		require.NoError(target.UpsertSession(Session{
			ID: id, Project: "project", Machine: "local", Agent: "claude",
		}))
	}
	emptyBefore, err := target.PendingArtifactExports(ctx, 10)
	require.NoError(err)
	require.Empty(emptyBefore, "no origin key: queue triggers stay gated during resync")

	require.NoError(target.CopySyncStateFrom(sourcePath))

	pending, err := target.PendingArtifactExports(ctx, 10)
	require.NoError(err)
	require.Len(pending, 2, "a resync must re-verify every copied session")
	assert.ElementsMatch([]string{"sess-1", "sess-2"}, []string{
		pending[0].SessionID, pending[1].SessionID,
	})
	for _, item := range pending {
		assert.Greater(item.Generation, oldGen[item.SessionID],
			"copied generation must advance and never regress")
	}
}

func TestCopySyncStateQueuesSessionsDiscoveredDuringRebuild(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	dir := t.TempDir()
	sourcePath := filepath.Join(dir, "source.db")
	source, err := Open(sourcePath)
	require.NoError(err)
	seedArtifactOrigin(t, source)
	ctx := t.Context()

	require.NoError(source.UpsertSession(Session{
		ID: "existing", Project: "project", Machine: "local", Agent: "claude",
	}))
	sourceClaims, err := source.PendingArtifactExports(ctx, 10)
	require.NoError(err)
	require.Len(sourceClaims, 1)
	require.NoError(source.AcknowledgeArtifactExports(ctx, sourceClaims))
	require.NoError(source.Close())

	target, err := Open(filepath.Join(dir, "target.db"))
	require.NoError(err)
	t.Cleanup(func() { require.NoError(target.Close()) })
	for _, id := range []string{"existing", "discovered-during-rebuild"} {
		require.NoError(target.UpsertSession(Session{
			ID: id, Project: "project", Machine: "local", Agent: "claude",
		}))
	}
	before, err := target.PendingArtifactExports(ctx, 10)
	require.NoError(err)
	require.Empty(before, "the rebuild has no origin until sync state is restored")

	require.NoError(target.CopySyncStateFrom(sourcePath))

	pending, err := target.PendingArtifactExports(ctx, 10)
	require.NoError(err)
	require.Len(pending, 2)
	generations := make(map[string]int64, len(pending))
	for _, item := range pending {
		generations[item.SessionID] = item.Generation
	}
	assert.Equal(sourceClaims[0].Generation+1, generations["existing"],
		"queue completion must preserve the copied ledger generation")
	assert.Equal(int64(1), generations["discovered-during-rebuild"],
		"a rebuilt-only session must receive a fresh queue row")
}

// TestCopySyncStateMergesQueueRowsAsPending exercises the ON CONFLICT merge
// branch of the queue copy (previously untested). The fresh DB already has the
// origin key and a trigger-created row that has itself been acknowledged, and
// the old DB carries an acknowledged row for the same session at a different
// generation. After the copy the row is pending again and its generation is the
// advanced maximum of both sources.
func TestCopySyncStateMergesQueueRowsAsPending(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	dir := t.TempDir()
	sourcePath := filepath.Join(dir, "source.db")
	source, err := Open(sourcePath)
	require.NoError(err)
	seedArtifactOrigin(t, source)
	ctx := t.Context()

	require.NoError(source.UpsertSession(Session{
		ID: "overlap", Project: "project", Machine: "local", Agent: "claude",
	}))
	for i := range 3 {
		_, err = source.getWriter().Exec(
			`UPDATE sessions SET display_name = ? WHERE id = 'overlap'`, "v"+strconv.Itoa(i))
		require.NoError(err)
	}
	oldClaims, err := source.PendingArtifactExports(ctx, 10)
	require.NoError(err)
	require.Len(oldClaims, 1)
	oldGen := oldClaims[0].Generation
	require.Greater(oldGen, int64(1))
	require.NoError(source.AcknowledgeArtifactExports(ctx, oldClaims))
	require.NoError(source.Close())

	target, err := Open(filepath.Join(dir, "target.db"))
	require.NoError(err)
	t.Cleanup(func() { require.NoError(target.Close()) })
	seedArtifactOrigin(t, target)
	require.NoError(target.UpsertSession(Session{
		ID: "overlap", Project: "project", Machine: "local", Agent: "claude",
	}))
	newClaims, err := target.PendingArtifactExports(ctx, 10)
	require.NoError(err)
	require.Len(newClaims, 1)
	newGen := newClaims[0].Generation
	require.NoError(target.AcknowledgeArtifactExports(ctx, newClaims))
	acked, err := target.PendingArtifactExports(ctx, 10)
	require.NoError(err)
	require.Empty(acked, "both queue rows are acknowledged before the merge")

	require.NoError(target.CopySyncStateFrom(sourcePath))

	pending, err := target.PendingArtifactExports(ctx, 10)
	require.NoError(err)
	require.Len(pending, 1, "the merged row is re-dirtied by the resync")
	assert.Equal("overlap", pending[0].SessionID)
	expected := max(oldGen+1, newGen) + 1
	assert.Equal(expected, pending[0].Generation,
		"merged generation is the advanced maximum of both sources")
}

func TestArtifactPublicationStateCopyAcceptsPreRevisionCheckpointHead(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	dir := t.TempDir()
	sourcePath := filepath.Join(dir, "legacy.db")
	legacy, err := sql.Open("sqlite3", sourcePath)
	require.NoError(err)
	_, err = legacy.ExecContext(t.Context(), `
		CREATE TABLE artifact_checkpoint_heads (
			origin TEXT PRIMARY KEY,
			sequence INTEGER NOT NULL,
			session_map_sha256 TEXT NOT NULL,
			checkpoint_sha256 TEXT NOT NULL
		);
		INSERT INTO artifact_checkpoint_heads VALUES (
			'desktop-a1b2c3', 4, 'map', 'checkpoint'
		);`)
	require.NoError(err)
	require.NoError(legacy.Close())

	target := testDB(t)
	require.NoError(target.CopySyncStateFrom(sourcePath))
	head, ok, err := target.GetArtifactCheckpointHead(t.Context(), "desktop-a1b2c3")
	require.NoError(err)
	require.True(ok)
	assert.Equal(4, head.Sequence)
	assert.Zero(head.PublicationRevision)
	assert.Zero(head.CheckpointSize)
	floor, ok, err := target.GetArtifactCheckpointFloor(
		t.Context(), "desktop-a1b2c3",
	)
	require.NoError(err)
	require.True(ok)
	assert.Equal(4, floor)
	next, err := target.ReserveArtifactCheckpointSequence(
		t.Context(), "desktop-a1b2c3", 0,
	)
	require.NoError(err)
	assert.Equal(5, next)
}

func TestArtifactPublicationExportCardinalityIsQueueBounded(t *testing.T) {
	for _, unrelated := range []int{20, 2000} {
		t.Run(strconv.Itoa(unrelated), func(t *testing.T) {
			require := require.New(t)

			database := testDB(t)
			seedArtifactOrigin(t, database)
			for i := range unrelated {
				_, err := database.getWriter().Exec(
					`INSERT INTO sessions (id, project, machine, agent)
					 VALUES (?, 'project', 'peer-a1b2c3', 'claude')`,
					"peer-"+strconv.Itoa(i),
				)
				require.NoError(err)
			}
			require.NoError(database.UpsertSession(Session{
				ID: "dirty", Project: "project", Machine: "local", Agent: "claude",
			}))
			pending, err := database.PendingArtifactExports(t.Context(), 1)
			require.NoError(err)
			require.Len(pending, 1)
			assert.Equal(t, "dirty", pending[0].SessionID)
		})
	}
}

func TestArtifactPublicationQueueTracksOnlyLocallyOwnedContent(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	database := testDB(t)
	seedArtifactOrigin(t, database)

	require.NoError(database.UpsertSession(Session{
		ID: "local-session", Project: "project", Machine: "local", Agent: "claude",
	}))
	require.Equal([]string{"local-session"}, artifactExportQueueIDs(t, database))
	clearArtifactExportQueue(t, database)

	require.NoError(database.UpsertSession(Session{
		ID: "peer-session", Project: "project", Machine: "peer-a1b2c3", Agent: "claude",
	}))
	assert.Empty(artifactExportQueueIDs(t, database), "foreign inserts stay out of the local publication queue")

	_, err := database.getWriter().Exec(
		`UPDATE sessions SET display_name = 'renamed' WHERE id = 'local-session'`,
	)
	require.NoError(err)
	require.Equal([]string{"local-session"}, artifactExportQueueIDs(t, database))
	clearArtifactExportQueue(t, database)

	_, err = database.getWriter().Exec(
		`UPDATE sessions SET machine = 'peer-a1b2c3' WHERE id = 'local-session'`,
	)
	require.NoError(err)
	require.Equal([]string{"local-session"}, artifactExportQueueIDs(t, database),
		"local-to-foreign transition must publish removal")
	clearArtifactExportQueue(t, database)

	_, err = database.getWriter().Exec(
		`UPDATE sessions SET machine = 'local' WHERE id = 'peer-session'`,
	)
	require.NoError(err)
	require.Equal([]string{"peer-session"}, artifactExportQueueIDs(t, database),
		"foreign-to-local transition must publish content")
	clearArtifactExportQueue(t, database)

	_, err = database.getWriter().Exec(
		`UPDATE sessions SET display_name = 'peer rename' WHERE id = 'local-session'`,
	)
	require.NoError(err)
	assert.Empty(artifactExportQueueIDs(t, database), "unchanged foreign updates stay out of the queue")
}

func TestArtifactPublicationQueueKeepsFirstDirtyTimeForFIFO(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	database := testDB(t)
	seedArtifactOrigin(t, database)
	require.NoError(database.UpsertSession(Session{
		ID: "dirty", Project: "project", Machine: "local", Agent: "claude",
	}))
	const firstDirty = "2026-01-02T03:04:05.000Z"
	_, err := database.getWriter().Exec(
		`UPDATE artifact_export_queue SET enqueued_at = ? WHERE session_id = 'dirty'`,
		firstDirty,
	)
	require.NoError(err)

	_, err = database.getWriter().Exec(
		`UPDATE sessions SET display_name = 'changed again' WHERE id = 'dirty'`,
	)
	require.NoError(err)
	pending, err := database.PendingArtifactExports(t.Context(), 1)
	require.NoError(err)
	require.Len(pending, 1)
	assert.Equal(firstDirty, pending[0].EnqueuedAt,
		"repeated changes retain FIFO position until acknowledgement")
	assert.Greater(pending[0].Generation, int64(1),
		"repeated changes advance the compare-and-ack generation")
}

func TestArtifactPublicationQueueRefreshesDirtyTimeOnlyAfterAcknowledgement(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	database := testDB(t)
	seedArtifactOrigin(t, database)
	require.NoError(database.UpsertSession(Session{
		ID: "dirty", Project: "project", Machine: "local", Agent: "claude",
	}))
	const oldDirty = "2020-01-02T03:04:05.000Z"
	_, err := database.getWriter().Exec(
		`UPDATE artifact_export_queue SET enqueued_at = ? WHERE session_id = 'dirty'`,
		oldDirty,
	)
	require.NoError(err)
	claim, err := database.PendingArtifactExports(t.Context(), 1)
	require.NoError(err)
	require.NoError(database.AcknowledgeArtifactExports(t.Context(), claim))

	_, err = database.getWriter().Exec(
		`UPDATE sessions SET display_name = 'first clean mutation' WHERE id = 'dirty'`,
	)
	require.NoError(err)
	pending, err := database.PendingArtifactExports(t.Context(), 1)
	require.NoError(err)
	require.Len(pending, 1)
	assert.NotEqual(oldDirty, pending[0].EnqueuedAt,
		"clean-to-pending transition receives a fresh FIFO position")
	firstDirty := pending[0].EnqueuedAt

	_, err = database.getWriter().Exec(
		`UPDATE sessions SET display_name = 'second pending mutation' WHERE id = 'dirty'`,
	)
	require.NoError(err)
	pending, err = database.PendingArtifactExports(t.Context(), 1)
	require.NoError(err)
	require.Len(pending, 1)
	assert.Equal(firstDirty, pending[0].EnqueuedAt,
		"repeated pending writes retain their FIFO position")
}

func TestArtifactPublicationQueueTracksMessagesUsageAndCascadeDeletion(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	database := testDB(t)
	seedArtifactOrigin(t, database)
	for _, session := range []Session{
		{ID: "local-session", Project: "project", Machine: "local", Agent: "claude"},
		{ID: "peer-session", Project: "project", Machine: "peer-a1b2c3", Agent: "claude"},
	} {
		require.NoError(database.UpsertSession(session))
	}
	clearArtifactExportQueue(t, database)

	require.NoError(database.InsertMessages([]Message{
		{SessionID: "local-session", Ordinal: 0, Role: "user", Content: "local"},
		{SessionID: "peer-session", Ordinal: 0, Role: "user", Content: "peer"},
	}))
	require.Equal([]string{"local-session"}, artifactExportQueueIDs(t, database))
	pending, err := database.PendingArtifactExports(t.Context(), 1)
	require.NoError(err)
	require.Len(pending, 1)
	assert.Equal(int64(2), pending[0].Generation,
		"InsertMessages enqueues once for the owning session")
	clearArtifactExportQueue(t, database)

	require.NoError(database.ReplaceSessionUsageEvents("local-session", []UsageEvent{{
		SessionID: "local-session", Source: "event", Model: "model", DedupKey: "local",
	}}))
	require.NoError(database.ReplaceSessionUsageEvents("peer-session", []UsageEvent{{
		SessionID: "peer-session", Source: "event", Model: "model", DedupKey: "peer",
	}}))
	require.Equal([]string{"local-session"}, artifactExportQueueIDs(t, database))
	pending, err = database.PendingArtifactExports(t.Context(), 1)
	require.NoError(err)
	require.Len(pending, 1)
	assert.Equal(int64(3), pending[0].Generation,
		"usage replacement enqueues once independently of event rows")
	clearArtifactExportQueue(t, database)

	_, err = database.getWriter().Exec(`DELETE FROM sessions WHERE id = 'local-session'`)
	require.NoError(err)
	require.Equal([]string{"local-session"}, artifactExportQueueIDs(t, database),
		"the owner signal must survive child-row cascade ordering")
}

func TestArtifactPublicationQueueMessageReplacementIsBatchBounded(t *testing.T) {
	for _, count := range []int{2, 2000} {
		t.Run(strconv.Itoa(count), func(t *testing.T) {
			require := require.New(t)

			database := testDB(t)
			seedArtifactOrigin(t, database)
			require.NoError(database.UpsertSession(Session{
				ID: "session", Project: "project", Machine: "local", Agent: "claude",
			}))
			clearArtifactExportQueue(t, database)
			messages := make([]Message, count)
			for i := range messages {
				messages[i] = Message{
					SessionID: "session", Ordinal: i, Role: "user",
					Content: "message " + strconv.Itoa(i),
				}
			}
			require.NoError(database.ReplaceSessionMessages("session", messages))
			pending, err := database.PendingArtifactExports(t.Context(), 1)
			require.NoError(err)
			require.Len(pending, 1)
			assert.Equal(t, int64(2), pending[0].Generation,
				"one transaction advances the queue independently of message count")
		})
	}
}

func TestArtifactPublicationQueueTracksMetadataOnlyStandaloneReplacements(t *testing.T) {
	for _, test := range []struct {
		name    string
		replace func(*DB, string, []Message) error
	}{
		{
			name: "messages",
			replace: func(database *DB, sessionID string, messages []Message) error {
				return database.ReplaceSessionMessages(sessionID, messages)
			},
		},
		{
			name: "content",
			replace: func(database *DB, sessionID string, messages []Message) error {
				return database.ReplaceSessionContent(
					sessionID, messages, SessionSignalUpdate{}, nil,
				)
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)

			database := testDB(t)
			seedArtifactOrigin(t, database)
			require.NoError(database.UpsertSession(Session{
				ID: "session", Project: "project", Machine: "local", Agent: "claude",
			}))
			original := Message{
				SessionID: "session", Ordinal: 0, Role: "assistant",
				Content: "unchanged", ContentLength: len("unchanged"),
			}
			require.NoError(test.replace(database, "session", []Message{original}))
			clearArtifactExportQueue(t, database)

			var generationBefore int64
			require.NoError(database.getReader().QueryRow(`
				SELECT generation FROM artifact_export_queue WHERE session_id = 'session'`,
			).Scan(&generationBefore))

			metadataOnly := original
			metadataOnly.ContentLength = 999
			metadataOnly.TokenUsage = []byte(`{"input_tokens":17}`)
			metadataOnly.ClaudeRequestID = "request-2"
			metadataOnly.SourceUUID = "source-2"
			require.NoError(test.replace(database, "session", []Message{metadataOnly}))

			pending, err := database.PendingArtifactExports(t.Context(), 10)
			require.NoError(err)
			require.Len(pending, 1)
			assert.Equal("session", pending[0].SessionID)
			assert.Equal(generationBefore+1, pending[0].Generation)
			stored, err := database.GetAllMessages(t.Context(), "session")
			require.NoError(err)
			require.Len(stored, 1)
			assert.Equal(metadataOnly.TokenUsage, stored[0].TokenUsage)
			assert.Equal(metadataOnly.ClaudeRequestID, stored[0].ClaudeRequestID)
			assert.Equal(metadataOnly.SourceUUID, stored[0].SourceUUID)
		})
	}
}

func TestArtifactPublicationQueueIgnoresSessionBookkeepingUpdates(t *testing.T) {
	database := testDB(t)
	seedArtifactOrigin(t, database)
	require.NoError(t, database.UpsertSession(Session{
		ID: "session", Project: "project", Machine: "local", Agent: "claude",
	}))
	clearArtifactExportQueue(t, database)
	_, err := database.getWriter().Exec(`
		UPDATE sessions SET
			file_path = '/tmp/local', file_size = 42, file_mtime = 43,
			next_ordinal = 9, last_entry_uuid = 'uuid', file_inode = 44,
			file_device = 45, file_hash = 'hash', local_modified_at = 'now',
			last_write_incremental = 1, secrets_rules_version = 'rules',
			secret_leak_count = 2, sync_marker = 'marker'
		WHERE id = 'session'`)
	require.NoError(t, err)
	assert.Empty(t, artifactExportQueueIDs(t, database))
}

// TestArtifactPublicationQueueEnqueuesSessionKindOnlyUpdate pins
// session_kind into the artifact_sessions_update_queue trigger's
// change-detection list: the field is export-visible via the manifest, so a
// session-kind-only update on an already-published session must re-enqueue
// its export.
func TestArtifactPublicationQueueEnqueuesSessionKindOnlyUpdate(t *testing.T) {
	database := testDB(t)
	seedArtifactOrigin(t, database)
	require.NoError(t, database.UpsertSession(Session{
		ID: "session", Project: "project", Machine: "local", Agent: "claude",
	}))
	clearArtifactExportQueue(t, database)

	_, err := database.getWriter().Exec(
		`UPDATE sessions SET session_kind = 'bg' WHERE id = 'session'`)
	require.NoError(t, err)
	assert.Equal(t, []string{"session"}, artifactExportQueueIDs(t, database),
		"a session-kind-only update must re-enqueue the session for export")
}

// TestArtifactExportQueueStaysEmptyWithoutOrigin covers the deviation-1
// origin gate: an archive that has never created or adopted an artifact
// origin must never populate artifact_export_queue, regardless of how many
// locally-owned sessions are inserted, updated, or deleted.
func TestArtifactExportQueueStaysEmptyWithoutOrigin(t *testing.T) {
	database := testDB(t)
	require.NoError(t, database.UpsertSession(Session{
		ID: "no-origin", Project: "project", Machine: "local", Agent: "claude",
	}))
	assertArtifactExportQueueEmpty(t, database, "insert")

	_, err := database.getWriter().Exec(
		`UPDATE sessions SET display_name = 'changed' WHERE id = 'no-origin'`,
	)
	require.NoError(t, err)
	assertArtifactExportQueueEmpty(t, database, "update")

	_, err = database.getWriter().Exec(`DELETE FROM sessions WHERE id = 'no-origin'`)
	require.NoError(t, err)
	assertArtifactExportQueueEmpty(t, database, "delete")
}

// TestArtifactExportQueueHooksRespectOriginGate covers the session_batch.go
// and usage_events.go enqueue hooks (artifactExportGenerationTx /
// enqueueArtifactExportTx), as distinct from the sessions-table triggers
// covered by TestArtifactExportQueueStaysEmptyWithoutOrigin: a batch write
// must not populate the queue before an artifact origin exists, and the
// identical write must enqueue once an origin is seeded.
func TestArtifactExportQueueHooksRespectOriginGate(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	database := testDB(t)
	write := SessionBatchWrite{
		Session: Session{
			ID: "gated-session", Project: "project", Machine: "local", Agent: "claude",
		},
		Messages: []Message{
			{SessionID: "gated-session", Ordinal: 0, Role: "user", Content: "hi"},
		},
	}
	result, err := database.WriteSessionBatch([]SessionBatchWrite{write})
	require.NoError(err)
	require.Equal(1, result.WrittenSessions)
	assert.Empty(artifactExportQueueIDs(t, database),
		"a batch write must not populate the export queue before an artifact origin exists")

	seedArtifactOrigin(t, database)
	write.Session.ID = "gated-session-2"
	write.Messages[0].SessionID = "gated-session-2"
	result, err = database.WriteSessionBatch([]SessionBatchWrite{write})
	require.NoError(err)
	require.Equal(1, result.WrittenSessions)
	assert.Equal([]string{"gated-session-2"}, artifactExportQueueIDs(t, database),
		"after an artifact origin exists, the identical batch write enqueues the session")
}

// TestReplaceSessionUsageEventsEnqueuesArtifactExport covers the standalone
// ReplaceSessionUsageEvents path (usage_events.go, enqueueArtifact=true),
// as distinct from the batch path (WriteSessionBatch passes false and relies
// on its own end-of-batch generation check; see
// TestArtifactPublicationUsageOnlyAtomicBatchEnqueuesExactlyOnce).
func TestReplaceSessionUsageEventsEnqueuesArtifactExport(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	database := testDB(t)
	seedArtifactOrigin(t, database)
	require.NoError(database.UpsertSession(Session{
		ID: "usage-standalone", Project: "project", Machine: "local", Agent: "claude",
	}))
	clearArtifactExportQueue(t, database)

	require.NoError(database.ReplaceSessionUsageEvents("usage-standalone", []UsageEvent{{
		SessionID: "usage-standalone", Source: "event", Model: "model", DedupKey: "standalone",
	}}))
	pending, err := database.PendingArtifactExports(t.Context(), 1)
	require.NoError(err)
	require.Len(pending, 1)
	assert.Equal("usage-standalone", pending[0].SessionID)
	assert.Equal(int64(2), pending[0].Generation,
		"standalone ReplaceSessionUsageEvents enqueues directly")
}

// TestArtifactSessionTriggersSurviveLegacySchemaMigration is the deviation-3
// regression test: the sessions triggers must be installed after
// applySchemaColumnMigrations runs, not at schema-init time, or the update
// trigger's change-detection list fires "no such column" against an archive
// created before those columns existed.
func TestArtifactSessionTriggersSurviveLegacySchemaMigration(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	path := filepath.Join(t.TempDir(), "legacy-artifact.db")
	conn, err := sql.Open("sqlite3", makeDSN(path, false))
	require.NoError(err)
	conn.SetMaxOpenConns(1)
	_, err = conn.ExecContext(t.Context(), preParentLegacySchema)
	require.NoError(err)
	_, err = conn.ExecContext(t.Context(), legacyArchiveRows)
	require.NoError(err)
	_, err = conn.ExecContext(t.Context(), fmt.Sprintf("PRAGMA user_version = %d", dataVersion))
	require.NoError(err)
	require.NoError(conn.Close())

	database, err := Open(path)
	require.NoError(err)
	t.Cleanup(func() { require.NoError(database.Close()) })
	assert.True(database.NeedsResync())
	seedArtifactOrigin(t, database)

	require.NoError(database.UpsertSession(Session{
		ID: "legacy-fresh", Project: "project", Machine: "local", Agent: "claude",
	}))
	assert.Contains(artifactExportQueueIDs(t, database), "legacy-fresh",
		"insert trigger must fire cleanly on the migrated schema")

	_, err = database.getWriter().Exec(
		`UPDATE sessions SET display_name = 'renamed' WHERE id = 'legacy-session'`,
	)
	require.NoError(err,
		"update trigger must not fail with 'no such column' on a column added by migration")
	assert.Contains(artifactExportQueueIDs(t, database), "legacy-session",
		"update trigger must enqueue the migrated legacy session")
}

func assertArtifactExportQueueEmpty(t *testing.T, database *DB, op string) {
	t.Helper()
	var count int
	require.NoError(t, database.getReader().QueryRow(
		`SELECT count(*) FROM artifact_export_queue`,
	).Scan(&count))
	assert.Zero(t, count,
		"session %s without an artifact origin must not populate the export queue", op)
}

func artifactExportQueueIDs(t *testing.T, database *DB) []string {
	t.Helper()
	rows, err := database.getReader().Query(
		`SELECT session_id FROM artifact_export_queue
		 WHERE pending = 1 ORDER BY session_id`,
	)
	require.NoError(t, err)
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		require.NoError(t, rows.Scan(&id))
		ids = append(ids, id)
	}
	require.NoError(t, rows.Err())
	return ids
}

func clearArtifactExportQueue(t *testing.T, database *DB) {
	t.Helper()
	_, err := database.getWriter().Exec(`UPDATE artifact_export_queue SET pending = 0`)
	require.NoError(t, err)
}
