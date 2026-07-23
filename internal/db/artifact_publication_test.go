package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestArtifactPublicationAtomicLifecycle(t *testing.T) {
	database := testDB(t)
	ctx := t.Context()
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

func TestArtifactPublicationQueueBootstrapsExistingLocalSessionsOnOpen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "archive.db")
	database, err := Open(path)
	require.NoError(t, err)
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
	require.NoError(t, database.UpsertSession(Session{
		ID: "existing-clean", Project: "project", Machine: "local", Agent: "claude",
	}))
	cleanClaim, err := database.ArtifactExportClaims(t.Context(), []string{"existing-clean"})
	require.NoError(t, err)
	require.Len(t, cleanClaim, 1)
	require.NoError(t, database.AcknowledgeArtifactExports(t.Context(), cleanClaim))
	_, err = database.getWriter().Exec(
		`DELETE FROM artifact_export_queue WHERE session_id <> 'existing-clean'`,
	)
	require.NoError(t, err)
	require.NoError(t, database.Close())

	database, err = Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, database.Close()) })
	pending, err := database.PendingArtifactExports(t.Context(), 10)
	require.NoError(t, err)
	require.Len(t, pending, 1)
	assert.Equal(t, "existing-local", pending[0].SessionID)
	assert.Equal(t, int64(1), pending[0].Generation)
	var cleanPending bool
	var cleanGeneration int64
	require.NoError(t, database.getReader().QueryRow(`
		SELECT pending, generation FROM artifact_export_queue
		WHERE session_id = 'existing-clean'`).Scan(&cleanPending, &cleanGeneration))
	assert.False(t, cleanPending, "idempotent bootstrap cannot requeue clean authority")
	assert.Equal(t, cleanClaim[0].Generation, cleanGeneration)

	require.NoError(t, database.Close())
	database, err = Open(path)
	require.NoError(t, err)
	pending, err = database.PendingArtifactExports(t.Context(), 10)
	require.NoError(t, err)
	require.Len(t, pending, 1)
	assert.Equal(t, "existing-local", pending[0].SessionID)
}

func TestArtifactExportClaimsSelectsExactPendingSessionSet(t *testing.T) {
	database := testDB(t)
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

func TestArtifactPublicationRepairQueueIsBoundedAndAcknowledged(t *testing.T) {
	database := testDB(t)
	ctx := t.Context()
	for _, repair := range []ArtifactRepair{
		{Origin: "desktop-a1b2c3", Kind: "manifests", Name: "b.json", SHA256: "hash-b", Size: 20},
		{Origin: "desktop-a1b2c3", Kind: "segments", Name: "a.ndjson", SHA256: "hash-a", Size: 10},
	} {
		require.NoError(t, database.EnqueueArtifactRepair(ctx, repair))
	}

	pending, err := database.PendingArtifactRepairs(ctx, 1)
	require.NoError(t, err)
	require.Len(t, pending, 1)
	require.NoError(t, database.AcknowledgeArtifactRepair(ctx, pending[0]))
	pending, err = database.PendingArtifactRepairs(ctx, 10)
	require.NoError(t, err)
	assert.Len(t, pending, 1)
}

func TestArtifactRepairAcknowledgementUsesFullClaimIdentity(t *testing.T) {
	database := testDB(t)
	ctx := t.Context()
	original := ArtifactRepair{
		Origin: "desktop-a1b2c3", Kind: "segments", Name: "segment.ndjson",
		SHA256: "old-hash", Size: 10,
	}
	require.NoError(t, database.EnqueueArtifactRepair(ctx, original))
	claimed, err := database.PendingArtifactRepairs(ctx, 1)
	require.NoError(t, err)
	require.Len(t, claimed, 1)
	require.NoError(t, database.EnqueueArtifactRepair(ctx, ArtifactRepair{
		Origin: original.Origin, Kind: original.Kind, Name: original.Name,
		SHA256: "new-hash", Size: 11,
	}))

	err = database.AcknowledgeArtifactRepair(ctx, claimed[0])
	require.ErrorIs(t, err, ErrArtifactRepairClaimStale)
	pending, err := database.PendingArtifactRepairs(ctx, 1)
	require.NoError(t, err)
	require.Len(t, pending, 1)
	assert.Equal(t, "new-hash", pending[0].SHA256)
	sameIdentityClaim := pending[0]

	// Re-detecting the identical expected identity only refreshes its timestamp;
	// the original claim still describes the same repair and may acknowledge it.
	require.NoError(t, database.EnqueueArtifactRepair(ctx, ArtifactRepair{
		Origin: original.Origin, Kind: original.Kind, Name: original.Name,
		SHA256: "new-hash", Size: 11, DetectedAt: "2030-01-01T00:00:00.000Z",
	}))
	require.NoError(t, database.AcknowledgeArtifactRepair(ctx, sameIdentityClaim))
}

func TestArtifactPeerCheckpointHeadIsMonotonicAndImmutable(t *testing.T) {
	database := testDB(t)
	ctx := t.Context()
	head := ArtifactPeerCheckpointHead{
		Origin: "peer-a1b2c3", Sequence: 2,
		CheckpointSHA256: strings.Repeat("a", 64), CheckpointSize: 123,
	}
	require.NoError(t, database.RecordArtifactPeerCheckpointHead(ctx, head))
	require.NoError(t, database.RecordArtifactPeerCheckpointHead(ctx, head),
		"an exact replay is idempotent")

	stale := head
	stale.Sequence = 1
	stale.CheckpointSHA256 = strings.Repeat("b", 64)
	require.Error(t, database.RecordArtifactPeerCheckpointHead(ctx, stale))
	conflict := head
	conflict.CheckpointSHA256 = strings.Repeat("c", 64)
	require.Error(t, database.RecordArtifactPeerCheckpointHead(ctx, conflict))

	got, found, err := database.GetArtifactPeerCheckpointHead(ctx, head.Origin)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, head, got)
}

