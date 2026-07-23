package artifact

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/docbank"
)

const retentionOrigin = "retention-a1b2c3"

type retentionFixture struct {
	store ArtifactStore
	old   map[Kind][]Ref
	live  map[Kind][]Ref
	meta  Ref
}

func TestGarbageCollectTrashesOnlySupersededLogicalClosure(t *testing.T) {
	fixture := newRetentionFixture(t)
	now := time.Now().Add(48 * time.Hour)

	result, err := GarbageCollect(t.Context(), GCOptions{
		Store: fixture.store,
		Grace: time.Hour,
		Now:   now,
	})
	require.NoError(t, err)

	assert.Equal(t, 1, result.Origins)
	assert.Zero(t, result.SkippedOrigins)
	assert.Equal(t, 3, result.Eligible)
	assert.Equal(t, 3, result.Deleted)
	for _, refs := range fixture.old {
		for _, ref := range refs {
			_, err := fixture.store.Stat(t.Context(), ref)
			assert.ErrorIs(t, err, ErrArtifactNotFound, ref.Name)
		}
	}
	for _, refs := range fixture.live {
		for _, ref := range refs {
			_, err := fixture.store.Stat(t.Context(), ref)
			assert.NoError(t, err, ref.Name)
		}
	}
	_, err = fixture.store.Stat(t.Context(), fixture.meta)
	assert.NoError(t, err, "metadata and purge tombstones are retained by logical GC")
}

func TestGarbageCollectDryRunAndGracePreserveCandidates(t *testing.T) {
	for _, tc := range []struct {
		name   string
		dryRun bool
		now    func(time.Time) time.Time
		want   func(t *testing.T, result GCResult)
	}{
		{
			name:   "dry run",
			dryRun: true,
			now:    func(created time.Time) time.Time { return created.Add(48 * time.Hour) },
			want: func(t *testing.T, result GCResult) {
				assert.Equal(t, 3, result.Eligible)
				assert.Zero(t, result.Deleted)
			},
		},
		{
			name: "inside grace",
			now:  func(created time.Time) time.Time { return created.Add(30 * time.Minute) },
			want: func(t *testing.T, result GCResult) {
				assert.Zero(t, result.Eligible)
				assert.Equal(t, 3, result.KeptByGrace)
				assert.Zero(t, result.Deleted)
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newRetentionFixture(t)
			created, err := fixture.store.Stat(t.Context(), fixture.old[KindCheckpoints][0])
			require.NoError(t, err)
			result, err := GarbageCollect(t.Context(), GCOptions{
				Store:  fixture.store,
				Grace:  time.Hour,
				DryRun: tc.dryRun,
				Now:    tc.now(created.Modified),
			})
			require.NoError(t, err)
			tc.want(t, result)
			for _, refs := range fixture.old {
				for _, ref := range refs {
					_, err := fixture.store.Stat(t.Context(), ref)
					assert.NoError(t, err, ref.Name)
				}
			}
		})
	}
}

