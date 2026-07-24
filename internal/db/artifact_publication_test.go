package db

import (
	"context"
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
	require.NoError(t, database.SetSyncState("artifact_origin_id", "test-origin"))
}

func TestArtifactPublicationAtomicLifecycle(t *testing.T) {
	database := testDB(t)
	ctx := t.Context()
	seedArtifactOrigin(t, database)
	require.NoError(t, database.UpsertSession(Session{
		ID: "session-a", Project: "project", Machine: "local", Agent: "claude",
	}))

	pending, err := database.PendingArtifactExports(ctx, 10)
	require.NoError(t, err)
	require.Len(t, pending, 1)
	require.Equal(t, []ArtifactExportQueueItem{{
		SessionID: "session-a", EnqueuedAt: pending[0].EnqueuedAt,
		Generation: pending[0].Generation,
	}}, pending)

	// Merely reading work models a failed export: nothing is acknowledged until
	// a checkpoint is durably created and its head is recorded.
	pendingAgain, err := database.PendingArtifactExports(ctx, 10)
	require.NoError(t, err)
	require.Equal(t, pending, pendingAgain)

	revision, changed, err := database.ApplyArtifactPublicationChanges(ctx, "desktop-a1b2c3", []ArtifactPublicationChange{{
		SessionID: "session-a", Generation: pending[0].Generation,
		ManifestHash: "manifest-a", SourceFingerprint: "source-a",
	}})
	require.NoError(t, err)
	assert.True(t, changed)
	assert.Equal(t, int64(1), revision)

	revision, changed, err = database.ApplyArtifactPublicationChanges(ctx, "desktop-a1b2c3", []ArtifactPublicationChange{{
		SessionID: "session-a", Generation: pending[0].Generation,
		ManifestHash: "manifest-a", SourceFingerprint: "source-a",
	}})
	require.NoError(t, err)
	assert.False(t, changed, "identical publication state must not force a checkpoint")

	head := ArtifactCheckpointHead{
		Origin: "desktop-a1b2c3", Sequence: 7, PublicationRevision: revision,
		SessionMapSHA256: "map-hash", CheckpointSHA256: "checkpoint-hash",
	}
	require.NoError(t, database.RecordArtifactCheckpointHead(ctx, head, pending))
	gotHead, ok, err := database.GetArtifactCheckpointHead(ctx, "desktop-a1b2c3")
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, head, gotHead)
	pending, err = database.PendingArtifactExports(ctx, 10)
	require.NoError(t, err)
	assert.Empty(t, pending)

	require.NoError(t, database.UpsertSession(Session{
		ID: "session-a", Project: "project-2", Machine: "local", Agent: "claude",
	}))
	pending, err = database.PendingArtifactExports(ctx, 10)
	require.NoError(t, err)
	require.Len(t, pending, 1)
	_, changed, err = database.ApplyArtifactPublicationChanges(ctx, "desktop-a1b2c3", []ArtifactPublicationChange{{
		SessionID: "session-a", Generation: pending[0].Generation, Delete: true,
	}})
	require.NoError(t, err)
	assert.True(t, changed)
	staleHead, ok, err := database.GetArtifactCheckpointHead(ctx, "desktop-a1b2c3")
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, revision, staleHead.PublicationRevision,
		"the retained head revision identifies it as stale")
	pending, err = database.PendingArtifactExports(ctx, 10)
	require.NoError(t, err)
	require.NoError(t, database.AcknowledgeArtifactExports(ctx, pending))
	var publications []ArtifactPublication
	_, err = database.StreamArtifactPublications(ctx, "desktop-a1b2c3", func(row ArtifactPublication) error {
		publications = append(publications, row)
		return nil
	})
	require.NoError(t, err)
	assert.Empty(t, publications)
}

func TestArtifactCheckpointHeadRejectsStalePublicationRevision(t *testing.T) {
	path := filepath.Join(t.TempDir(), "archive.db")
	first, err := Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, first.Close()) })
	second, err := Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, second.Close()) })
	seedArtifactOrigin(t, first)

	ctx := t.Context()
	require.NoError(t, first.UpsertSession(Session{
		ID: "session-a", Project: "project", Machine: "local", Agent: "claude",
	}))
	claimA, err := first.ArtifactExportClaims(ctx, []string{"session-a"})
	require.NoError(t, err)
	require.Len(t, claimA, 1)
	revisionA, changed, err := first.ApplyArtifactPublicationChanges(
		ctx, "desktop-a1b2c3", []ArtifactPublicationChange{{
			SessionID: "session-a", Generation: claimA[0].Generation,
			ManifestHash: "manifest-a", SourceFingerprint: "source-a",
		}},
	)
	require.NoError(t, err)
	require.True(t, changed)
	streamedRevisionA, err := first.StreamArtifactPublications(
		ctx, "desktop-a1b2c3", func(ArtifactPublication) error { return nil },
	)
	require.NoError(t, err)
	require.Equal(t, revisionA, streamedRevisionA)

	require.NoError(t, second.UpsertSession(Session{
		ID: "session-b", Project: "project", Machine: "local", Agent: "claude",
	}))
	claimB, err := second.ArtifactExportClaims(ctx, []string{"session-b"})
	require.NoError(t, err)
	require.Len(t, claimB, 1)
	revisionB, changed, err := second.ApplyArtifactPublicationChanges(
		ctx, "desktop-a1b2c3", []ArtifactPublicationChange{{
			SessionID: "session-b", Generation: claimB[0].Generation,
			ManifestHash: "manifest-b", SourceFingerprint: "source-b",
		}},
	)
	require.NoError(t, err)
	require.True(t, changed)
	require.Greater(t, revisionB, revisionA)
	streamedRevisionB, err := second.StreamArtifactPublications(
		ctx, "desktop-a1b2c3", func(ArtifactPublication) error { return nil },
	)
	require.NoError(t, err)
	require.Equal(t, revisionB, streamedRevisionB)
	require.NoError(t, second.RecordArtifactCheckpointHead(ctx, ArtifactCheckpointHead{
		Origin: "desktop-a1b2c3", Sequence: 1, PublicationRevision: revisionB,
		SessionMapSHA256: "map-b", CheckpointSHA256: "checkpoint-b", CheckpointSize: 12,
	}, claimB))

	err = first.RecordArtifactCheckpointHead(ctx, ArtifactCheckpointHead{
		Origin: "desktop-a1b2c3", Sequence: 2, PublicationRevision: streamedRevisionA,
		SessionMapSHA256: "map-a", CheckpointSHA256: "checkpoint-a", CheckpointSize: 12,
	}, claimA)
	require.ErrorIs(t, err, ErrArtifactExportClaimStale)
	head, ok, err := first.GetArtifactCheckpointHead(ctx, "desktop-a1b2c3")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, 1, head.Sequence)
	require.Equal(t, revisionB, head.PublicationRevision)
	pending, err := first.ArtifactExportClaims(ctx, []string{"session-a"})
	require.NoError(t, err)
	require.Equal(t, claimA, pending, "stale recording cannot consume pending work")

	retryRevision, changed, err := first.ApplyArtifactPublicationChanges(
		ctx, "desktop-a1b2c3", []ArtifactPublicationChange{{
			SessionID: "session-a", Generation: claimA[0].Generation,
			ManifestHash: "manifest-a", SourceFingerprint: "source-a",
		}},
	)
	require.NoError(t, err)
	require.False(t, changed)
	require.Equal(t, revisionB, retryRevision)
	require.NoError(t, first.RecordArtifactCheckpointHead(ctx, ArtifactCheckpointHead{
		Origin: "desktop-a1b2c3", Sequence: 3, PublicationRevision: retryRevision,
		SessionMapSHA256: "map-b", CheckpointSHA256: "checkpoint-c", CheckpointSize: 12,
	}, claimA))
}