func TestArtifactCheckpointLandingReplacesExactManifestMap(t *testing.T) {
	database := testDB(t)
	ctx := t.Context()
	origin := "peer-a1b2c3"

	recorded := ArtifactCheckpointLanding{Origin: origin, Sequence: 7}
	recordedMap := map[string]string{
		origin + "~alpha": "manifest-alpha",
		origin + "~bravo": "manifest-bravo",
	}
	require.NoError(t, database.RecordArtifactCheckpointLanding(ctx, recorded, recordedMap))

	got, gotMap, ok, err := database.GetArtifactCheckpointLanding(ctx, origin)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, recorded, got)
	assert.Equal(t, recordedMap, gotMap)

	replacement := ArtifactCheckpointLanding{Origin: origin, Sequence: 8}
	replacementMap := map[string]string{
		origin + "~alpha": "manifest-alpha-v2",
	}
	require.NoError(t, database.RecordArtifactCheckpointLanding(ctx, replacement, replacementMap))

	got, gotMap, ok, err = database.GetArtifactCheckpointLanding(ctx, origin)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, replacement, got)
	assert.Equal(t, replacementMap, gotMap,
		"removed checkpoint entries must not remain in exact landing provenance")

	stale := ArtifactCheckpointLanding{Origin: origin, Sequence: 7}
	err = database.RecordArtifactCheckpointLanding(ctx, stale, map[string]string{
		origin + "~stale": "manifest-stale",
	})
	require.Error(t, err)

	got, gotMap, ok, err = database.GetArtifactCheckpointLanding(ctx, origin)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, replacement, got)
	assert.Equal(t, replacementMap, gotMap,
		"a lower sequence must preserve the newer exact landing snapshot")
}