func TestGarbageCollectPreservesRawSourceReachableFromLatestCheckpoint(t *testing.T) {
	store := newRetentionStore(t)
	database := testDB(t)
	seedSession(t, database, "raw-session", "alpha")
	_, err := ExportToStore(t.Context(), database, store, ExportOptions{
		Origin: retentionOrigin, Full: true,
	})
	require.NoError(t, err)

	latest, found, err := latestGCCheckpoint(t.Context(), store, retentionOrigin)
	require.NoError(t, err)
	require.True(t, found)
	checkpointData, err := readGCStoreArtifact(t.Context(), store, latest, checkpointDecodedLimit)
	require.NoError(t, err)
	var cp checkpoint
	require.NoError(t, json.Unmarshal(checkpointData, &cp))
	gid := retentionOrigin + "~raw-session"
	manifestRef := Ref{
		Origin: retentionOrigin, Kind: KindManifests, Name: cp.Sessions[gid] + ".json",
	}
	manifestEntry, err := store.Stat(t.Context(), manifestRef)
	require.NoError(t, err)
	manifestData, err := readGCStoreArtifact(t.Context(), store, manifestEntry, manifestDecodedLimit)
	require.NoError(t, err)
	var m manifest
	require.NoError(t, json.Unmarshal(manifestData, &m))

	rawBody := []byte("canonical raw source")
	rawRef := createRetentionRef(t, store, Ref{
		Origin: retentionOrigin, Kind: KindRaw, Name: hashHex(rawBody),
	}, rawBody)
	m.RawSource = &rawSourceRef{Hash: rawRef.Name, Size: int64(len(rawBody))}
	updatedManifest, err := canonicalJSON(m)
	require.NoError(t, err)
	updatedManifestHash := hashHex(updatedManifest)
	createRetentionRef(t, store, Ref{
		Origin: retentionOrigin, Kind: KindManifests, Name: updatedManifestHash + ".json",
	}, updatedManifest)
	cp.Sequence++
	cp.Sessions[gid] = updatedManifestHash
	updatedCheckpoint, err := canonicalJSON(cp)
	require.NoError(t, err)
	createRetentionRef(t, store, Ref{
		Origin: retentionOrigin, Kind: KindCheckpoints,
		Name: fmt.Sprintf("cp-%010d.json", cp.Sequence),
	}, updatedCheckpoint)
	staleRaw := createRetentionRef(t, store, Ref{
		Origin: retentionOrigin, Kind: KindRaw, Name: hashHex([]byte("stale raw")),
	}, []byte("stale raw"))

	result, err := GarbageCollect(t.Context(), GCOptions{
		Store: store, Now: time.Now().Add(time.Hour),
	})
	require.NoError(t, err)
	_, err = store.Stat(t.Context(), rawRef)
	assert.NoError(t, err, "latest checkpoint raw dependency must remain live")
	_, err = store.Stat(t.Context(), staleRaw)
	assert.ErrorIs(t, err, ErrArtifactNotFound)
	assert.Positive(t, result.Deleted)
}

func TestGarbageCollectLeavesOriginsWithoutSafeCheckpointUntouched(t *testing.T) {
	for _, tc := range []struct {
		name       string
		checkpoint []byte
	}{
		{name: "no checkpoint"},
		{name: "corrupt checkpoint", checkpoint: []byte(`{"v":1`)},
		{name: "future checkpoint", checkpoint: []byte(`{"v":2,"origin":"retention-a1b2c3","seq":1,"sessions":{}}\n`)},
		{name: "missing sessions field", checkpoint: []byte(`{"v":1,"origin":"retention-a1b2c3","seq":1}`)},
		{name: "incomplete checkpoint", checkpoint: []byte(`{"v":1,"origin":"retention-a1b2c3","seq":1,"sessions":{"retention-a1b2c3~missing":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}}\n`)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := newRetentionStore(t)
			orphan := createRetentionRef(t, store, Ref{
				Origin: retentionOrigin,
				Kind:   KindRaw,
				Name:   hashHex([]byte("orphan")),
			}, []byte("orphan"))
			if tc.checkpoint != nil {
				createRetentionRef(t, store, Ref{
					Origin: retentionOrigin,
					Kind:   KindCheckpoints,
					Name:   "cp-0000000001.json",
				}, tc.checkpoint)
			}

			result, err := GarbageCollect(t.Context(), GCOptions{
				Store: store,
				Grace: 0,
				Now:   time.Now().Add(time.Hour),
			})
			require.NoError(t, err)
			assert.Equal(t, 1, result.SkippedOrigins)
			_, err = store.Stat(t.Context(), orphan)
			assert.NoError(t, err, "unsafe origins must remain untouched")
		})
	}
}