func TestBootstrapArtifactExportQueueEnqueuesExistingLocalSessions(t *testing.T) {
	database := testDB(t)
	require.NoError(t, database.UpsertSession(Session{
		ID: "existing-local", Project: "project", Machine: "local", Agent: "claude",
	}))
	require.NoError(t, database.UpsertSession(Session{
		ID: "existing-peer", Project: "project", Machine: "peer-a1b2c3", Agent: "claude",
	}))
	require.NoError(t, database.UpsertSession(Session{
		ID: "existing-trash", Project: "project", Machine: "local", Agent: "claude",
	}))
	require.NoError(t, database.SoftDeleteSession("existing-trash"))

	require.NoError(t, database.BootstrapArtifactExportQueue())
	pending, err := database.PendingArtifactExports(t.Context(), 10)
	require.NoError(t, err)
	require.Len(t, pending, 1)
	assert.Equal(t, "existing-local", pending[0].SessionID,
		"only the live locally-owned session is bootstrapped")
	assert.Equal(t, int64(1), pending[0].Generation)
	firstEnqueuedAt := pending[0].EnqueuedAt

	require.NoError(t, database.BootstrapArtifactExportQueue())
	pendingAgain, err := database.PendingArtifactExports(t.Context(), 10)
	require.NoError(t, err)
	require.Len(t, pendingAgain, 1, "repeated bootstrap must not duplicate queue rows")
	assert.Equal(t, "existing-local", pendingAgain[0].SessionID)
	assert.Equal(t, int64(1), pendingAgain[0].Generation,
		"INSERT OR IGNORE leaves the existing generation untouched")
	assert.Equal(t, firstEnqueuedAt, pendingAgain[0].EnqueuedAt)
}

func TestArtifactExportClaimsSelectsExactPendingSessionSet(t *testing.T) {
	database := testDB(t)
	seedArtifactOrigin(t, database)
	for _, id := range []string{"alpha", "bravo", "charlie"} {
		require.NoError(t, database.UpsertSession(Session{
			ID: id, Project: "project", Machine: "local", Agent: "claude",
		}))
	}

	claims, err := database.ArtifactExportClaims(t.Context(),
		[]string{"charlie", "missing", "alpha", "charlie"})
	require.NoError(t, err)
	require.Len(t, claims, 2)
	assert.Equal(t, "alpha", claims[0].SessionID)
	assert.Equal(t, "charlie", claims[1].SessionID)
	assert.Positive(t, claims[0].Generation)
	assert.Positive(t, claims[1].Generation)
}

func TestArtifactPublicationStaleClaimRollsBackEntireBatch(t *testing.T) {
	database := testDB(t)
	ctx := t.Context()
	seedArtifactOrigin(t, database)
	for _, id := range []string{"alpha", "bravo"} {
		require.NoError(t, database.UpsertSession(Session{
			ID: id, Project: "project", Machine: "local", Agent: "claude",
		}))
	}
	claimed, err := database.PendingArtifactExports(ctx, 10)
	require.NoError(t, err)
	require.Len(t, claimed, 2)

	_, err = database.getWriter().Exec(
		`UPDATE sessions SET display_name = 'newer' WHERE id = 'bravo'`,
	)
	require.NoError(t, err)
	_, _, err = database.ApplyArtifactPublicationChanges(ctx, "desktop-a1b2c3", []ArtifactPublicationChange{
		{SessionID: "alpha", Generation: claimed[0].Generation, ManifestHash: "alpha", SourceFingerprint: "alpha"},
		{SessionID: "bravo", Generation: claimed[1].Generation, ManifestHash: "bravo", SourceFingerprint: "bravo"},
	})
	require.ErrorIs(t, err, ErrArtifactExportClaimStale)

	var publications []ArtifactPublication
	_, err = database.StreamArtifactPublications(ctx, "desktop-a1b2c3", func(row ArtifactPublication) error {
		publications = append(publications, row)
		return nil
	})
	require.NoError(t, err)
	assert.Empty(t, publications, "a stale claim must not partially mutate publication state")

	fresh, err := database.PendingArtifactExports(ctx, 10)
	require.NoError(t, err)
	require.Len(t, fresh, 2)
	_, _, err = database.ApplyArtifactPublicationChanges(ctx, "desktop-a1b2c3", []ArtifactPublicationChange{
		{SessionID: fresh[0].SessionID, Generation: fresh[0].Generation, ManifestHash: "fresh-0", SourceFingerprint: "fresh-0"},
		{SessionID: fresh[1].SessionID, Generation: fresh[1].Generation, ManifestHash: "fresh-1", SourceFingerprint: "fresh-1"},
	})
	require.NoError(t, err)
	publications = nil
	_, err = database.StreamArtifactPublications(ctx, "desktop-a1b2c3", func(row ArtifactPublication) error {
		publications = append(publications, row)
		return nil
	})
	require.NoError(t, err)
	assert.Len(t, publications, 2, "fresh claims remain applicable after a stale retry")
}

