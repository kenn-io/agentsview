package artifact

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
)

func openTransportStore(t *testing.T, root string) ArtifactStore {
	t.Helper()
	require.NoError(t, os.MkdirAll(root, 0o755))
	store, err := newProtocolTestStore(root)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	return store
}

func listTransportArtifacts(
	ctx context.Context, store ArtifactStore, origin string,
) (OriginArtifactIndex, error) {
	index := OriginArtifactIndex{Origin: origin}
	for _, kind := range transportKinds {
		var names []string
		err := visitStoreKind(ctx, store, origin, kind, func(entry Entry) error {
			wire, err := ToWireRef(entry.Ref)
			if err != nil {
				return err
			}
			names = append(names, wire.Name)
			return nil
		})
		if err != nil {
			return OriginArtifactIndex{}, err
		}
		switch kind {
		case KindSegments:
			index.Segments = names
		case KindRaw:
			index.Raw = names
		case KindManifests:
			index.Manifests = names
		case KindMeta:
			index.Meta = names
		case KindCheckpoints:
			index.Checkpoints = names
		}
	}
	return index, nil
}

func readTransportArtifact(
	ctx context.Context,
	store ArtifactStore,
	origin, kind, name string,
) (_ []byte, retErr error) {
	ref, err := FromWireRef(origin, Kind(kind), name)
	if err != nil {
		return nil, err
	}
	entry, found, err := findStoreEntry(ctx, store, ref)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, ErrArtifactNotFound
	}
	spool, _, _, err := spoolWireArtifact(ctx, store, entry)
	if err != nil {
		return nil, err
	}
	defer func() { retErr = errors.Join(retErr, closeAndRemoveTransportSpool(spool)) }()
	return io.ReadAll(spool)
}

func onlyTransportRef(
	t *testing.T, store ArtifactStore, origin string, kind Kind,
) Ref {
	t.Helper()
	page, err := firstStoreEntryPage(t.Context(), store, origin, kind, 10)
	require.NoError(t, err)
	require.Empty(t, page.Next)
	require.Len(t, page.Items, 1)
	return page.Items[0].Ref
}

func wireTransportArtifact(
	t *testing.T, store ArtifactStore, ref Ref,
) (WireRef, []byte) {
	t.Helper()
	wire, err := ToWireRef(ref)
	require.NoError(t, err)
	data, err := readTransportArtifact(
		t.Context(), store, ref.Origin, string(ref.Kind), wire.Name,
	)
	require.NoError(t, err)
	return wire, data
}

type storeBoundaryTransport struct {
	prepared  ArtifactStore
	exchanged ArtifactStore
}

type transportChangeObservingStore struct {
	ArtifactStore
	changed []Entry
	pending Entry
}

func (s *transportChangeObservingStore) RecordTransportChanged(
	ctx context.Context, entry Entry,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.changed = append(s.changed, entry)
	return nil
}

func (s *transportChangeObservingStore) PendingTransportRepair(
	ctx context.Context, ref Ref,
) (Entry, bool, error) {
	if err := ctx.Err(); err != nil {
		return Entry{}, false, err
	}
	return s.pending, s.pending.Ref == ref, nil
}

func (s *transportChangeObservingStore) RepairTransportArtifact(
	ctx context.Context, entry Entry, trusted io.Reader,
) error {
	if entry != s.pending {
		return errors.New("unexpected transport repair")
	}
	if err := s.Quarantine(ctx, entry.Ref, "transport parity repair"); err != nil {
		return err
	}
	if _, err := s.Create(ctx, entry.Ref, entry.Identity,
		canonicalArtifactMediaType(entry.Ref.Kind), trusted); err != nil {
		return err
	}
	return nil
}

func (s *transportChangeObservingStore) AcknowledgeTransportRepair(
	ctx context.Context, entry Entry,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if entry != s.pending {
		return errors.New("unexpected transport repair acknowledgement")
	}
	s.pending = Entry{}
	return nil
}

