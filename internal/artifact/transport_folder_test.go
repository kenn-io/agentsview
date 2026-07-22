package artifact

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
)

func openFolderTransportForTest(t *testing.T, target string) *folderTransport {
	t.Helper()
	transport, err := openFolderTransport(target)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, transport.Close()) })
	return transport
}

type forbidArtifactOpenStore struct{ ArtifactStore }

func (s forbidArtifactOpenStore) Open(ctx context.Context, ref Ref) (Entry, VerifiedReader, error) {
	if ref.Kind == KindManifests || ref.Kind == KindSegments {
		return Entry{}, nil, errors.New("unchanged artifact content must not be reopened")
	}
	return s.ArtifactStore.Open(ctx, ref)
}

func TestFolderTransportUnchangedExchangeDoesNotReopenCommonContent(t *testing.T) {
	ctx := context.Background()
	origin := "laptop-a1b2c3"
	localStore := exportStore(t, origin, func(database *db.DB) {
		seedSession(t, database, "sess-1", "alpha")
	})
	target := t.TempDir()
	transport := openFolderTransportForTest(t, target)
	require.NoError(t, transport.Exchange(ctx, localStore))

	require.NoError(t, transport.Exchange(ctx, forbidArtifactOpenStore{localStore}),
		"name-set convergence must not reread payloads already held by both stores")
}

func TestFolderTransportRejectsSymlinkedArtifactKind(t *testing.T) {
	origin := "laptop-a1b2c3"
	localStore := exportStore(t, origin, func(database *db.DB) {
		seedSession(t, database, "sess-1", "alpha")
	})
	target := t.TempDir()
	segments := filepath.Join(target, origin, KindSegments)
	external := filepath.Join(t.TempDir(), KindSegments)
	require.NoError(t, os.MkdirAll(filepath.Dir(segments), 0o755))
	require.NoError(t, os.MkdirAll(external, 0o755))
	require.NoError(t, os.Symlink(external, segments))

	transport := openFolderTransportForTest(t, target)
	err := transport.Exchange(context.Background(), localStore)

	require.Error(t, err)
	targetSegments, globErr := filepath.Glob(
		filepath.Join(target, origin, KindSegments, "*"),
	)
	require.NoError(t, globErr)
	assert.Empty(t, targetSegments,
		"folder exchange must not follow an artifact-kind symlink")
}

func TestPublishFolderWireRejectsDivergentImmutableCollision(t *testing.T) {
	body := []byte("canonical body")
	identity := identityForBytes(t, body)
	ref, err := NewRef("peer-a1b2c3", KindRaw, identity.SHA256)
	require.NoError(t, err)
	store := openTransportStore(t, t.TempDir())
	created, err := store.Create(t.Context(), ref, identity,
		canonicalArtifactMediaType(ref.Kind), bytes.NewReader(body))
	require.NoError(t, err)
	wire, err := ToWireRef(ref)
	require.NoError(t, err)
	targetPath := t.TempDir()
	require.NoError(t, os.MkdirAll(
		filepath.Join(targetPath, wire.Origin, string(wire.Kind)), 0o755,
	))
	require.NoError(t, os.WriteFile(
		filepath.Join(targetPath, folderWirePath(wire)), []byte("collision"), 0o644,
	))
	target, err := openArtifactRoot(targetPath, "test target")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, target.Close()) })

	err = publishFolderWire(t.Context(), store, target, created.Entry)
	require.ErrorIs(t, err, ErrArtifactConflict)
	got, readErr := os.ReadFile(filepath.Join(targetPath, folderWirePath(wire)))
	require.NoError(t, readErr)
	assert.Equal(t, []byte("collision"), got)
}

func TestTransportMemoryFolderExchangeRemainsBounded(t *testing.T) {
	measure := func(size int64) (uint64, uint64) {
		hasher := sha256.New()
		_, err := io.CopyN(hasher, repeatedByteReader('x'), size)
		require.NoError(t, err)
		identity, err := NewIdentity(hex.EncodeToString(hasher.Sum(nil)), size)
		require.NoError(t, err)
		ref, err := NewRef("peer-a1b2c3", KindRaw, identity.SHA256)
		require.NoError(t, err)
		source := openTransportStore(t, t.TempDir())
		_, err = source.Create(t.Context(), ref, identity,
			canonicalArtifactMediaType(ref.Kind),
			io.LimitReader(repeatedByteReader('x'), size),
		)
		require.NoError(t, err)
		target := t.TempDir()
		transport := openFolderTransportForTest(t, target)
		runtime.GC()
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		require.NoError(t, transport.Exchange(t.Context(), source))
		runtime.ReadMemStats(&after)
		outbound := after.TotalAlloc - before.TotalAlloc

		destination := openTransportStore(t, t.TempDir())
		runtime.GC()
		runtime.ReadMemStats(&before)
		require.NoError(t, transport.Exchange(t.Context(), destination))
		runtime.ReadMemStats(&after)
		_, err = destination.Stat(t.Context(), ref)
		require.NoError(t, err)
		return outbound, after.TotalAlloc - before.TotalAlloc
	}

	smallOut, smallIn := measure(1 << 20)
	largeOut, largeIn := measure(24 << 20)
	assert.Less(t, largeOut, smallOut+(4<<20),
		"real folder Exchange upload allocation must remain bounded")
	assert.Less(t, largeIn, smallIn+(4<<20),
		"real folder Exchange download allocation must remain bounded")
}