func TestArtifactCheckpointHeadStaleClaimRollsBackHead(t *testing.T) {
	database := testDB(t)
	ctx := t.Context()
	seedArtifactOrigin(t, database)
	require.NoError(t, database.UpsertSession(Session{
		ID: "racing", Project: "project", Machine: "local", Agent: "claude",
	}))
	claimed, err := database.PendingArtifactExports(ctx, 1)
	require.NoError(t, err)
	require.Len(t, claimed, 1)
	_, err = database.getWriter().Exec(
		`UPDATE sessions SET display_name = 'newer' WHERE id = 'racing'`,
	)
	require.NoError(t, err)

	err = database.RecordArtifactCheckpointHead(ctx, ArtifactCheckpointHead{
		Origin: "desktop-a1b2c3", Sequence: 1,
		SessionMapSHA256: "map-1", CheckpointSHA256: "checkpoint-1",
	}, claimed)
	require.ErrorIs(t, err, ErrArtifactExportClaimStale)
	_, ok, err := database.GetArtifactCheckpointHead(ctx, "desktop-a1b2c3")
	require.NoError(t, err)
	assert.False(t, ok, "stale acknowledgement must roll back the checkpoint head")
	pending, err := database.PendingArtifactExports(ctx, 1)
	require.NoError(t, err)
	require.Len(t, pending, 1)
	assert.Greater(t, pending[0].Generation, claimed[0].Generation)
}

func TestArtifactPublicationRowsStreamInCanonicalOrder(t *testing.T) {
	database := testDB(t)
	ctx := t.Context()
	seedArtifactOrigin(t, database)
	for _, id := range []string{"zulu", "alpha"} {
		require.NoError(t, database.UpsertSession(Session{
			ID: id, Project: "project", Machine: "local", Agent: "claude",
		}))
	}
	claimed, err := database.PendingArtifactExports(ctx, 10)
	require.NoError(t, err)
	claimGeneration := map[string]int64{}
	for _, item := range claimed {
		claimGeneration[item.SessionID] = item.Generation
	}
	_, changed, err := database.ApplyArtifactPublicationChanges(ctx, "desktop-a1b2c3", []ArtifactPublicationChange{
		{SessionID: "zulu", Generation: claimGeneration["zulu"], ManifestHash: "hash-z", SourceFingerprint: "source-z"},
		{SessionID: "alpha", Generation: claimGeneration["alpha"], ManifestHash: "hash-a", SourceFingerprint: "source-a"},
	})
	require.NoError(t, err)
	require.True(t, changed)

	var got []string
	_, err = database.StreamArtifactPublications(ctx, "desktop-a1b2c3", func(row ArtifactPublication) error {
		got = append(got, row.SessionID+"="+row.ManifestHash)
		return nil
	})
	require.NoError(t, err)
	assert.Equal(t, []string{"alpha=hash-a", "zulu=hash-z"}, got)
}

func TestArtifactCheckpointHeadRejectsRegressionWithoutAcknowledgingWork(t *testing.T) {
	database := testDB(t)
	ctx := t.Context()
	seedArtifactOrigin(t, database)
	require.NoError(t, database.RecordArtifactCheckpointHead(ctx, ArtifactCheckpointHead{
		Origin: "desktop-a1b2c3", Sequence: 7,
		SessionMapSHA256: "map-7", CheckpointSHA256: "checkpoint-7",
	}, nil))
	require.NoError(t, database.UpsertSession(Session{
		ID: "still-pending", Project: "project", Machine: "local", Agent: "claude",
	}))

	pendingBefore, err := database.PendingArtifactExports(ctx, 10)
	require.NoError(t, err)
	err = database.RecordArtifactCheckpointHead(ctx, ArtifactCheckpointHead{
		Origin: "desktop-a1b2c3", Sequence: 6,
		SessionMapSHA256: "map-6", CheckpointSHA256: "checkpoint-6",
	}, pendingBefore)
	require.Error(t, err)

	pending, err := database.PendingArtifactExports(ctx, 10)
	require.NoError(t, err)
	require.Len(t, pending, 1)
	assert.Equal(t, "still-pending", pending[0].SessionID)
	head, ok, err := database.GetArtifactCheckpointHead(ctx, "desktop-a1b2c3")
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, 7, head.Sequence)
}

func TestArtifactPublicationAcknowledgementCannotConsumeNewerMutation(t *testing.T) {
	database := testDB(t)
	ctx := t.Context()
	seedArtifactOrigin(t, database)
	require.NoError(t, database.UpsertSession(Session{
		ID: "racing", Project: "project", Machine: "local", Agent: "claude",
	}))
	claimed, err := database.PendingArtifactExports(ctx, 1)
	require.NoError(t, err)
	require.Len(t, claimed, 1)

	// Both writes can share the same SQLite millisecond. Generation, not wall
	// time, is the compare-and-ack token.
	_, err = database.getWriter().Exec(
		`UPDATE sessions SET display_name = 'newer' WHERE id = 'racing'`,
	)
	require.NoError(t, err)
	newer, err := database.PendingArtifactExports(ctx, 1)
	require.NoError(t, err)
	require.Len(t, newer, 1)
	assert.Equal(t, claimed[0].EnqueuedAt, newer[0].EnqueuedAt)
	assert.Greater(t, newer[0].Generation, claimed[0].Generation)

	err = database.RecordArtifactCheckpointHead(ctx, ArtifactCheckpointHead{
		Origin: "desktop-a1b2c3", Sequence: 1,
		SessionMapSHA256: "map-1", CheckpointSHA256: "checkpoint-1",
	}, claimed)
	require.ErrorIs(t, err, ErrArtifactExportClaimStale)
	pending, err := database.PendingArtifactExports(ctx, 1)
	require.NoError(t, err)
	require.Equal(t, newer, pending, "stale acknowledgment must leave newer work pending")
}