func TestGarbageCollectRejectsDuplicateCheckpointKeysBeforeTrashing(t *testing.T) {
	t.Run("top-level sessions field", func(t *testing.T) {
		store := newRetentionStore(t)
		body := []byte(`{"v":1,"origin":"retention-a1b2c3","seq":1,"sessions":{},"sessions":{}}`)
		createRetentionRef(t, store, Ref{
			Origin: retentionOrigin, Kind: KindCheckpoints, Name: "cp-0000000001.json",
		}, body)
		orphan := createRetentionRef(t, store, Ref{
			Origin: retentionOrigin, Kind: KindRaw, Name: hashHex([]byte("top-level orphan")),
		}, []byte("top-level orphan"))

		result, err := GarbageCollect(t.Context(), GCOptions{
			Store: store, Now: time.Now().Add(time.Hour),
		})

		require.NoError(t, err)
		assert.Equal(t, 1, result.SkippedOrigins)
		_, err = store.Stat(t.Context(), orphan)
		assert.NoError(t, err)
	})

	t.Run("session id", func(t *testing.T) {
		store := newRetentionStore(t)
		database := testDB(t)
		seedSession(t, database, "duplicate-session", "alpha")
		_, err := ExportToStore(t.Context(), database, store, ExportOptions{
			Origin: retentionOrigin, Full: true,
		})
		require.NoError(t, err)
		latest, found, err := latestGCCheckpoint(t.Context(), store, retentionOrigin)
		require.NoError(t, err)
		require.True(t, found)
		data, err := readGCStoreArtifact(t.Context(), store, latest, checkpointDecodedLimit)
		require.NoError(t, err)
		var cp checkpoint
		require.NoError(t, json.Unmarshal(data, &cp))
		gid := retentionOrigin + "~duplicate-session"
		hash := cp.Sessions[gid]
		body := fmt.Appendf(nil,
			`{"v":1,"origin":%q,"seq":2,"sessions":{%q:%q,%q:%q}}`,
			retentionOrigin, gid, hash, gid, hash)
		createRetentionRef(t, store, Ref{
			Origin: retentionOrigin, Kind: KindCheckpoints, Name: "cp-0000000002.json",
		}, body)
		orphan := createRetentionRef(t, store, Ref{
			Origin: retentionOrigin, Kind: KindRaw, Name: hashHex([]byte("session orphan")),
		}, []byte("session orphan"))

		result, err := GarbageCollect(t.Context(), GCOptions{
			Store: store, Now: time.Now().Add(time.Hour),
		})

		require.NoError(t, err)
		assert.Equal(t, 1, result.SkippedOrigins)
		_, err = store.Stat(t.Context(), orphan)
		assert.NoError(t, err)
	})
}

func TestGarbageCollectCancellationStopsBeforeLaterOrigin(t *testing.T) {
	base := newRetentionStore(t)
	for _, origin := range []string{"cancel-a1b2c3", "cancel-d4e5f6"} {
		createEmptyCheckpoint(t, base, origin, 1)
		createRetentionRef(t, base, Ref{
			Origin: origin, Kind: KindRaw, Name: hashHex([]byte(origin)),
		}, []byte(origin))
	}
	ctx, cancel := context.WithCancel(t.Context())
	store := &cancelAfterTrashStore{ArtifactStore: base, cancel: cancel}

	result, err := GarbageCollect(ctx, GCOptions{
		Store: store,
		Grace: 0,
		Now:   time.Now().Add(time.Hour),
	})
	require.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, 1, result.Deleted)
	assert.Equal(t, 1, store.trashCalls)
}

func TestGarbageCollectExpiresQuarantineThroughLogicalCapability(t *testing.T) {
	_, store := newTestDocbankStore(t, docbank.Config{})
	ref := createRetentionRef(t, store, Ref{
		Origin: retentionOrigin, Kind: KindRaw, Name: hashHex([]byte("quarantine")),
	}, []byte("quarantine"))
	require.NoError(t, store.Quarantine(t.Context(), ref, "invalid protocol body"))
	items, err := firstStoreQuarantinePage(t.Context(), store, 1)
	require.NoError(t, err)
	require.Len(t, items, 1)
	assert.Equal(t, ref, items[0].Ref)

	result, err := GarbageCollect(t.Context(), GCOptions{
		Store:           store,
		QuarantineGrace: time.Hour,
		Now:             items[0].Modified.Add(2 * time.Hour),
	})
	require.NoError(t, err)
	assert.False(t, result.QuarantineSkipped)
	assert.Equal(t, 1, result.QuarantinedScanned)
	assert.Equal(t, 1, result.QuarantinedEligible)
	assert.Equal(t, 1, result.QuarantinedDeleted)
	items, err = firstStoreQuarantinePage(t.Context(), store, 1)
	require.NoError(t, err)
	assert.Empty(t, items)
}

func TestGarbageCollectQuarantineDryRunDoesNotTrash(t *testing.T) {
	_, store := newTestDocbankStore(t, docbank.Config{})
	ref := createRetentionRef(t, store, Ref{
		Origin: retentionOrigin, Kind: KindRaw, Name: hashHex([]byte("dry quarantine")),
	}, []byte("dry quarantine"))
	require.NoError(t, store.Quarantine(t.Context(), ref, "diagnostic"))
	items, err := firstStoreQuarantinePage(t.Context(), store, 1)
	require.NoError(t, err)
	require.Len(t, items, 1)

	result, err := GarbageCollect(t.Context(), GCOptions{
		Store: store, DryRun: true,
		QuarantineGrace: time.Hour, Now: items[0].Modified.Add(2 * time.Hour),
	})

	require.NoError(t, err)
	assert.Equal(t, 1, result.QuarantinedEligible)
	assert.Zero(t, result.QuarantinedDeleted)
	items, err = firstStoreQuarantinePage(t.Context(), store, 1)
	require.NoError(t, err)
	assert.Len(t, items, 1)
}

