package rawsync

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/artifact"
)

func TestArtifactObjectStoreContract(t *testing.T) {
	t.Parallel()

	repository, err := artifact.OpenRepository(t.Context(), t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, repository.Close()) })
	store, err := NewArtifactObjectStore(repository.Content())
	require.NoError(t, err)
	identity, err := NewAuthIdentity("tenant-a", "device-a")
	require.NoError(t, err)
	body := []byte("raw bytes")
	ref := objectRefForBytes(t, body)

	created, err := store.PutObject(t.Context(), identity.TenantID, ref, bytes.NewReader(body))
	require.NoError(t, err)
	assert.True(t, created.Created)
	assert.Equal(t, ref, created.Info.Ref)
	retried, err := store.PutObject(t.Context(), identity.TenantID, ref, bytes.NewReader(body))
	require.NoError(t, err)
	assert.False(t, retried.Created)

	stat, err := store.StatObject(t.Context(), identity.TenantID, ref)
	require.NoError(t, err)
	assert.Equal(t, ref, stat.Ref)
	info, reader, err := store.OpenObject(t.Context(), identity.TenantID, ref)
	require.NoError(t, err)
	got, err := io.ReadAll(reader)
	require.NoError(t, err)
	require.NoError(t, reader.Verify())
	require.NoError(t, reader.Close())
	assert.Equal(t, body, got)
	assert.Equal(t, ref, info.Ref)
	wrongLength := ObjectRef{SHA256: ref.SHA256, Length: ref.Length + 1}
	_, err = store.StatObject(t.Context(), identity.TenantID, wrongLength)
	assert.ErrorIs(t, err, ErrConflict)
	_, wrongReader, err := store.OpenObject(t.Context(), identity.TenantID, wrongLength)
	assert.ErrorIs(t, err, ErrConflict)
	assert.Nil(t, wrongReader)
	if wrongReader != nil {
		_ = wrongReader.Close()
	}
	_, err = store.MissingObjects(
		t.Context(), identity.TenantID, []ObjectRef{wrongLength},
	)
	assert.ErrorIs(t, err, ErrConflict)

	missing, err := store.MissingObjects(t.Context(), identity.TenantID, []ObjectRef{
		ref,
		{SHA256: strings.Repeat("c", 64), Length: 7},
		{SHA256: strings.Repeat("c", 64), Length: 7},
	})
	require.NoError(t, err)
	assert.Equal(t, []ObjectRef{{SHA256: strings.Repeat("c", 64), Length: 7}}, missing)

	otherTenantMissing, err := store.MissingObjects(t.Context(), "tenant-b", []ObjectRef{ref})
	require.NoError(t, err)
	assert.Equal(t, []ObjectRef{ref}, otherTenantMissing)
	_, _, err = store.OpenObject(t.Context(), "tenant-b", ref)
	assert.ErrorIs(t, err, ErrNotFound)
}

func TestArtifactObjectStoreRejectsInvalidWritesAndRequests(t *testing.T) {
	t.Parallel()

	repository, err := artifact.OpenRepository(t.Context(), t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, repository.Close()) })
	store, err := NewArtifactObjectStore(repository.Content())
	require.NoError(t, err)
	ref := objectRefForBytes(t, []byte("expected"))

	_, err = store.PutObject(t.Context(), "tenant-a", ref, bytes.NewReader([]byte("corrupt")))
	assert.ErrorIs(t, err, ErrConflict)
	_, err = store.PutObject(t.Context(), "tenant-a", ref, bytes.NewReader([]byte("EXPected")))
	assert.ErrorIs(t, err, ErrConflict)
	_, err = store.PutObject(t.Context(), "bad/tenant", ref, bytes.NewReader([]byte("expected")))
	assert.ErrorIs(t, err, ErrInvalid)
	_, err = store.MissingObjects(t.Context(), "tenant-a", []ObjectRef{
		ref,
		{SHA256: ref.SHA256, Length: ref.Length + 1},
	})
	assert.ErrorIs(t, err, ErrConflict)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = store.PutObject(ctx, "tenant-a", ref, bytes.NewReader([]byte("expected")))
	assert.ErrorIs(t, err, context.Canceled)
	_, err = NewArtifactObjectStore(nil)
	assert.ErrorIs(t, err, ErrInvalid)
}

func TestArtifactObjectStoreRetainsCanonicalManifestEnvelope(t *testing.T) {
	t.Parallel()

	repository, err := artifact.OpenRepository(t.Context(), t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, repository.Close()) })
	store, err := NewArtifactObjectStore(repository.Content())
	require.NoError(t, err)
	identity, err := NewAuthIdentity("tenant-a", "device-a")
	require.NoError(t, err)
	manifest, err := ValidateAndCanonicalize(identity, validManifest(), DefaultManifestLimits())
	require.NoError(t, err)

	created, err := store.PutManifest(t.Context(), manifest)
	require.NoError(t, err)
	assert.True(t, created.Created)
	assert.Equal(t, ObjectRef{SHA256: manifest.ManifestID, Length: int64(len(manifest.CanonicalJSON))}, created.Info.Ref)
	retried, err := store.PutManifest(t.Context(), manifest)
	require.NoError(t, err)
	assert.False(t, retried.Created)

	info, reader, err := store.OpenManifest(t.Context(), identity, manifest.ManifestID)
	require.NoError(t, err)
	got, err := io.ReadAll(reader)
	require.NoError(t, err)
	require.NoError(t, reader.Verify())
	require.NoError(t, reader.Close())
	assert.Equal(t, manifest.CanonicalJSON, got)
	assert.Equal(t, created.Info.Ref, info.Ref)

	other, err := NewAuthIdentity("tenant-b", "device-a")
	require.NoError(t, err)
	_, _, err = store.OpenManifest(t.Context(), other, manifest.ManifestID)
	assert.ErrorIs(t, err, ErrNotFound)
}

func TestArtifactObjectStoreAcceptsCanonicalManifestBeyondDefaultPolicy(t *testing.T) {
	t.Parallel()

	repository, err := artifact.OpenRepository(t.Context(), t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, repository.Close()) })
	store, err := NewArtifactObjectStore(repository.Content())
	require.NoError(t, err)
	identity, err := NewAuthIdentity("tenant-a", "device-a")
	require.NoError(t, err)
	manifest := validManifest()
	manifest.Entries[0].Path = strings.Repeat("p", DefaultManifestLimits().MaxPathBytes+1)
	limits := DefaultManifestLimits()
	limits.MaxPathBytes++
	canonical, err := ValidateAndCanonicalize(identity, manifest, limits)
	require.NoError(t, err)

	result, err := store.PutManifest(t.Context(), canonical)
	require.NoError(t, err)
	assert.True(t, result.Created)
}

func objectRefForBytes(t *testing.T, body []byte) ObjectRef {
	t.Helper()
	sum := sha256.Sum256(body)
	ref, err := NewObjectRef(hex.EncodeToString(sum[:]), int64(len(body)))
	require.NoError(t, err)
	return ref
}