func TestArtifactPublicationQueueGenerationSurvivesAcknowledgementABA(t *testing.T) {
	database := testDB(t)
	ctx := t.Context()
	seedArtifactOrigin(t, database)
	require.NoError(t, database.UpsertSession(Session{
		ID: "aba", Project: "project", Machine: "local", Agent: "claude",
	}))
	oldClaim, err := database.PendingArtifactExports(ctx, 1)
	require.NoError(t, err)
	require.Len(t, oldClaim, 1)
	_, _, err = database.ApplyArtifactPublicationChanges(ctx, "desktop-a1b2c3", []ArtifactPublicationChange{{
		SessionID: "aba", Generation: oldClaim[0].Generation,
		ManifestHash: "old-manifest", SourceFingerprint: "old-source",
	}})
	require.NoError(t, err)
	require.NoError(t, database.AcknowledgeArtifactExports(ctx, oldClaim))
	pending, err := database.PendingArtifactExports(ctx, 1)
	require.NoError(t, err)
	assert.Empty(t, pending)

	_, err = database.getWriter().Exec(
		`UPDATE sessions SET display_name = 'new mutation' WHERE id = 'aba'`,
	)
	require.NoError(t, err)
	newClaim, err := database.PendingArtifactExports(ctx, 1)
	require.NoError(t, err)
	require.Len(t, newClaim, 1)
	_, err = database.getWriter().Exec(
		`UPDATE artifact_export_queue SET enqueued_at = ? WHERE session_id = 'aba'`,
		oldClaim[0].EnqueuedAt,
	)
	require.NoError(t, err)
	newClaim, err = database.PendingArtifactExports(ctx, 1)
	require.NoError(t, err)
	require.Len(t, newClaim, 1)
	assert.Equal(t, oldClaim[0].EnqueuedAt, newClaim[0].EnqueuedAt,
		"the ABA guard cannot depend on timestamp precision")
	assert.Greater(t, newClaim[0].Generation, oldClaim[0].Generation)

	revision, _, err := database.ApplyArtifactPublicationChanges(ctx, "desktop-a1b2c3", []ArtifactPublicationChange{{
		SessionID: "aba", Generation: newClaim[0].Generation,
		ManifestHash: "new-manifest", SourceFingerprint: "new-source",
	}})
	require.NoError(t, err)
	require.NoError(t, database.RecordArtifactCheckpointHead(ctx, ArtifactCheckpointHead{
		Origin: "desktop-a1b2c3", Sequence: 1, PublicationRevision: revision,
		SessionMapSHA256: "map-1", CheckpointSHA256: "checkpoint-1",
	}, nil))

	_, _, err = database.ApplyArtifactPublicationChanges(ctx, "desktop-a1b2c3", []ArtifactPublicationChange{{
		SessionID: "aba", Generation: oldClaim[0].Generation,
		ManifestHash: "stale-manifest", SourceFingerprint: "stale-source",
	}})
	require.ErrorIs(t, err, ErrArtifactExportClaimStale)
	err = database.RecordArtifactCheckpointHead(ctx, ArtifactCheckpointHead{
		Origin: "desktop-a1b2c3", Sequence: 2,
		SessionMapSHA256: "stale-map", CheckpointSHA256: "stale-checkpoint",
	}, oldClaim)
	require.ErrorIs(t, err, ErrArtifactExportClaimStale)
	require.ErrorIs(t, database.AcknowledgeArtifactExports(ctx, oldClaim), ErrArtifactExportClaimStale)

	var publications []ArtifactPublication
	_, err = database.StreamArtifactPublications(ctx, "desktop-a1b2c3", func(row ArtifactPublication) error {
		publications = append(publications, row)
		return nil
	})
	require.NoError(t, err)
	require.Len(t, publications, 1)
	assert.Equal(t, "new-manifest", publications[0].ManifestHash)
	head, ok, err := database.GetArtifactCheckpointHead(ctx, "desktop-a1b2c3")
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, 1, head.Sequence)
	pending, err = database.PendingArtifactExports(ctx, 1)
	require.NoError(t, err)
	assert.Equal(t, newClaim, pending)
}

func TestArtifactPublicationUsageOnlyAtomicBatchEnqueuesExactlyOnce(t *testing.T) {
	for _, machine := range []string{"local", "peer-a1b2c3"} {
		t.Run(machine, func(t *testing.T) {
			database := testDB(t)
			seedArtifactOrigin(t, database)
			session := Session{
				ID: "usage-batch", Project: "project", Machine: machine, Agent: "claude",
			}
			require.NoError(t, database.UpsertSession(session))
			if machine == "local" {
				claim, err := database.PendingArtifactExports(t.Context(), 1)
				require.NoError(t, err)
				require.NoError(t, database.AcknowledgeArtifactExports(t.Context(), claim))
			}
			stored, err := database.GetSessionFull(t.Context(), session.ID)
			require.NoError(t, err)
			require.NotNil(t, stored)

			_, err = database.WriteSessionBatchAtomic([]SessionBatchWrite{{
				Session: *stored,
				UsageEvents: []UsageEvent{{
					SessionID: session.ID, Source: "event", Model: "model",
					InputTokens: 10, OutputTokens: 2, DedupKey: "changed",
				}},
				ReplaceMessages: false,
			}})
			require.NoError(t, err)
			pending, err := database.PendingArtifactExports(t.Context(), 10)
			require.NoError(t, err)
			if machine != "local" {
				assert.Empty(t, pending)
				return
			}
			require.Len(t, pending, 1)
			assert.Equal(t, int64(2), pending[0].Generation,
				"usage-only batch advances the clean authority exactly once")
		})
	}
}