func TestGarbageCollectCheckpointReachabilityHeapStaysBounded(t *testing.T) {
	small := measureCheckpointReachabilityHeap(t, 10)
	large := measureCheckpointReachabilityHeap(t, 10_000)

	assert.LessOrEqual(t, large, small+int64(32<<20),
		"10,000 checkpoint sessions must stay within the explicit GC memory budget")
}

func TestGarbageCollectKeepsQuarantineInsideGraceAndReportsUnsupportedStore(t *testing.T) {
	t.Run("inside grace", func(t *testing.T) {
		_, store := newTestDocbankStore(t, docbank.Config{})
		ref := createRetentionRef(t, store, Ref{
			Origin: retentionOrigin, Kind: KindRaw, Name: hashHex([]byte("recent")),
		}, []byte("recent"))
		require.NoError(t, store.Quarantine(t.Context(), ref, "recent"))
		items, err := firstStoreQuarantinePage(t.Context(), store, 1)
		require.NoError(t, err)
		require.Len(t, items, 1)

		result, err := GarbageCollect(t.Context(), GCOptions{
			Store:           store,
			QuarantineGrace: time.Hour,
			Now:             items[0].Modified.Add(30 * time.Minute),
		})
		require.NoError(t, err)
		assert.Zero(t, result.QuarantinedEligible)
		assert.Zero(t, result.QuarantinedDeleted)
		items, err = firstStoreQuarantinePage(t.Context(), store, 1)
		require.NoError(t, err)
		assert.Len(t, items, 1)
	})

	t.Run("unsupported", func(t *testing.T) {
		store := struct{ ArtifactStore }{ArtifactStore: newRetentionStore(t)}
		result, err := GarbageCollect(t.Context(), GCOptions{Store: store})
		require.NoError(t, err)
		assert.True(t, result.QuarantineSkipped)
	})
}

type cancelAfterTrashStore struct {
	ArtifactStore
	cancel     context.CancelFunc
	trashCalls int
}

func (s *cancelAfterTrashStore) Trash(ctx context.Context, ref Ref) error {
	s.trashCalls++
	err := s.ArtifactStore.Trash(ctx, ref)
	s.cancel()
	return err
}