func TestArtifactClaimErrorsRemainDiscoverableWhenWrapped(t *testing.T) {
	assert.True(t, errors.Is(fmt.Errorf("retry: %w", ErrArtifactExportClaimStale), ErrArtifactExportClaimStale))
	assert.True(t, errors.Is(fmt.Errorf("retry: %w", ErrArtifactRepairClaimStale), ErrArtifactRepairClaimStale))
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
	require.NoError(t, source.EnqueueArtifactRepair(ctx, ArtifactRepair{
		Origin: "peer-d4e5f6", Kind: "segments", Name: "segment.ndjson",
		SHA256: "segment", Size: 123,
	}))
	require.NoError(t, source.RecordArtifactCheckpointLanding(ctx,
		ArtifactCheckpointLanding{Origin: "peer-d4e5f6", Sequence: 6},
		map[string]string{"peer-d4e5f6~session": "manifest"},
	))
	require.NoError(t, source.RecordArtifactPeerCheckpointHead(ctx,
		ArtifactPeerCheckpointHead{
			Origin: "peer-d4e5f6", Sequence: 7,
			CheckpointSHA256: strings.Repeat("d", 64), CheckpointSize: 456,
		}))
	require.NoError(t, source.Close())

	target, err := Open(filepath.Join(dir, "target.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, target.Close()) })
	require.NoError(t, target.CopySyncStateFrom(sourcePath))

	pending, err := target.PendingArtifactExports(ctx, 10)
	require.NoError(t, err)
	require.Len(t, pending, 1)
	assert.Equal(t, "queued", pending[0].SessionID)
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
	repairs, err := target.PendingArtifactRepairs(ctx, 10)
	require.NoError(t, err)
	assert.Len(t, repairs, 1)
	landing, manifests, ok, err := target.GetArtifactCheckpointLanding(ctx, "peer-d4e5f6")
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, 6, landing.Sequence)
	assert.Equal(t, map[string]string{"peer-d4e5f6~session": "manifest"}, manifests)
	peerHead, ok, err := target.GetArtifactPeerCheckpointHead(ctx, "peer-d4e5f6")
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, 7, peerHead.Sequence)
	assert.Equal(t, strings.Repeat("d", 64), peerHead.CheckpointSHA256)
	assert.Equal(t, int64(456), peerHead.CheckpointSize)

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

func TestArtifactResetRepublishPendingUsesCompareAndSwapClear(t *testing.T) {
	database := testDB(t)
	pending := ArtifactResetRepublishPending{
		Version:         1,
		RootFingerprint: strings.Repeat("a", 64),
		Origin:          "desktop-d4e5f6",
		Token:           strings.Repeat("b", 64),
		BaselineHLC:     "2026-07-22T120000.000000000Z-00000000000000000000",
	}
	require.NoError(t, database.SetArtifactResetRepublishPending(t.Context(), pending))

	got, found, err := database.ArtifactResetRepublishPending(t.Context())
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, pending, got)

	stale := pending
	stale.Token = strings.Repeat("c", 64)
	cleared, err := database.ClearArtifactResetRepublishPending(t.Context(), stale)
	require.NoError(t, err)
	assert.False(t, cleared)
	_, found, err = database.ArtifactResetRepublishPending(t.Context())
	require.NoError(t, err)
	assert.True(t, found)

	cleared, err = database.ClearArtifactResetRepublishPending(t.Context(), pending)
	require.NoError(t, err)
	assert.True(t, cleared)
	_, found, err = database.ArtifactResetRepublishPending(t.Context())
	require.NoError(t, err)
	assert.False(t, found)
}

func TestArtifactImportQueueIsIdempotentAndRejectsIdentityConflicts(t *testing.T) {
	database := testDB(t)
	work := ArtifactImportWork{
		Origin: "peer-a1b2c3", Kind: "meta",
		Name:   artifactImportMetadataName("a"),
		SHA256: strings.Repeat("a", 64), Size: 42,
		Reason: "session not landed", RequiredFormatVersion: 1,
	}

	require.NoError(t, database.EnqueueArtifactImport(t.Context(), work))
	require.NoError(t, database.EnqueueArtifactImport(t.Context(), work))
	count, oldest, err := database.ArtifactImportQueueStats(t.Context())
	require.NoError(t, err)
	assert.Equal(t, 1, count)
	assert.NotEmpty(t, oldest)

	conflict := work
	conflict.SHA256 = strings.Repeat("b", 64)
	require.ErrorContains(t,
		database.EnqueueArtifactImport(t.Context(), conflict), "identity")
	pending, err := database.PendingArtifactImports(t.Context(), 1, 10)
	require.NoError(t, err)
	require.Len(t, pending, 1)
	assert.Equal(t, work.SHA256, pending[0].SHA256)
}