func TestArtifactClaimErrorsRemainDiscoverableWhenWrapped(t *testing.T) {
	assert.True(t, errors.Is(fmt.Errorf("retry: %w", ErrArtifactExportClaimStale), ErrArtifactExportClaimStale))
}

func TestArtifactCheckpointFloorReservationIsConcurrentAndNeverLowers(t *testing.T) {
	path := filepath.Join(t.TempDir(), "floor.db")
	first, err := Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, first.Close()) })
	second, err := Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, second.Close()) })

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
				context.Background(), "desktop-a1b2c3", 10,
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
		require.NoError(t, reserveErr)
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
	assert.Equal(t, want, got)

	// Simulate a crash after the committed floor reservation but before the
	// checkpoint node is created, followed by a vault reset reporting no live
	// sequence. The durable floor consumes the missing sequence permanently.
	next, err := first.ReserveArtifactCheckpointSequence(t.Context(), "desktop-a1b2c3", 0)
	require.NoError(t, err)
	assert.Equal(t, 11+reservations, next)
}

func TestArtifactPublicationStateSurvivesFullResyncCopy(t *testing.T) {
	dir := t.TempDir()
	sourcePath := filepath.Join(dir, "source.db")
	source, err := Open(sourcePath)
	require.NoError(t, err)
	seedArtifactOrigin(t, source)

	ctx := t.Context()
	require.NoError(t, source.UpsertSession(Session{
		ID: "queued", Project: "project", Machine: "local", Agent: "claude",
	}))
	require.NoError(t, source.UpsertSession(Session{
		ID: "published", Project: "project", Machine: "local", Agent: "claude",
	}))
	claims, err := source.PendingArtifactExports(ctx, 10)
	require.NoError(t, err)
	var publishedClaim ArtifactExportQueueItem
	for _, claim := range claims {
		if claim.SessionID == "published" {
			publishedClaim = claim
		}
	}
	require.NotZero(t, publishedClaim.Generation)
	revision, _, err := source.ApplyArtifactPublicationChanges(ctx, "desktop-a1b2c3", []ArtifactPublicationChange{{
		SessionID: "published", Generation: publishedClaim.Generation,
		ManifestHash: "manifest", SourceFingerprint: "source",
	}})
	require.NoError(t, err)
	require.NoError(t, source.AcknowledgeArtifactExports(ctx, []ArtifactExportQueueItem{publishedClaim}))
	require.NoError(t, source.RecordArtifactCheckpointHead(ctx, ArtifactCheckpointHead{
		Origin: "desktop-a1b2c3", Sequence: 4, PublicationRevision: revision,
		SessionMapSHA256: "map", CheckpointSHA256: "checkpoint",
	}, nil))
	sequence, err := source.ReserveArtifactCheckpointSequence(ctx, "desktop-a1b2c3", 8)
	require.NoError(t, err)
	require.Equal(t, 9, sequence)
	require.NoError(t, source.Close())

	target, err := Open(filepath.Join(dir, "target.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, target.Close()) })
	require.NoError(t, target.CopySyncStateFrom(sourcePath))

	// A resync re-verifies every copied session, so the acknowledged
	// "published" row is re-dirtied alongside the still-pending "queued" row.
	pending, err := target.PendingArtifactExports(ctx, 10)
	require.NoError(t, err)
	require.Len(t, pending, 2)
	assert.ElementsMatch(t, []string{"queued", "published"}, []string{
		pending[0].SessionID, pending[1].SessionID,
	})
	var publications []ArtifactPublication
	streamedRevision, err := target.StreamArtifactPublications(ctx, "desktop-a1b2c3", func(row ArtifactPublication) error {
		publications = append(publications, row)
		return nil
	})
	require.NoError(t, err)
	assert.Equal(t, revision, streamedRevision)
	require.Len(t, publications, 1)
	assert.Equal(t, "published", publications[0].SessionID)
	head, ok, err := target.GetArtifactCheckpointHead(ctx, "desktop-a1b2c3")
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, 4, head.Sequence)
	next, err := target.ReserveArtifactCheckpointSequence(ctx, "desktop-a1b2c3", 0)
	require.NoError(t, err)
	assert.Equal(t, 10, next)

	// Clean authority rows survive the swap and copied generations are advanced,
	// so an in-flight claim against the source database cannot become valid in
	// the replacement archive after the same session is dirtied again.
	require.NoError(t, target.UpsertSession(Session{
		ID: "published", Project: "project", Machine: "local", Agent: "claude",
	}))
	pending, err = target.PendingArtifactExports(ctx, 10)
	require.NoError(t, err)
	var republished ArtifactExportQueueItem
	for _, item := range pending {
		if item.SessionID == "published" {
			republished = item
		}
	}
	require.Greater(t, republished.Generation, publishedClaim.Generation)
	_, _, err = target.ApplyArtifactPublicationChanges(ctx, "desktop-a1b2c3", []ArtifactPublicationChange{{
		SessionID: "published", Generation: publishedClaim.Generation,
		ManifestHash: "stale", SourceFingerprint: "stale",
	}})
	require.ErrorIs(t, err, ErrArtifactExportClaimStale)
}

// TestCopySyncStateForcesPendingOnFreshCopiedQueueRows models the shipped
// resync flow: the old DB holds acknowledged (pending=0) queue rows, the fresh
// DB has no origin key so the triggers stay gated and the queue is empty, and
// CopySyncStateFrom must re-dirty every copied row so the exporter re-verifies
// the rebuilt archive. Covers the fresh-insert branch of the queue copy.
func TestCopySyncStateForcesPendingOnFreshCopiedQueueRows(t *testing.T) {
	dir := t.TempDir()
	sourcePath := filepath.Join(dir, "source.db")
	source, err := Open(sourcePath)
	require.NoError(t, err)
	seedArtifactOrigin(t, source)
	ctx := t.Context()

	for _, id := range []string{"sess-1", "sess-2"} {
		require.NoError(t, source.UpsertSession(Session{
			ID: id, Project: "project", Machine: "local", Agent: "claude",
		}))
	}
	claims, err := source.PendingArtifactExports(ctx, 10)
	require.NoError(t, err)
	require.Len(t, claims, 2)
	require.NoError(t, source.AcknowledgeArtifactExports(ctx, claims))
	drained, err := source.PendingArtifactExports(ctx, 10)
	require.NoError(t, err)
	require.Empty(t, drained, "old queue rows are acknowledged before the resync")
	oldGen := map[string]int64{}
	for _, claim := range claims {
		oldGen[claim.SessionID] = claim.Generation
	}
	require.NoError(t, source.Close())

	target, err := Open(filepath.Join(dir, "target.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, target.Close()) })
	for _, id := range []string{"sess-1", "sess-2"} {
		require.NoError(t, target.UpsertSession(Session{
			ID: id, Project: "project", Machine: "local", Agent: "claude",
		}))
	}
	emptyBefore, err := target.PendingArtifactExports(ctx, 10)
	require.NoError(t, err)
	require.Empty(t, emptyBefore, "no origin key: queue triggers stay gated during resync")

	require.NoError(t, target.CopySyncStateFrom(sourcePath))

	pending, err := target.PendingArtifactExports(ctx, 10)
	require.NoError(t, err)
	require.Len(t, pending, 2, "a resync must re-verify every copied session")
	assert.ElementsMatch(t, []string{"sess-1", "sess-2"}, []string{
		pending[0].SessionID, pending[1].SessionID,
	})
	for _, item := range pending {
		assert.Greater(t, item.Generation, oldGen[item.SessionID],
			"copied generation must advance and never regress")
	}
}

// TestCopySyncStateMergesQueueRowsAsPending exercises the ON CONFLICT merge
// branch of the queue copy (previously untested). The fresh DB already has the
// origin key and a trigger-created row that has itself been acknowledged, and
// the old DB carries an acknowledged row for the same session at a different
// generation. After the copy the row is pending again and its generation is the
// advanced maximum of both sources.
func TestCopySyncStateMergesQueueRowsAsPending(t *testing.T) {
	dir := t.TempDir()
	sourcePath := filepath.Join(dir, "source.db")
	source, err := Open(sourcePath)
	require.NoError(t, err)
	seedArtifactOrigin(t, source)
	ctx := t.Context()

	require.NoError(t, source.UpsertSession(Session{
		ID: "overlap", Project: "project", Machine: "local", Agent: "claude",
	}))
	for i := range 3 {
		_, err = source.getWriter().Exec(
			`UPDATE sessions SET display_name = ? WHERE id = 'overlap'`, "v"+strconv.Itoa(i))
		require.NoError(t, err)
	}
	oldClaims, err := source.PendingArtifactExports(ctx, 10)
	require.NoError(t, err)
	require.Len(t, oldClaims, 1)
	oldGen := oldClaims[0].Generation
	require.Greater(t, oldGen, int64(1))
	require.NoError(t, source.AcknowledgeArtifactExports(ctx, oldClaims))
	require.NoError(t, source.Close())

	target, err := Open(filepath.Join(dir, "target.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, target.Close()) })
	seedArtifactOrigin(t, target)
	require.NoError(t, target.UpsertSession(Session{
		ID: "overlap", Project: "project", Machine: "local", Agent: "claude",
	}))
	newClaims, err := target.PendingArtifactExports(ctx, 10)
	require.NoError(t, err)
	require.Len(t, newClaims, 1)
	newGen := newClaims[0].Generation
	require.NoError(t, target.AcknowledgeArtifactExports(ctx, newClaims))
	acked, err := target.PendingArtifactExports(ctx, 10)
	require.NoError(t, err)
	require.Empty(t, acked, "both queue rows are acknowledged before the merge")

	require.NoError(t, target.CopySyncStateFrom(sourcePath))

	pending, err := target.PendingArtifactExports(ctx, 10)
	require.NoError(t, err)
	require.Len(t, pending, 1, "the merged row is re-dirtied by the resync")
	assert.Equal(t, "overlap", pending[0].SessionID)
	expected := max(oldGen+1, newGen) + 1
	assert.Equal(t, expected, pending[0].Generation,
		"merged generation is the advanced maximum of both sources")
}

func TestArtifactPublicationStateCopyAcceptsPreRevisionCheckpointHead(t *testing.T) {
	dir := t.TempDir()
	sourcePath := filepath.Join(dir, "legacy.db")
	legacy, err := sql.Open("sqlite3", sourcePath)
	require.NoError(t, err)
	_, err = legacy.Exec(`
		CREATE TABLE artifact_checkpoint_heads (
			origin TEXT PRIMARY KEY,
			sequence INTEGER NOT NULL,
			session_map_sha256 TEXT NOT NULL,
			checkpoint_sha256 TEXT NOT NULL
		);
		INSERT INTO artifact_checkpoint_heads VALUES (
			'desktop-a1b2c3', 4, 'map', 'checkpoint'
		);`)
	require.NoError(t, err)
	require.NoError(t, legacy.Close())

	target := testDB(t)
	require.NoError(t, target.CopySyncStateFrom(sourcePath))
	head, ok, err := target.GetArtifactCheckpointHead(t.Context(), "desktop-a1b2c3")
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, 4, head.Sequence)
	assert.Zero(t, head.PublicationRevision)
	assert.Zero(t, head.CheckpointSize)
}

func TestArtifactPublicationExportCardinalityIsQueueBounded(t *testing.T) {
	for _, unrelated := range []int{20, 2000} {
		t.Run(strconv.Itoa(unrelated), func(t *testing.T) {
			database := testDB(t)
			seedArtifactOrigin(t, database)
			for i := range unrelated {
				_, err := database.getWriter().Exec(
					`INSERT INTO sessions (id, project, machine, agent)
					 VALUES (?, 'project', 'peer-a1b2c3', 'claude')`,
					"peer-"+strconv.Itoa(i),
				)
				require.NoError(t, err)
			}
			require.NoError(t, database.UpsertSession(Session{
				ID: "dirty", Project: "project", Machine: "local", Agent: "claude",
			}))
			pending, err := database.PendingArtifactExports(t.Context(), 1)
			require.NoError(t, err)
			require.Len(t, pending, 1)
			assert.Equal(t, "dirty", pending[0].SessionID)
		})
	}
}

func TestArtifactPublicationQueueTracksOnlyLocallyOwnedContent(t *testing.T) {
	database := testDB(t)
	seedArtifactOrigin(t, database)

	require.NoError(t, database.UpsertSession(Session{
		ID: "local-session", Project: "project", Machine: "local", Agent: "claude",
	}))
	require.Equal(t, []string{"local-session"}, artifactExportQueueIDs(t, database))
	clearArtifactExportQueue(t, database)

	require.NoError(t, database.UpsertSession(Session{
		ID: "peer-session", Project: "project", Machine: "peer-a1b2c3", Agent: "claude",
	}))
	assert.Empty(t, artifactExportQueueIDs(t, database), "foreign inserts stay out of the local publication queue")

	_, err := database.getWriter().Exec(
		`UPDATE sessions SET display_name = 'renamed' WHERE id = 'local-session'`,
	)
	require.NoError(t, err)
	require.Equal(t, []string{"local-session"}, artifactExportQueueIDs(t, database))
	clearArtifactExportQueue(t, database)

	_, err = database.getWriter().Exec(
		`UPDATE sessions SET machine = 'peer-a1b2c3' WHERE id = 'local-session'`,
	)
	require.NoError(t, err)
	require.Equal(t, []string{"local-session"}, artifactExportQueueIDs(t, database),
		"local-to-foreign transition must publish removal")
	clearArtifactExportQueue(t, database)

	_, err = database.getWriter().Exec(
		`UPDATE sessions SET machine = 'local' WHERE id = 'peer-session'`,
	)
	require.NoError(t, err)
	require.Equal(t, []string{"peer-session"}, artifactExportQueueIDs(t, database),
		"foreign-to-local transition must publish content")
	clearArtifactExportQueue(t, database)

	_, err = database.getWriter().Exec(
		`UPDATE sessions SET display_name = 'peer rename' WHERE id = 'local-session'`,
	)
	require.NoError(t, err)
	assert.Empty(t, artifactExportQueueIDs(t, database), "unchanged foreign updates stay out of the queue")
}

func TestArtifactPublicationQueueKeepsFirstDirtyTimeForFIFO(t *testing.T) {
	database := testDB(t)
	seedArtifactOrigin(t, database)
	require.NoError(t, database.UpsertSession(Session{
		ID: "dirty", Project: "project", Machine: "local", Agent: "claude",
	}))
	const firstDirty = "2026-01-02T03:04:05.000Z"
	_, err := database.getWriter().Exec(
		`UPDATE artifact_export_queue SET enqueued_at = ? WHERE session_id = 'dirty'`,
		firstDirty,
	)
	require.NoError(t, err)

	_, err = database.getWriter().Exec(
		`UPDATE sessions SET display_name = 'changed again' WHERE id = 'dirty'`,
	)
	require.NoError(t, err)
	pending, err := database.PendingArtifactExports(t.Context(), 1)
	require.NoError(t, err)
	require.Len(t, pending, 1)
	assert.Equal(t, firstDirty, pending[0].EnqueuedAt,
		"repeated changes retain FIFO position until acknowledgement")
	assert.Greater(t, pending[0].Generation, int64(1),
		"repeated changes advance the compare-and-ack generation")
}

func TestArtifactPublicationQueueRefreshesDirtyTimeOnlyAfterAcknowledgement(t *testing.T) {
	database := testDB(t)
	seedArtifactOrigin(t, database)
	require.NoError(t, database.UpsertSession(Session{
		ID: "dirty", Project: "project", Machine: "local", Agent: "claude",
	}))
	const oldDirty = "2020-01-02T03:04:05.000Z"
	_, err := database.getWriter().Exec(
		`UPDATE artifact_export_queue SET enqueued_at = ? WHERE session_id = 'dirty'`,
		oldDirty,
	)
	require.NoError(t, err)
	claim, err := database.PendingArtifactExports(t.Context(), 1)
	require.NoError(t, err)
	require.NoError(t, database.AcknowledgeArtifactExports(t.Context(), claim))

	_, err = database.getWriter().Exec(
		`UPDATE sessions SET display_name = 'first clean mutation' WHERE id = 'dirty'`,
	)
	require.NoError(t, err)
	pending, err := database.PendingArtifactExports(t.Context(), 1)
	require.NoError(t, err)
	require.Len(t, pending, 1)
	assert.NotEqual(t, oldDirty, pending[0].EnqueuedAt,
		"clean-to-pending transition receives a fresh FIFO position")
	firstDirty := pending[0].EnqueuedAt

	_, err = database.getWriter().Exec(
		`UPDATE sessions SET display_name = 'second pending mutation' WHERE id = 'dirty'`,
	)
	require.NoError(t, err)
	pending, err = database.PendingArtifactExports(t.Context(), 1)
	require.NoError(t, err)
	require.Len(t, pending, 1)
	assert.Equal(t, firstDirty, pending[0].EnqueuedAt,
		"repeated pending writes retain their FIFO position")
}

func TestArtifactPublicationQueueTracksMessagesUsageAndCascadeDeletion(t *testing.T) {
	database := testDB(t)
	seedArtifactOrigin(t, database)
	for _, session := range []Session{
		{ID: "local-session", Project: "project", Machine: "local", Agent: "claude"},
		{ID: "peer-session", Project: "project", Machine: "peer-a1b2c3", Agent: "claude"},
	} {
		require.NoError(t, database.UpsertSession(session))
	}
	clearArtifactExportQueue(t, database)

	require.NoError(t, database.InsertMessages([]Message{
		{SessionID: "local-session", Ordinal: 0, Role: "user", Content: "local"},
		{SessionID: "peer-session", Ordinal: 0, Role: "user", Content: "peer"},
	}))
	require.Equal(t, []string{"local-session"}, artifactExportQueueIDs(t, database))
	pending, err := database.PendingArtifactExports(t.Context(), 1)
	require.NoError(t, err)
	require.Len(t, pending, 1)
	assert.Equal(t, int64(2), pending[0].Generation,
		"InsertMessages enqueues once for the owning session")
	clearArtifactExportQueue(t, database)

	require.NoError(t, database.ReplaceSessionUsageEvents("local-session", []UsageEvent{{
		SessionID: "local-session", Source: "event", Model: "model", DedupKey: "local",
	}}))
	require.NoError(t, database.ReplaceSessionUsageEvents("peer-session", []UsageEvent{{
		SessionID: "peer-session", Source: "event", Model: "model", DedupKey: "peer",
	}}))
	require.Equal(t, []string{"local-session"}, artifactExportQueueIDs(t, database))
	pending, err = database.PendingArtifactExports(t.Context(), 1)
	require.NoError(t, err)
	require.Len(t, pending, 1)
	assert.Equal(t, int64(3), pending[0].Generation,
		"usage replacement enqueues once independently of event rows")
	clearArtifactExportQueue(t, database)

	_, err = database.getWriter().Exec(`DELETE FROM sessions WHERE id = 'local-session'`)
	require.NoError(t, err)
	require.Equal(t, []string{"local-session"}, artifactExportQueueIDs(t, database),
		"the owner signal must survive child-row cascade ordering")
}

func TestArtifactPublicationQueueMessageReplacementIsBatchBounded(t *testing.T) {
	for _, count := range []int{2, 2000} {
		t.Run(strconv.Itoa(count), func(t *testing.T) {
			database := testDB(t)
			seedArtifactOrigin(t, database)
			require.NoError(t, database.UpsertSession(Session{
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
			require.NoError(t, database.ReplaceSessionMessages("session", messages))
			pending, err := database.PendingArtifactExports(t.Context(), 1)
			require.NoError(t, err)
			require.Len(t, pending, 1)
			assert.Equal(t, int64(2), pending[0].Generation,
				"one transaction advances the queue independently of message count")
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
	require.NoError(t, err)
	require.Equal(t, 1, result.WrittenSessions)
	assert.Empty(t, artifactExportQueueIDs(t, database),
		"a batch write must not populate the export queue before an artifact origin exists")

	seedArtifactOrigin(t, database)
	write.Session.ID = "gated-session-2"
	write.Messages[0].SessionID = "gated-session-2"
	result, err = database.WriteSessionBatch([]SessionBatchWrite{write})
	require.NoError(t, err)
	require.Equal(t, 1, result.WrittenSessions)
	assert.Equal(t, []string{"gated-session-2"}, artifactExportQueueIDs(t, database),
		"after an artifact origin exists, the identical batch write enqueues the session")
}

// TestReplaceSessionUsageEventsEnqueuesArtifactExport covers the standalone
// ReplaceSessionUsageEvents path (usage_events.go, enqueueArtifact=true),
// as distinct from the batch path (WriteSessionBatch passes false and relies
// on its own end-of-batch generation check; see
// TestArtifactPublicationUsageOnlyAtomicBatchEnqueuesExactlyOnce).
func TestReplaceSessionUsageEventsEnqueuesArtifactExport(t *testing.T) {
	database := testDB(t)
	seedArtifactOrigin(t, database)
	require.NoError(t, database.UpsertSession(Session{
		ID: "usage-standalone", Project: "project", Machine: "local", Agent: "claude",
	}))
	clearArtifactExportQueue(t, database)

	require.NoError(t, database.ReplaceSessionUsageEvents("usage-standalone", []UsageEvent{{
		SessionID: "usage-standalone", Source: "event", Model: "model", DedupKey: "standalone",
	}}))
	pending, err := database.PendingArtifactExports(t.Context(), 1)
	require.NoError(t, err)
	require.Len(t, pending, 1)
	assert.Equal(t, "usage-standalone", pending[0].SessionID)
	assert.Equal(t, int64(2), pending[0].Generation,
		"standalone ReplaceSessionUsageEvents enqueues directly")
}

// TestArtifactSessionTriggersSurviveLegacySchemaMigration is the deviation-3
// regression test: the sessions triggers must be installed after
// applySchemaColumnMigrations runs, not at schema-init time, or the update
// trigger's change-detection list fires "no such column" against an archive
// created before those columns existed.
func TestArtifactSessionTriggersSurviveLegacySchemaMigration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy-artifact.db")
	conn, err := sql.Open("sqlite3", makeDSN(path, false))
	require.NoError(t, err)
	conn.SetMaxOpenConns(1)
	_, err = conn.Exec(preParentLegacySchema)
	require.NoError(t, err)
	_, err = conn.Exec(legacyArchiveRows)
	require.NoError(t, err)
	_, err = conn.Exec(fmt.Sprintf("PRAGMA user_version = %d", dataVersion))
	require.NoError(t, err)
	require.NoError(t, conn.Close())

	database, err := Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, database.Close()) })
	assert.True(t, database.NeedsResync())
	seedArtifactOrigin(t, database)

	require.NoError(t, database.UpsertSession(Session{
		ID: "legacy-fresh", Project: "project", Machine: "local", Agent: "claude",
	}))
	assert.Contains(t, artifactExportQueueIDs(t, database), "legacy-fresh",
		"insert trigger must fire cleanly on the migrated schema")

	_, err = database.getWriter().Exec(
		`UPDATE sessions SET display_name = 'renamed' WHERE id = 'legacy-session'`,
	)
	require.NoError(t, err,
		"update trigger must not fail with 'no such column' on a column added by migration")
	assert.Contains(t, artifactExportQueueIDs(t, database), "legacy-session",
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