func TestTransportReportsChangedArtifacts(t *testing.T) {
	factories := []struct {
		name string
		open func(*testing.T) Transport
	}{
		{
			name: "folder",
			open: func(t *testing.T) Transport {
				return openFolderTransportForTest(t, t.TempDir())
			},
		},
		{
			name: "http",
			open: func(t *testing.T) Transport {
				peer := newFakeArtifactPeer()
				server := httptest.NewServer(peer)
				t.Cleanup(server.Close)
				transport, err := newHTTPTransport(server.URL, "", false)
				require.NoError(t, err)
				t.Cleanup(func() { require.NoError(t, transport.Close()) })
				return transport
			},
		},
		{
			name: "s3",
			open: func(t *testing.T) Transport {
				mock := newMockS3(t, "bucket", 2)
				server := httptest.NewServer(mock)
				t.Cleanup(server.Close)
				transport, err := newObjectTransport(
					"s3://bucket/arts", testObjectOptions(server.URL),
				)
				require.NoError(t, err)
				return transport
			},
		},
	}

	for _, factory := range factories {
		t.Run(factory.name, func(t *testing.T) {
			origin := "peer-a1b2c3"
			database := testDB(t)
			seedSession(t, database, "session-1", "alpha")
			source := openTransportStore(t, t.TempDir())
			_, err := ExportToStore(t.Context(), database, source, ExportOptions{
				Origin: origin, Full: true,
			})
			require.NoError(t, err)
			recorder := NewMetadataRecorder(database, MetadataRecorderOptions{
				Origin: origin, Store: source, Now: fixedHLCTime,
			})
			_, err = recorder.Append(t.Context(), MetadataEventInput{
				SessionID: "session-1", Op: MetadataOpStar,
			})
			require.NoError(t, err)

			checkpoint := onlyTransportRef(t, source, origin, KindCheckpoints)
			metadata := onlyTransportRef(t, source, origin, KindMeta)
			checkpointEntry, err := source.Stat(t.Context(), checkpoint)
			require.NoError(t, err)
			metadataEntry, err := source.Stat(t.Context(), metadata)
			require.NoError(t, err)

			transport := factory.open(t)
			require.NoError(t, transport.Exchange(t.Context(), source))
			destination := &transportChangeObservingStore{
				ArtifactStore: openTransportStore(t, t.TempDir()),
			}
			require.NoError(t, transport.Exchange(t.Context(), destination))
			assert.Equal(t, 1, countTransportChange(destination.changed, checkpointEntry))
			assert.Equal(t, 1, countTransportChange(destination.changed, metadataEntry))

			require.NoError(t, transport.Exchange(t.Context(), destination))
			assert.Equal(t, 1, countTransportChange(destination.changed, checkpointEntry),
				"duplicate immutable exchange must not report a change")
			assert.Equal(t, 1, countTransportChange(destination.changed, metadataEntry),
				"duplicate immutable exchange must not report a change")

			destination.pending = checkpointEntry
			require.NoError(t, transport.Exchange(t.Context(), destination))
			assert.Equal(t, 2, countTransportChange(destination.changed, checkpointEntry),
				"verified repair must report the exact changed entry")
		})
	}
}

func countTransportChange(changed []Entry, expected Entry) int {
	count := 0
	for _, entry := range changed {
		if entry.Ref == expected.Ref && entry.Identity == expected.Identity {
			count++
		}
	}
	return count
}

type closeTrackingTransport struct {
	prepareErr error
	closeErr   error
	closed     bool
}

func (t *closeTrackingTransport) Prepare(context.Context, ArtifactStore) error {
	return t.prepareErr
}

func (*closeTrackingTransport) Exchange(context.Context, ArtifactStore) error {
	return nil
}

func (t *closeTrackingTransport) Close() error {
	t.closed = true
	return t.closeErr
}

type membershipStatStore struct {
	ArtifactStore
	created   bool
	statCalls int
}

func (s *membershipStatStore) Stat(ctx context.Context, ref Ref) (Entry, error) {
	s.statCalls++
	return s.ArtifactStore.Stat(ctx, ref)
}

func (s *membershipStatStore) Create(
	ctx context.Context, ref Ref, identity Identity, mediaType string, body io.Reader,
) (CreateResult, error) {
	result, err := s.ArtifactStore.Create(ctx, ref, identity, mediaType, body)
	if err == nil {
		s.created = true
	}
	return result, err
}

func (t *storeBoundaryTransport) Prepare(ctx context.Context, store ArtifactStore) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	t.prepared = store
	return nil
}

func (t *storeBoundaryTransport) Exchange(ctx context.Context, store ArtifactStore) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	t.exchanged = store
	return nil
}