func TestArtifactImportQueueRejectsIncompleteWork(t *testing.T) {
	database := testDB(t)
	valid := ArtifactImportWork{
		Origin: "peer-a1b2c3", Kind: "meta",
		Name:   artifactImportMetadataName("a"),
		SHA256: strings.Repeat("a", 64), Size: 1,
		Reason: "retry", RequiredFormatVersion: 1,
	}
	tests := map[string]func(*ArtifactImportWork){
		"origin":      func(work *ArtifactImportWork) { work.Origin = "" },
		"kind":        func(work *ArtifactImportWork) { work.Kind = "segments" },
		"name":        func(work *ArtifactImportWork) { work.Name = "elsewhere.json" },
		"identity":    func(work *ArtifactImportWork) { work.SHA256 = "short" },
		"size":        func(work *ArtifactImportWork) { work.Size = -1 },
		"reason":      func(work *ArtifactImportWork) { work.Reason = "" },
		"format":      func(work *ArtifactImportWork) { work.RequiredFormatVersion = 0 },
		"enqueueTime": func(work *ArtifactImportWork) { work.EnqueuedAt = "" },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			work := valid
			mutate(&work)
			if name == "enqueueTime" {
				_, err := database.AcknowledgeArtifactImport(t.Context(), work)
				assert.Error(t, err)
				return
			}
			assert.Error(t, database.EnqueueArtifactImport(t.Context(), work))
		})
	}
	checkpoint := valid
	checkpoint.Kind = "checkpoints"
	checkpoint.Name = "cp-4.json"
	assert.Error(t, database.EnqueueArtifactImport(t.Context(), checkpoint))
	_, err := database.PendingArtifactImports(t.Context(), 0, 1)
	assert.Error(t, err)
}

func TestArtifactImportQueueBoundsFIFOAndFutureFormatEligibility(t *testing.T) {
	database := testDB(t)
	works := []ArtifactImportWork{
		{
			Origin: "peer-a1b2c3", Kind: "meta", Name: artifactImportMetadataName("a"),
			SHA256: strings.Repeat("a", 64), Size: 1, Reason: "first",
			RequiredFormatVersion: 1,
		},
		{
			Origin: "peer-a1b2c3", Kind: "meta", Name: artifactImportMetadataName("b"),
			SHA256: strings.Repeat("b", 64), Size: 2, Reason: "future",
			RequiredFormatVersion: 2,
		},
		{
			Origin: "peer-a1b2c3", Kind: "meta", Name: artifactImportMetadataName("c"),
			SHA256: strings.Repeat("c", 64), Size: 3, Reason: "second",
			RequiredFormatVersion: 1,
		},
	}
	for _, work := range works {
		require.NoError(t, database.EnqueueArtifactImport(t.Context(), work))
	}
	_, err := database.getWriter().Exec(`
		UPDATE artifact_import_queue SET enqueued_at = CASE name
			WHEN ? THEN '2026-07-22T01:00:00.000Z'
			WHEN ? THEN '2026-07-22T02:00:00.000Z'
			ELSE '2026-07-22T03:00:00.000Z' END`, works[0].Name, works[1].Name)
	require.NoError(t, err)

	_, err = database.PendingArtifactImports(t.Context(), 1, 0)
	assert.Error(t, err)
	_, err = database.PendingArtifactImports(t.Context(), 1, 1025)
	assert.Error(t, err)
	ready, err := database.PendingArtifactImports(t.Context(), 1, 1)
	require.NoError(t, err)
	require.Len(t, ready, 1)
	assert.Equal(t, "first", ready[0].Reason)
	ready, err = database.PendingArtifactImports(t.Context(), 1, 10)
	require.NoError(t, err)
	require.Len(t, ready, 2)
	assert.Equal(t, []string{"first", "second"}, []string{ready[0].Reason, ready[1].Reason})
	all, err := database.PendingArtifactImports(t.Context(), 2, 10)
	require.NoError(t, err)
	require.Len(t, all, 3)
	assert.Equal(t, "2026-07-22T01:00:00.000Z", all[0].EnqueuedAt)
	count, oldest, err := database.ArtifactImportQueueStats(t.Context())
	require.NoError(t, err)
	assert.Equal(t, 3, count)
	assert.Equal(t, "2026-07-22T01:00:00.000Z", oldest)
}

