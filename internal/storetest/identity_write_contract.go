package storetest

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/db/bunmodel"
	"go.kenn.io/agentsview/internal/export"
)

// RunCanonicalIdentityWriteContract proves that the shared identity writers
// replace every portable payload field and persist the same complete models on
// each real engine.
func RunCanonicalIdentityWriteContract(
	t *testing.T, backend string, store bun.IDB,
) {
	t.Helper()
	t.Run(backend, func(t *testing.T) {
		ctx := t.Context()
		const (
			archiveID   = "identity-write-archive"
			archiveSalt = "identity-write-salt"
			generation  = "identity-write-generation"
			sessionID   = "identity-write-session"
		)
		require.NoError(t, db.UpsertSourceArchiveRow(
			ctx, store, archiveID, archiveSalt,
		))

		observedAt := time.Date(2026, 8, 4, 12, 34, 56, 123456000, time.UTC)
		initial := export.ProjectIdentityObservation{
			SessionID: sessionID, Project: "identity-write-project",
			Machine: "identity-write-machine", RootPath: "/workspace/identity",
			GitRemote:     "https://example.com/acme/identity.git",
			GitRemoteName: "origin", RepositoryPath: "old/repository",
			WorktreeName: "old-worktree", WorktreeRootPath: "/old/worktree",
			WorktreeRelationship: export.WorktreeMain,
			CheckoutState:        export.CheckoutBranch, GitBranch: "old-branch",
			RemoteResolution:     export.ProjectResolutionUnknown,
			RemoteCandidateCount: 1,
			ObservedAt:           observedAt.Add(-time.Hour),
		}
		observationRows, err := db.CanonicalProjectIdentityObservationRows(
			archiveID, "old-salt", []export.ProjectIdentityObservation{initial},
		)
		require.NoError(t, err)
		require.NoError(t, db.UpsertProjectIdentityObservationRows(
			ctx, store, observationRows,
		))
		snapshotRows, err := db.CanonicalSessionProjectIdentitySnapshotRows(
			archiveID, generation, []export.ProjectIdentityObservation{initial},
		)
		require.NoError(t, err)
		require.NoError(t, db.UpsertSessionProjectIdentitySnapshotRows(
			ctx, store, snapshotRows,
		))

		updated := initial
		updated.GitRemoteName = "upstream"
		updated.RepositoryPath = "acme/identity"
		updated.WorktreeName = "feature"
		updated.WorktreeRootPath = "/workspace/identity"
		updated.WorktreeRelationship = export.WorktreeLinked
		updated.CheckoutState = export.CheckoutDetached
		updated.GitBranch = "feature/identity"
		updated.RemoteResolution = export.ProjectResolutionResolved
		updated.RemoteCandidateCount = 7
		updated.ObservedAt = observedAt
		observationRows, err = db.CanonicalProjectIdentityObservationRows(
			archiveID, archiveSalt, []export.ProjectIdentityObservation{updated},
		)
		require.NoError(t, err)
		require.NoError(t, db.UpsertProjectIdentityObservationRows(
			ctx, store, observationRows,
		))
		snapshotRows, err = db.CanonicalSessionProjectIdentitySnapshotRows(
			archiveID, generation, []export.ProjectIdentityObservation{updated},
		)
		require.NoError(t, err)
		require.NoError(t, db.UpsertSessionProjectIdentitySnapshotRows(
			ctx, store, snapshotRows,
		))

		wantObservation := bunmodel.SourceProjectIdentityObservation{
			SourceArchiveID: archiveID, SourceArchiveSalt: archiveSalt,
			Project: "identity-write-project", Machine: "identity-write-machine",
			RootPath:      "/workspace/identity",
			GitRemote:     "https://example.com/acme/identity.git",
			GitRemoteName: "upstream", RepositoryPath: "acme/identity",
			WorktreeName: "feature", WorktreeRootPath: "/workspace/identity",
			WorktreeRelationship: "linked_worktree", CheckoutState: "detached",
			GitBranch: "feature/identity", RemoteResolution: "resolved",
			RemoteCandidateCount: 7, ObservedAt: bunmodel.NewTimestamp(observedAt),
			NormalizedRemote: "example.com/acme/identity",
			KeySource:        "git_remote",
			Key:              "sha256:9d1aa351d2629a1ff511b97a0ccd08789b9f8f1629391221525e03378516bd60",
		}
		var gotObservation bunmodel.SourceProjectIdentityObservation
		require.NoError(t, store.NewSelect().Model(&gotObservation).
			Where("source_archive_id = ?", archiveID).
			Where("project = ?", updated.Project).
			Where("machine = ?", updated.Machine).
			Where("root_path = ?", updated.RootPath).
			Where("git_remote = ?", updated.GitRemote).Scan(ctx))
		assert.Equal(t, wantObservation, gotObservation)

		wantSnapshot := bunmodel.SourceSessionProjectIdentitySnapshot{
			SourceArchiveID: archiveID, SourceDatabaseGeneration: generation,
			SourceSessionID: sessionID, Project: "identity-write-project",
			Machine: "identity-write-machine", RootPath: "/workspace/identity",
			GitRemote:     "https://example.com/acme/identity.git",
			GitRemoteName: "upstream", RepositoryPath: "acme/identity",
			WorktreeName: "feature", WorktreeRootPath: "/workspace/identity",
			WorktreeRelationship: "linked_worktree", CheckoutState: "detached",
			GitBranch: "feature/identity", RemoteResolution: "resolved",
			RemoteCandidateCount: 7, ObservedAt: bunmodel.NewTimestamp(observedAt),
			NormalizedRemote: "example.com/acme/identity",
			KeySource:        "git_remote",
			Key:              "sha256:9d1aa351d2629a1ff511b97a0ccd08789b9f8f1629391221525e03378516bd60",
		}
		var gotSnapshot bunmodel.SourceSessionProjectIdentitySnapshot
		require.NoError(t, store.NewSelect().Model(&gotSnapshot).
			Where("source_archive_id = ?", archiveID).
			Where("source_database_generation = ?", generation).
			Where("source_session_id = ?", sessionID).Scan(ctx))
		assert.Equal(t, wantSnapshot, gotSnapshot)
	})
}