func newRetentionFixture(t *testing.T) retentionFixture {
	t.Helper()
	store := newRetentionStore(t)
	database := testDB(t)
	seedSession(t, database, "sess-1", "alpha")
	_, err := ExportToStore(t.Context(), database, store, ExportOptions{
		Origin: retentionOrigin,
		Full:   true,
	})
	require.NoError(t, err)
	firstCheckpoint, found, err := latestGCCheckpoint(t.Context(), store, retentionOrigin)
	require.NoError(t, err)
	require.True(t, found)
	firstCheckpointData, err := readGCStoreArtifact(
		t.Context(), store, firstCheckpoint, checkpointDecodedLimit)
	require.NoError(t, err)
	var first checkpoint
	require.NoError(t, json.Unmarshal(firstCheckpointData, &first))
	firstManifestHash := first.Sessions[retentionOrigin+"~sess-1"]
	require.NotEmpty(t, firstManifestHash)
	firstManifestRef := Ref{
		Origin: retentionOrigin, Kind: KindManifests, Name: firstManifestHash + ".json",
	}
	firstManifestEntry, err := store.Stat(t.Context(), firstManifestRef)
	require.NoError(t, err)
	firstManifestData, err := readGCStoreArtifact(
		t.Context(), store, firstManifestEntry, manifestDecodedLimit)
	require.NoError(t, err)
	var firstManifest manifest
	require.NoError(t, json.Unmarshal(firstManifestData, &firstManifest))
	require.Len(t, firstManifest.Segments, 1)
	firstSegmentRef := Ref{
		Origin: retentionOrigin, Kind: KindSegments,
		Name: firstManifest.Segments[0] + ".ndjson",
	}
	require.NoError(t, database.ReplaceSessionMessages("sess-1", []db.Message{
		{SessionID: "sess-1", Ordinal: 0, Role: "user", Content: "changed", ContentLength: 7},
	}))
	_, err = ExportToStore(t.Context(), database, store, ExportOptions{
		Origin: retentionOrigin,
		Full:   true,
	})
	require.NoError(t, err)
	latestCheckpoint, found, err := latestGCCheckpoint(t.Context(), store, retentionOrigin)
	require.NoError(t, err)
	require.True(t, found)
	latestCheckpointData, err := readGCStoreArtifact(
		t.Context(), store, latestCheckpoint, checkpointDecodedLimit)
	require.NoError(t, err)
	var latest checkpoint
	require.NoError(t, json.Unmarshal(latestCheckpointData, &latest))
	latestManifestHash := latest.Sessions[retentionOrigin+"~sess-1"]
	require.NotEmpty(t, latestManifestHash)
	require.NotEqual(t, firstManifestHash, latestManifestHash)
	latestManifestRef := Ref{
		Origin: retentionOrigin, Kind: KindManifests, Name: latestManifestHash + ".json",
	}
	latestManifestEntry, err := store.Stat(t.Context(), latestManifestRef)
	require.NoError(t, err)
	latestManifestData, err := readGCStoreArtifact(
		t.Context(), store, latestManifestEntry, manifestDecodedLimit)
	require.NoError(t, err)
	var latestManifest manifest
	require.NoError(t, json.Unmarshal(latestManifestData, &latestManifest))
	require.Len(t, latestManifest.Segments, 1)
	latestSegmentRef := Ref{
		Origin: retentionOrigin, Kind: KindSegments,
		Name: latestManifest.Segments[0] + ".ndjson",
	}
	require.NotEqual(t, firstSegmentRef, latestSegmentRef)
	old := map[Kind][]Ref{
		KindCheckpoints: {firstCheckpoint.Ref},
		KindManifests:   {firstManifestRef},
		KindSegments:    {firstSegmentRef},
	}
	live := map[Kind][]Ref{
		KindCheckpoints: {latestCheckpoint.Ref},
		KindManifests:   {latestManifestRef},
		KindSegments:    {latestSegmentRef},
	}

	metadata := metadataEvent{
		Version: formatVersion, HLC: "20260721T120000.000000000Z-000000-retention-a1b2c3",
		Origin: retentionOrigin, SessionGID: retentionOrigin + "~sess-1", Op: MetadataOpPurge,
	}
	metadataBody, err := canonicalJSON(metadata)
	require.NoError(t, err)
	metaHash := hashHex(metadataBody)
	meta := createRetentionRef(t, store, Ref{
		Origin: retentionOrigin, Kind: KindMeta,
		Name: metadata.HLC + "-" + metaHash + ".json",
	}, metadataBody)

	return retentionFixture{
		store: store,
		old:   old,
		live:  live,
		meta:  meta,
	}
}

func newRetentionStore(t *testing.T) ArtifactStore {
	t.Helper()
	_, store := newTestDocbankStore(t, docbank.Config{})
	return store
}

func createRetentionRef(t *testing.T, store ArtifactStore, ref Ref, body []byte) Ref {
	t.Helper()
	identity, err := NewIdentity(hashHex(body), int64(len(body)))
	require.NoError(t, err)
	_, err = store.Create(t.Context(), ref, identity,
		canonicalArtifactMediaType(ref.Kind), bytes.NewReader(body))
	require.NoError(t, err)
	return ref
}

func createEmptyCheckpoint(t *testing.T, store ArtifactStore, origin string, sequence int) Ref {
	t.Helper()
	body, err := canonicalJSON(checkpoint{
		Version: formatVersion, Origin: origin, Sequence: sequence,
		Sessions: map[string]string{},
	})
	require.NoError(t, err)
	return createRetentionRef(t, store, Ref{
		Origin: origin, Kind: KindCheckpoints,
		Name: fmt.Sprintf("cp-%010d.json", sequence),
	}, body)
}

func TestGarbageCollectRejectsInvalidOptions(t *testing.T) {
	_, err := GarbageCollect(t.Context(), GCOptions{})
	assert.Error(t, err)
	_, err = GarbageCollect(t.Context(), GCOptions{
		Store: newRetentionStore(t), Grace: -time.Second,
	})
	assert.Error(t, err)
}