func TestArtifactImportQueueAcknowledgesExactClaimAndPersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "archive.db")
	database, err := Open(path)
	require.NoError(t, err)
	work := ArtifactImportWork{
		Origin: "peer-a1b2c3", Kind: "meta",
		Name:   artifactImportMetadataName("d"),
		SHA256: strings.Repeat("d", 64), Size: 4,
		Reason: "retry", RequiredFormatVersion: 1,
	}
	require.NoError(t, database.EnqueueArtifactImport(t.Context(), work))
	require.NoError(t, database.Close())

	database, err = Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, database.Close()) })
	pending, err := database.PendingArtifactImports(t.Context(), 1, 10)
	require.NoError(t, err)
	require.Len(t, pending, 1)
	stale := pending[0]
	stale.EnqueuedAt = "2000-01-01T00:00:00.000Z"
	acknowledged, err := database.AcknowledgeArtifactImport(t.Context(), stale)
	require.NoError(t, err)
	assert.False(t, acknowledged)
	acknowledged, err = database.AcknowledgeArtifactImport(t.Context(), pending[0])
	require.NoError(t, err)
	assert.True(t, acknowledged)
	acknowledged, err = database.AcknowledgeArtifactImport(t.Context(), pending[0])
	require.NoError(t, err)
	assert.False(t, acknowledged)
}

func TestArtifactImportQueueAdditiveMigrationPreservesArchive(t *testing.T) {
	path := filepath.Join(t.TempDir(), "archive.db")
	database, err := Open(path)
	require.NoError(t, err)
	require.NoError(t, database.UpsertSession(Session{
		ID: "preserved", Project: "project", Machine: "local", Agent: "claude",
	}))
	require.NoError(t, database.Close())

	legacy, err := sql.Open("sqlite3", path)
	require.NoError(t, err)
	_, err = legacy.Exec(`DROP TABLE artifact_import_queue`)
	require.NoError(t, err)
	require.NoError(t, legacy.Close())

	database, err = Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, database.Close()) })
	preserved, err := database.GetSession(t.Context(), "preserved")
	require.NoError(t, err)
	require.NotNil(t, preserved)
	require.NoError(t, database.EnqueueArtifactImport(t.Context(), ArtifactImportWork{
		Origin: "peer-a1b2c3", Kind: "meta",
		Name:   artifactImportMetadataName("e"),
		SHA256: strings.Repeat("e", 64), Size: 5,
		Reason: "migration", RequiredFormatVersion: 1,
	}))
}

func TestArtifactImportQueueKeepsNewestCheckpointAndAllMetadata(t *testing.T) {
	database := testDB(t)
	checkpoint := func(sequence int, digest string) ArtifactImportWork {
		return ArtifactImportWork{
			Origin: "peer-a1b2c3", Kind: "checkpoints",
			Name: fmt.Sprintf("cp-%010d.json", sequence), SHA256: strings.Repeat(digest, 64),
			Size: int64(sequence), Reason: "dependency incomplete", RequiredFormatVersion: 1,
		}
	}
	metadata := func(digest string) ArtifactImportWork {
		return ArtifactImportWork{
			Origin: "peer-a1b2c3", Kind: "meta", Name: artifactImportMetadataName(digest),
			SHA256: strings.Repeat(digest, 64), Size: 1,
			Reason: "session not landed", RequiredFormatVersion: 1,
		}
	}
	for _, work := range []ArtifactImportWork{
		checkpoint(4, "4"), metadata("a"), metadata("b"), checkpoint(5, "5"),
		checkpoint(4, "4"),
	} {
		require.NoError(t, database.EnqueueArtifactImport(t.Context(), work))
	}
	pending, err := database.PendingArtifactImports(t.Context(), 1, 10)
	require.NoError(t, err)
	require.Len(t, pending, 3)
	var names []string
	for _, work := range pending {
		names = append(names, work.Name)
	}
	assert.ElementsMatch(t, []string{
		"cp-0000000005.json", artifactImportMetadataName("a"),
		artifactImportMetadataName("b"),
	}, names)
}

func artifactImportMetadataName(digest string) string {
	return "2026-07-22T120000.000000000Z-00000000000000000000-" +
		strings.Repeat(digest, 64) + ".json"
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
