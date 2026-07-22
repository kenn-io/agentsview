package artifact

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type metadataPointCountingStore struct {
	ArtifactStore
	listCalls atomic.Int32
	statCalls atomic.Int32
	openCalls atomic.Int32
}

func (s *metadataPointCountingStore) List(
	ctx context.Context, origin string, kind Kind, cursor Cursor, limit int,
) (Page, error) {
	s.listCalls.Add(1)
	return s.ArtifactStore.List(ctx, origin, kind, cursor, limit)
}

func (s *metadataPointCountingStore) Stat(ctx context.Context, ref Ref) (Entry, error) {
	s.statCalls.Add(1)
	return s.ArtifactStore.Stat(ctx, ref)
}

func (s *metadataPointCountingStore) Open(
	ctx context.Context, ref Ref,
) (Entry, VerifiedReader, error) {
	s.openCalls.Add(1)
	return s.ArtifactStore.Open(ctx, ref)
}

func TestMetadataRecorderAppendWritesCanonicalEvent(t *testing.T) {
	database := testDB(t)
	store := newTestArtifactStore(t)
	now := fixedHLCTime()
	recorder := NewMetadataRecorder(database, MetadataRecorderOptions{
		Origin: "laptop-a1b2c3",
		Store:  store,
		Now:    func() time.Time { return now },
	})

	value := json.RawMessage(`{"display_name":"Renamed session"}`)
	record, err := recorder.Append(context.Background(), MetadataEventInput{
		SessionID: "sess-1",
		Op:        MetadataOpRename,
		Value:     value,
	})
	require.NoError(t, err)

	assert.Equal(t, "2026-06-14T010203.000000001Z-00000000000000000000", record.HLC)
	assert.Equal(t, "laptop-a1b2c3", record.Origin)
	assert.Equal(t, "laptop-a1b2c3~sess-1", record.SessionGID)
	assert.Equal(t, MetadataOpRename, record.Op)

	data := readContractArtifact(t, store, record.Ref)
	assert.Equal(t,
		"{\"hlc\":\"2026-06-14T010203.000000001Z-00000000000000000000\",\"op\":\"rename\",\"origin\":\"laptop-a1b2c3\",\"session_gid\":\"laptop-a1b2c3~sess-1\",\"v\":1,\"value\":{\"display_name\":\"Renamed session\"}}\n",
		string(data),
	)
	assert.Equal(t, hashHex(data), record.Hash)
	assert.Equal(t, record.HLC+"-"+record.Hash+".json", record.Ref.Name)
	// The metadata event filename must be safe on every supported OS,
	// including Windows, which forbids these characters in path components.
	assert.NotContains(t, record.Ref.Name, ":",
		"metadata filename must not contain ':' (invalid on Windows)")
	for _, c := range `<>:"/\|?*` {
		assert.NotContainsf(t, record.HLC, string(c),
			"HLC %q must not contain %q (invalid in Windows filenames)", record.HLC, string(c))
	}
}

func TestMetadataRecorderAppendUsesCanonicalStoreIdentity(t *testing.T) {
	database := testDB(t)
	filesystem, err := newProtocolTestStore(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, filesystem.Close()) })
	store := &recordingArtifactStore{ArtifactStore: filesystem}
	recorder := NewMetadataRecorder(database, MetadataRecorderOptions{
		Store:  store,
		Origin: "laptop-a1b2c3",
		Now:    fixedHLCTime,
	})

	record, err := recorder.Append(t.Context(), MetadataEventInput{
		SessionID: "sess-1",
		Op:        MetadataOpStar,
	})
	require.NoError(t, err)
	require.Len(t, store.creates, 1)
	created := store.creates[0]
	assert.Equal(t, Kind(KindMeta), created.Ref.Kind)
	assert.Equal(t, record.HLC+"-"+record.Hash+".json", created.Ref.Name)
	assert.Equal(t, record.Hash, created.Identity.SHA256)
	assert.Equal(t, int64(len(readContractArtifact(t, store, created.Ref))), created.Identity.Size)
	assert.Equal(t, created.Ref, record.Ref)
}

func TestMetadataRecorderStoreModeDoesNotExposeLegacyPath(t *testing.T) {
	database := testDB(t)
	filesystem, err := newProtocolTestStore(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, filesystem.Close()) })
	recorder := NewMetadataRecorder(database, MetadataRecorderOptions{
		Store:  filesystem,
		Origin: "laptop-a1b2c3",
		Now:    fixedHLCTime,
	})

	record, err := recorder.Append(t.Context(), MetadataEventInput{
		SessionID: "sess-1",
		Op:        MetadataOpStar,
	})
	require.NoError(t, err)
	assert.Empty(t, record.Path, "ArtifactStore takes precedence over the legacy filesystem path")
	assert.Equal(t, Kind(KindMeta), record.Ref.Kind)
}