func TestTransportBoundaryPassesTheCanonicalStore(t *testing.T) {
	store, err := newProtocolTestStore(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	transport := &storeBoundaryTransport{}

	require.NoError(t, transport.Prepare(t.Context(), store))
	require.NoError(t, transport.Exchange(t.Context(), store))

	assert.Same(t, store, transport.prepared)
	assert.Same(t, store, transport.exchanged)
	var _ Transport = transport
}

func TestCoordinatedTransportStoreFindsExactQueuedRepair(t *testing.T) {
	database := testDB(t)
	store, err := newProtocolTestStore(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	body := []byte("repair me")
	ref := requireContractRef(t, contractOrigin, KindRaw, hashHex(body))
	entry := Entry{Ref: ref, Identity: identityForBytes(t, body)}
	require.NoError(t, database.EnqueueArtifactRepair(t.Context(), db.ArtifactRepair{
		Origin: ref.Origin, Kind: string(ref.Kind), Name: ref.Name,
		SHA256: entry.Identity.SHA256, Size: entry.Identity.Size,
	}))
	coordinator := NewStoreImportCoordinator(database, store, "local-a1b2c3")
	wrapped := newCoordinatedTransportStore(database, store, coordinator)

	got, ok, err := wrapped.PendingTransportRepair(t.Context(), ref)
	require.NoError(t, err)
	assert.True(t, ok)
	assert.Equal(t, entry, got)

	_, ok, err = wrapped.PendingTransportRepair(t.Context(), requireContractRef(
		t, contractOrigin, KindRaw, strings64("f"),
	))
	require.NoError(t, err)
	assert.False(t, ok)
}

func TestRepositoryConstructsFolderTransportOnlyAfterIdentityValidation(t *testing.T) {
	dataDir := t.TempDir()
	repository, err := OpenRepository(t.Context(), dataDir)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, repository.Close()) })

	transport, err := repository.NewFolderTransport(filepath.Join(dataDir, "artifacts", "share"))
	require.ErrorContains(t, err, "must not overlap")
	assert.Nil(t, transport)

	target := t.TempDir()
	transport, err = repository.NewFolderTransport(target)
	require.NoError(t, err)
	folder, ok := transport.(*folderTransport)
	require.True(t, ok)
	assert.NotNil(t, folder.root)
	t.Cleanup(func() { require.NoError(t, folder.Close()) })
}

func TestRepositoryFolderTransportRetainsValidatedOpenedTarget(t *testing.T) {
	repository, err := OpenRepository(t.Context(), t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, repository.Close()) })
	target := t.TempDir()
	transport, err := repository.NewFolderTransport(target)
	require.NoError(t, err)
	moved := target + "-moved"
	require.NoError(t, os.Rename(target, moved))
	require.NoError(t, os.Mkdir(target, 0o755))

	body := []byte("retained target")
	identity := identityForBytes(t, body)
	ref, err := NewRef("peer-a1b2c3", KindRaw, identity.SHA256)
	require.NoError(t, err)
	store := openTransportStore(t, t.TempDir())
	created, err := store.Create(t.Context(), ref, identity,
		canonicalArtifactMediaType(ref.Kind), bytes.NewReader(body))
	require.NoError(t, err)
	wire, err := ToWireRef(created.Entry.Ref)
	require.NoError(t, err)

	require.NoError(t, transport.Exchange(t.Context(), store))
	assert.FileExists(t, filepath.Join(moved, folderWirePath(wire)))
	assert.NoFileExists(t, filepath.Join(target, folderWirePath(wire)))
}

func TestRepositoryFolderTransportCloseIsIdempotentAndStopsUse(t *testing.T) {
	repository, err := OpenRepository(t.Context(), t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, repository.Close()) })
	transport, err := repository.NewFolderTransport(t.TempDir())
	require.NoError(t, err)
	folder, ok := transport.(*folderTransport)
	require.True(t, ok)
	require.NoError(t, folder.Close())
	require.NoError(t, folder.Close())

	err = folder.Prepare(t.Context(), openTransportStore(t, t.TempDir()))
	assert.ErrorIs(t, err, os.ErrClosed)
	err = folder.Exchange(t.Context(), openTransportStore(t, t.TempDir()))
	assert.ErrorIs(t, err, os.ErrClosed)
}

func TestSyncWithTransportJoinsCloseErrorOnPrepareFailure(t *testing.T) {
	prepareErr := errors.New("prepare failed")
	closeErr := errors.New("close failed")
	transport := &closeTrackingTransport{prepareErr: prepareErr, closeErr: closeErr}

	_, err := syncWithTransport(t.Context(), testDB(t), SyncOptions{
		DataDir: t.TempDir(),
		Origin:  "peer-a1b2c3",
	}, transport)

	assert.True(t, transport.closed)
	assert.ErrorIs(t, err, prepareErr)
	assert.ErrorIs(t, err, closeErr)
}

func TestFolderPullUsesStatForRemotePointMembership(t *testing.T) {
	body := []byte("remote point membership")
	identity := identityForBytes(t, body)
	ref, err := NewRef("peer-a1b2c3", KindRaw, identity.SHA256)
	require.NoError(t, err)
	wire, err := ToWireRef(ref)
	require.NoError(t, err)
	targetPath := t.TempDir()
	require.NoError(t, os.MkdirAll(
		filepath.Join(targetPath, wire.Origin, string(wire.Kind)), 0o755,
	))
	require.NoError(t, os.WriteFile(
		filepath.Join(targetPath, folderWirePath(wire)), body, 0o644,
	))
	transport := openFolderTransportForTest(t, targetPath)
	base := openTransportStore(t, t.TempDir())
	store := &membershipStatStore{ArtifactStore: base}

	require.NoError(t, transport.Exchange(t.Context(), store))
	assert.Positive(t, store.statCalls)
	assert.True(t, store.created, "uncataloged wire content must still be ingested")
	_, err = store.Stat(t.Context(), ref)
	require.NoError(t, err)
}
