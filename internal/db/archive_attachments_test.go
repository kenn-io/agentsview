package db

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestArchiveAttachmentSchemaPreservesExistingHistory(t *testing.T) {
	database := testDB(t)
	insertSession(t, database, "kept-session", "example")
	require.NoError(t, database.Update(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(), `DROP TABLE archive_session_provenance; DROP TABLE archive_attachment_sources;
			DROP TABLE archive_source_bindings; DROP TABLE archive_attachments`)
		return err
	}))
	path := database.Path()
	require.NoError(t, database.Close())
	reopened, err := OpenIsolatedContext(t.Context(), path)
	require.NoError(t, err)
	defer reopened.Close()
	session, err := reopened.GetSessionFull(t.Context(), "kept-session")
	require.NoError(t, err)
	require.NotNil(t, session)
	assert.Equal(t, "example", session.Project)
	require.NoError(t, reopened.RegisterArchiveAttachment(t.Context(), ArchiveAttachmentKey{"repository", "snapshot"}, "archive"))
}

func TestArchiveAttachmentOwnershipAndCompletion(t *testing.T) {
	database := testDB(t)
	ctx := t.Context()
	_, err := database.EnsureInstallationIdentity(ctx, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	require.NoError(t, err)
	key := ArchiveAttachmentKey{RepositoryID: "repository-one", SnapshotID: "snapshot-one"}
	require.NoError(t, database.RegisterArchiveAttachment(ctx, key, "archive-one"))
	require.ErrorIs(t, database.RegisterArchiveAttachment(ctx, ArchiveAttachmentKey{"replacement", key.SnapshotID}, "archive-one"), ErrArchiveSourceConflict)
	require.NoError(t, database.RegisterArchiveAttachment(ctx, key, "archive-one"))
	require.ErrorIs(t, database.RegisterArchiveAttachment(ctx, key, "another-archive"), ErrArchiveSourceConflict)
	provenance := ArchiveSourceProvenance{
		Attachment: key, ManifestID: "manifest-one",
		Source: ArchiveSourceIdentity{ArchiveID: "archive-one", DeviceID: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", RootID: "root-one", SourceKey: "source-one"},
	}
	targets := []ArchiveSourceTarget{{Provenance: provenance, Target: "parser-policy-one"}}
	require.NoError(t, database.SetArchiveAttachmentTargets(ctx, key, "summary-one", targets))
	status, err := database.ArchiveAttachmentStatus(ctx, key)
	require.NoError(t, err)
	assert.Equal(t, "summary-one", status.Target)
	assert.Equal(t, int64(1), status.Pending)
	require.NoError(t, database.CompleteArchiveSource(ctx, provenance, "parser-policy-one"))
	require.NoError(t, database.CompleteArchiveSource(ctx, provenance, "parser-policy-one"))
	status, err = database.ArchiveAttachmentStatus(ctx, key)
	require.NoError(t, err)
	assert.Zero(t, status.Pending)

	// Parser/policy upgrades cannot be completed by a stale in-flight parse.
	targets[0].Target = "parser-policy-two"
	require.NoError(t, database.SetArchiveAttachmentTargets(ctx, key, "summary-two", targets))
	require.ErrorIs(t, database.CompleteArchiveSource(ctx, provenance, "parser-policy-one"), ErrArchiveSourceConflict)
	require.NoError(t, database.RecordArchiveSourceError(ctx, provenance, "archive unavailable"))
	status, err = database.ArchiveAttachmentStatus(ctx, key)
	require.NoError(t, err)
	assert.Equal(t, int64(1), status.Pending)
	// Re-registering a current source does not clear an error or advance completion.
	require.NoError(t, database.SetArchiveAttachmentTargets(ctx, key, "summary-two", targets))
	pending, err := database.PendingArchiveSources(ctx, key, ArchiveSourceIdentity{}, 10)
	require.NoError(t, err)
	require.Len(t, pending, 1)
	assert.Equal(t, "archive unavailable", pending[0].LastError)
	assert.Equal(t, "parser-policy-two", pending[0].Target)
	require.NoError(t, database.CompleteArchiveSource(ctx, provenance, "parser-policy-two"))
	status, err = database.ArchiveAttachmentStatus(ctx, key)
	require.NoError(t, err)
	assert.Zero(t, status.Pending)

	// A different snapshot may see the same accepted source, but cannot replace it.
	other := ArchiveAttachmentKey{RepositoryID: "repository-two", SnapshotID: "snapshot-two"}
	require.NoError(t, database.RegisterArchiveAttachment(ctx, other, "archive-one"))
	provenance.Attachment = other
	targets[0].Provenance = provenance
	require.NoError(t, database.SetArchiveAttachmentTargets(ctx, other, "summary-two", targets))
	targets[0].Provenance.ManifestID = "conflicting-manifest"
	require.ErrorIs(t, database.SetArchiveAttachmentTargets(ctx, other, "summary-three", targets), ErrArchiveSourceConflict)
	pending, err = database.PendingArchiveSources(ctx, other, ArchiveSourceIdentity{}, 10)
	require.NoError(t, err)
	require.Len(t, pending, 1)
	assert.Equal(t, "manifest-one", pending[0].Provenance.ManifestID)
}

func TestArchiveAttachmentRefusesLocalAuthority(t *testing.T) {
	for _, kind := range []string{"installation", "machine alias", "retained publication authority", "archive-only"} {
		t.Run(kind, func(t *testing.T) {
			database := testDB(t)
			ctx := t.Context()
			const owner = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
			const source = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
			_, err := database.EnsureInstallationIdentity(ctx, owner)
			require.NoError(t, err)
			key := ArchiveAttachmentKey{RepositoryID: "repository-one", SnapshotID: "snapshot-one"}
			if kind == "archive-only" {
				require.NoError(t, database.EnableArchiveOnly(ctx))
				require.ErrorIs(t, database.RegisterArchiveAttachment(ctx, key, "archive-one"), ErrArchiveOnly)
				return
			}
			require.NoError(t, database.RegisterArchiveAttachment(ctx, key, "archive-one"))
			device := source
			switch kind {
			case "installation":
				device = owner
			case "machine alias":
				require.NoError(t, database.SetSyncState(ctx, MachineAliasKeyPrefix+source, owner))
			case "retained publication authority":
				require.NoError(t, database.SetSyncState(ctx, "artifact_local_machine_name", source))
			}
			provenance := ArchiveSourceProvenance{
				Attachment: key, ManifestID: "manifest-one",
				Source: ArchiveSourceIdentity{ArchiveID: "archive-one", DeviceID: device, RootID: "root-one", SourceKey: "source-one"},
			}
			require.ErrorIs(t, database.SetArchiveAttachmentTargets(ctx, key, "summary-one",
				[]ArchiveSourceTarget{{Provenance: provenance, Target: "version-one"}}), ErrArchiveSourceConflict)
			pending, err := database.PendingArchiveSources(ctx, key, ArchiveSourceIdentity{}, 10)
			require.NoError(t, err)
			assert.Empty(t, pending)
		})
	}
}
