//go:build pgtest

package postgres

import (
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/artifact"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/rawsync"
)

func TestRawResumableUploadEndToEnd(t *testing.T) {
	pg, dataDir := newRawUploadTestDatabase(t)
	repository, err := artifact.OpenRepository(t.Context(), dataDir)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, repository.Close()) })
	objects, err := rawsync.NewArtifactObjectStore(repository.Content())
	require.NoError(t, err)
	metadata, err := NewRawIngestStore(pg)
	require.NoError(t, err)
	custody, err := rawsync.NewService(
		objects, metadata, rawsync.DefaultManifestLimits(), "parser-data-17",
	)
	require.NoError(t, err)
	sessions, err := NewRawUploadStore(pg, dataDir)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, sessions.Close()) })
	uploads, err := rawsync.NewUploadService(
		sessions, custody, rawsync.DefaultUploadSessionTTL,
	)
	require.NoError(t, err)

	identity := rawUploadIdentity(t, pg, "tenant-a", "device-a")
	body := []byte("durable resumable raw object")
	object := rawCustodyObjectRef(t, body)
	session, created, err := uploads.Start(
		t.Context(), identity, parser.AgentCodex, object,
	)
	require.NoError(t, err)
	assert.True(t, created)

	partial, err := uploads.Append(
		t.Context(), identity, session.ID, 0, body[:8],
	)
	require.NoError(t, err)
	assert.Equal(t, int64(8), partial.Offset)
	completed, err := uploads.Append(
		t.Context(), identity, session.ID, partial.Offset, body[8:],
	)
	require.NoError(t, err)
	assert.True(t, completed.Complete)
	assert.Equal(t, object.Length, completed.Offset)

	missing, err := custody.MissingObjects(
		t.Context(), identity, parser.AgentCodex, []rawsync.ObjectRef{object},
	)
	require.NoError(t, err)
	assert.Empty(t, missing)
	_, reader, err := objects.OpenObject(t.Context(), identity.TenantID, object)
	require.NoError(t, err)
	stored, err := io.ReadAll(reader)
	require.NoError(t, err)
	require.NoError(t, reader.Verify())
	require.NoError(t, reader.Close())
	assert.Equal(t, body, stored)

	var state string
	var offset int64
	require.NoError(t, pg.QueryRowContext(t.Context(), `
		SELECT state, offset_bytes
		FROM raw_upload_sessions
		WHERE upload_id = $1`, session.ID).Scan(&state, &offset))
	assert.Equal(t, "complete", state)
	assert.Equal(t, object.Length, offset)
	assert.Equal(t, 1, rawIngestTableCount(t, pg, "raw_objects"))

	status, err := uploads.Status(
		t.Context(), identity, session.ID,
	)
	require.NoError(t, err)
	assert.True(t, status.Complete)
	assert.Equal(t, rawsync.DefaultUploadSessionTTL, status.ExpiresAt.Sub(status.CreatedAt))
}