func TestGarbageCollectRejectsCheckpointSequenceMismatch(t *testing.T) {
	store := newRetentionStore(t)
	body, err := json.Marshal(checkpoint{
		Version: formatVersion, Origin: retentionOrigin, Sequence: 2,
		Sessions: map[string]string{},
	})
	require.NoError(t, err)
	createRetentionRef(t, store, Ref{
		Origin: retentionOrigin, Kind: KindCheckpoints, Name: "cp-0000000001.json",
	}, append(body, '\n'))
	orphan := createRetentionRef(t, store, Ref{
		Origin: retentionOrigin, Kind: KindRaw, Name: hashHex([]byte("keep")),
	}, []byte("keep"))

	result, err := GarbageCollect(t.Context(), GCOptions{
		Store: store, Now: time.Now().Add(time.Hour),
	})
	require.NoError(t, err)
	assert.Equal(t, 1, result.SkippedOrigins)
	_, err = store.Stat(t.Context(), orphan)
	assert.NoError(t, err)
}

func TestGarbageCollectPropagatesStoreFailure(t *testing.T) {
	want := errors.New("list failed")
	store := &failingRetentionStore{err: want}
	_, err := GarbageCollect(t.Context(), GCOptions{Store: store})
	assert.ErrorIs(t, err, want)
}

type failingRetentionStore struct {
	ArtifactStore
	err error
}

func (s *failingRetentionStore) Origins(context.Context) (OriginIterator, error) {
	return nil, s.err
}

type checkpointHeapStore struct {
	ArtifactStore
	entry    Entry
	body     []byte
	baseline uint64
	peak     uint64
}

func newCheckpointHeapStore(t *testing.T, sessions int) *checkpointHeapStore {
	t.Helper()
	const manifestHash = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	body := bytes.NewBuffer(make([]byte, 0, sessions*140))
	_, _ = fmt.Fprintf(body, `{"v":1,"origin":%q,"seq":1,"sessions":{`, retentionOrigin)
	for index := range sessions {
		if index > 0 {
			_ = body.WriteByte(',')
		}
		_, _ = fmt.Fprintf(body, `%q:%q`, fmt.Sprintf("%s~session-%05d", retentionOrigin, index), manifestHash)
	}
	_, _ = body.WriteString("}}")
	data := body.Bytes()
	identity, err := NewIdentity(hashHex(data), int64(len(data)))
	require.NoError(t, err)
	return &checkpointHeapStore{
		entry: Entry{
			Ref:      Ref{Origin: retentionOrigin, Kind: KindCheckpoints, Name: "cp-0000000001.json"},
			Identity: identity, Modified: time.Now(),
		},
		body: data,
	}
}

func (s *checkpointHeapStore) Origins(context.Context) (OriginIterator, error) {
	return &testOriginIterator{next: func(context.Context, int) ([]string, error) {
		return []string{retentionOrigin}, io.EOF
	}}, nil
}

func (s *checkpointHeapStore) Entries(
	_ context.Context, _ string, kind Kind,
) (EntryIterator, error) {
	return &testEntryIterator{next: func(context.Context, int) ([]Entry, error) {
		if kind == KindCheckpoints {
			return []Entry{s.entry}, io.EOF
		}
		return nil, io.EOF
	}}, nil
}

func (s *checkpointHeapStore) Open(
	context.Context, Ref,
) (Entry, VerifiedReader, error) {
	return s.entry, &checkpointHeapReader{Reader: bytes.NewReader(s.body)}, nil
}

func (s *checkpointHeapStore) Stat(context.Context, Ref) (Entry, error) {
	runtime.GC()
	var stats runtime.MemStats
	runtime.ReadMemStats(&stats)
	if stats.HeapAlloc > s.peak {
		s.peak = stats.HeapAlloc
	}
	return Entry{}, ErrArtifactNotFound
}

type checkpointHeapReader struct{ *bytes.Reader }

func (r *checkpointHeapReader) Verify() error { return nil }
func (r *checkpointHeapReader) Close() error  { return nil }

func measureCheckpointReachabilityHeap(t *testing.T, sessions int) int64 {
	t.Helper()
	store := newCheckpointHeapStore(t, sessions)
	runtime.GC()
	var baseline runtime.MemStats
	runtime.ReadMemStats(&baseline)
	store.baseline = baseline.HeapAlloc
	result, err := GarbageCollect(t.Context(), GCOptions{Store: store})
	require.NoError(t, err)
	assert.Equal(t, 1, result.SkippedOrigins)
	if store.peak <= store.baseline {
		return 0
	}
	return int64(store.peak - store.baseline)
}