func TestMetadataRepairStoreWorkIsBoundedByTargetSessionProvenance(t *testing.T) {
	for _, unrelated := range []int{20, 1_000} {
		t.Run(strconv.Itoa(unrelated), func(t *testing.T) {
			database := testDB(t)
			filesystem, err := newProtocolTestStore(t.TempDir())
			require.NoError(t, err)
			store := &metadataPointCountingStore{ArtifactStore: filesystem}
			t.Cleanup(func() { require.NoError(t, store.Close()) })
			recorder := NewMetadataRecorder(database, MetadataRecorderOptions{
				Store: store, Origin: "desk-a1b2c3", Now: fixedHLCTime,
			})
			for index := range unrelated {
				_, err := recorder.Append(t.Context(), MetadataEventInput{
					SessionID: fmt.Sprintf("unrelated-%05d", index), Op: MetadataOpStar,
				})
				require.NoError(t, err)
			}
			_, err = recorder.Append(t.Context(), MetadataEventInput{
				SessionID: "target", Op: MetadataOpStar,
			})
			require.NoError(t, err)
			_, err = recorder.Append(t.Context(), MetadataEventInput{
				SessionID: "target", Op: MetadataOpSoftDelete,
			})
			require.NoError(t, err)

			store.listCalls.Store(0)
			store.statCalls.Store(0)
			store.openCalls.Store(0)
			repaired, err := recorder.RepairLocalSessionMetadata(
				t.Context(), "target", MetadataOpStar,
			)
			require.NoError(t, err)
			assert.Equal(t, 1, repaired)
			assert.Zero(t, store.listCalls.Load(),
				"targeted repair must never scan the metadata ledger")
			assert.Equal(t, int32(1), store.statCalls.Load())
			assert.Equal(t, int32(1), store.openCalls.Load())
		})
	}
}

func TestImportObservesRemoteHLCForLaterLocalEdits(t *testing.T) {
	ctx := context.Background()
	store := newTestArtifactStore(t)
	localOrigin := "desktop-d4e5f6"
	peerOrigin := "laptop-a1b2c3"
	database := testDB(t)
	seedSession(t, database, "sess-1", "alpha")
	peerGID := localOrigin + "~sess-1"

	recorderNow := fixedHLCTime()
	// A peer event whose wall time is ahead of the recorder's local clock but
	// within the drift bound.
	remoteStamp := HLCTimestamp{WallTime: recorderNow.Add(2 * time.Minute), Logical: 5}
	createMetadataArtifactInStore(
		t, store, replayRenameEvent(t, peerOrigin, peerGID, remoteStamp.String(), "Peer name"),
	)

	recorder := NewMetadataRecorder(database, MetadataRecorderOptions{
		Origin: localOrigin,
		Store:  store,
		Now:    func() time.Time { return recorderNow },
	})

	imported, err := recorder.ImportFromStore(ctx, store)
	require.NoError(t, err)
	assert.Equal(t, 1, imported.Metadata)

	// The next local edit must receive an HLC strictly after the observed
	// remote HLC even though the local wall clock is behind it.
	rec, err := recorder.Append(ctx, MetadataEventInput{
		SessionID: "sess-1",
		Op:        MetadataOpStar,
	})
	require.NoError(t, err)
	localStamp, err := ParseHLCTimestamp(rec.HLC)
	require.NoError(t, err)
	assert.Equal(t, 1, localStamp.Compare(remoteStamp),
		"local HLC %s must be after observed remote HLC %s", rec.HLC, remoteStamp.String())
}

func TestMetadataRecorderWithoutOriginIsNoOp(t *testing.T) {
	database := testDB(t)
	store := newTestArtifactStore(t)
	recorder := NewMetadataRecorder(database, MetadataRecorderOptions{Store: store})

	record, err := recorder.Append(context.Background(), MetadataEventInput{
		SessionID: "sess-1",
		Op:        MetadataOpStar,
	})
	require.NoError(t, err)
	assert.Equal(t, MetadataRecord{}, record)

	origin, err := StoredOrigin(database)
	require.NoError(t, err)
	assert.Empty(t, origin,
		"recorder must not mint an origin for a machine that never opted into artifact sync")
	assertNoPublishedArtifacts(t, store, "desk-a1b2c3")

	repaired, err := recorder.RepairLocalSessionMetadata(context.Background(), "sess-1")
	require.NoError(t, err)
	assert.Zero(t, repaired)
}

func TestMetadataRecorderRecordsAfterOriginAdopted(t *testing.T) {
	database := testDB(t)
	store := newTestArtifactStore(t)
	recorder := NewMetadataRecorder(database, MetadataRecorderOptions{Store: store})

	record, err := recorder.Append(context.Background(), MetadataEventInput{
		SessionID: "sess-1",
		Op:        MetadataOpStar,
	})
	require.NoError(t, err)
	assert.Empty(t, record.Origin)

	require.NoError(t, AdoptOrigin(database, "desk-a1b2c3"))

	record, err = recorder.Append(context.Background(), MetadataEventInput{
		SessionID: "sess-1",
		Op:        MetadataOpStar,
	})
	require.NoError(t, err)
	assert.Equal(t, "desk-a1b2c3", record.Origin)
	_, err = store.Stat(t.Context(), record.Ref)
	require.NoError(t, err,
		"recorder must start writing events once the origin is adopted, without reconstruction")
}

func TestMetadataSessionGID(t *testing.T) {
	assert.Equal(t, "desk-a1b2c3~sess-1", MetadataSessionGID("desk-a1b2c3", "sess-1"))
	assert.Equal(t, "laptop-d4e5f6~sess-1", MetadataSessionGID("desk-a1b2c3", "laptop-d4e5f6~sess-1"))
}
