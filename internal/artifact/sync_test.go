package artifact

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/docbank"
)

func TestEnsureOriginPersists(t *testing.T) {
	database := testDB(t)

	first, err := EnsureOrigin(database)
	require.NoError(t, err)
	require.NotEmpty(t, first)
	require.NotEqual(t, "local", first)

	second, err := EnsureOrigin(database)
	require.NoError(t, err)
	assert.Equal(t, first, second)
}

func TestCheckpointFloorBootstrapsFromLiveAndQuarantinedNodes(t *testing.T) {
	_, store := newTestDocbankStore(t, docbank.Config{})
	database := testDB(t)
	origin := contractOrigin
	for sequence := 1; sequence <= checkpointFloorPageSize+2; sequence++ {
		body := fmt.Appendf(nil, `{"v":1,"origin":%q,"sequence":%d,"sessions":{}}`, origin, sequence)
		createCheckpointBody(t, store, sequence, body)
	}
	quarantinedName := fmt.Sprintf("cp-%010d.json", checkpointFloorPageSize+2)
	require.NoError(t, store.Quarantine(t.Context(),
		requireContractRef(t, origin, KindCheckpoints, quarantinedName),
		"test quarantine"))

	sequence, err := reserveCheckpointSequenceFromStore(
		t.Context(), database, store, origin,
	)
	require.NoError(t, err)
	assert.Equal(t, 131, sequence)

	// A fresh vault may report no sequence after reset or quarantine expiry, but
	// the SQLite floor remains authoritative and may never be lowered.
	_, emptyStore := newTestDocbankStore(t, docbank.Config{})
	sequence, err = reserveCheckpointSequenceFromStore(
		t.Context(), database, emptyStore, origin,
	)
	require.NoError(t, err)
	assert.Equal(t, 132, sequence)

	// If both SQLite and the vault are lost simultaneously, local prevention is
	// impossible; peer common-checkpoint conflict handling is the final backstop.
}

func TestCheckpointFloorTraversesStoreOnlyBeforeBootstrap(t *testing.T) {
	database := testDB(t)
	store := &countingCheckpointFloorStore{floor: 40}

	sequence, err := reserveCheckpointSequenceFromStore(
		t.Context(), database, store, contractOrigin,
	)
	require.NoError(t, err)
	assert.Equal(t, 41, sequence)
	sequence, err = reserveCheckpointSequenceFromStore(
		t.Context(), database, store, contractOrigin,
	)
	require.NoError(t, err)
	assert.Equal(t, 42, sequence)
	assert.Equal(t, 1, store.calls, "durable floor avoids repeated vault traversal")
}

type countingCheckpointFloorStore struct {
	ArtifactStore
	floor int
	calls int
}

func (s *countingCheckpointFloorStore) checkpointFloor(context.Context, string) (int, error) {
	s.calls++
	return s.floor, nil
}

type recordingSyncStateValueReader struct {
	states map[string]string
	keys   []string
	calls  int
}

type countingQueuedExportStore struct {
	database     *db.DB
	queueQueries int
	sessionLoads int
	messageLoads int
	usageLoads   int
}

type countingCanonicalExportDB struct {
	*db.DB
	sessionLoads int
	messageLoads int
	usageLoads   int
}

func (s *countingCanonicalExportDB) GetSessionFull(
	ctx context.Context, id string,
) (*db.Session, error) {
	s.sessionLoads++
	return s.DB.GetSessionFull(ctx, id)
}

func (s *countingCanonicalExportDB) GetAllMessages(
	ctx context.Context, id string,
) ([]db.Message, error) {
	s.messageLoads++
	return s.DB.GetAllMessages(ctx, id)
}

func (s *countingCanonicalExportDB) GetUsageEvents(
	ctx context.Context, id string,
) ([]db.UsageEvent, error) {
	s.usageLoads++
	return s.DB.GetUsageEvents(ctx, id)
}

type recordedArtifactCreate struct {
	Ref      Ref
	Identity Identity
	Created  bool
}

type recordingArtifactStore struct {
	ArtifactStore
	creates []recordedArtifactCreate
}

type checkpointReadCountingStore struct {
	ArtifactStore
	lists int
	opens int
}

type dependencyOpenCountingStore struct {
	ArtifactStore
	opens map[Kind]int
}

func (s *dependencyOpenCountingStore) Open(
	ctx context.Context, ref Ref,
) (Entry, VerifiedReader, error) {
	s.opens[ref.Kind]++
	return s.ArtifactStore.Open(ctx, ref)
}

type catalogCheckpointStore struct {
	ArtifactStore
	entry     Entry
	statCalls int
	openCalls int
}

type checkpointStatOverrideStore struct {
	ArtifactStore
	identity *Identity
	err      error
}

type originTraversalGateStore struct {
	ArtifactStore
	processedFirstPage bool
}

func (s *originTraversalGateStore) Origins(context.Context) (OriginIterator, error) {
	page := 0
	return &testOriginIterator{next: func(context.Context, int) ([]string, error) {
		page++
		if page == 1 {
			origins := make([]string, artifactImportPageSize)
			for i := range origins {
				origins[i] = fmt.Sprintf("peer-%06x", i+1)
			}
			return origins, nil
		}
		if !s.processedFirstPage {
			return nil, errors.New("second origin page requested before first page was processed")
		}
		return []string{"peer-ffffff"}, io.EOF
	}}, nil
}

func (s *originTraversalGateStore) Entries(
	ctx context.Context, origin string, kind Kind,
) (EntryIterator, error) {
	s.processedFirstPage = true
	return s.ArtifactStore.Entries(ctx, origin, kind)
}

type checkpointHeaderFirstStore struct {
	ArtifactStore
	listedAll bool
}

func (s *checkpointHeaderFirstStore) Entries(
	ctx context.Context, origin string, kind Kind,
) (EntryIterator, error) {
	iterator, err := s.ArtifactStore.Entries(ctx, origin, kind)
	if err != nil || kind != KindCheckpoints {
		return iterator, err
	}
	return &testEntryIterator{
		next: func(ctx context.Context, limit int) ([]Entry, error) {
			entries, err := iterator.Next(ctx, limit)
			if errors.Is(err, io.EOF) {
				s.listedAll = true
			}
			return entries, err
		},
		close: iterator.Close,
	}, nil
}

func (s *checkpointHeaderFirstStore) Open(
	ctx context.Context, ref Ref,
) (Entry, VerifiedReader, error) {
	if ref.Kind == KindCheckpoints && !s.listedAll {
		return Entry{}, nil, errors.New("checkpoint body opened before all headers were enumerated")
	}
	return s.ArtifactStore.Open(ctx, ref)
}

type cancelAfterMetadataOpenStore struct {
	ArtifactStore
	cancel context.CancelFunc
	opens  int
}

type exactImportCountingStore struct {
	ArtifactStore
	origins atomic.Int32
	entries atomic.Int32
	opens   atomic.Int32
}

type exactImportGateStore struct {
	ArtifactStore
	entered chan struct{}
	release chan struct{}
	once    sync.Once
	active  atomic.Int32
	maximum atomic.Int32
	opens   atomic.Int32
}

func (s *exactImportGateStore) Open(
	ctx context.Context, ref Ref,
) (Entry, VerifiedReader, error) {
	active := s.active.Add(1)
	defer s.active.Add(-1)
	for {
		maximum := s.maximum.Load()
		if active <= maximum || s.maximum.CompareAndSwap(maximum, active) {
			break
		}
	}
	s.opens.Add(1)
	blocked := false
	s.once.Do(func() {
		blocked = true
		close(s.entered)
	})
	if blocked {
		select {
		case <-ctx.Done():
			return Entry{}, nil, ctx.Err()
		case <-s.release:
		}
	}
	return s.ArtifactStore.Open(ctx, ref)
}

func (s *exactImportCountingStore) Origins(ctx context.Context) (OriginIterator, error) {
	s.origins.Add(1)
	return s.ArtifactStore.Origins(ctx)
}

func (s *exactImportCountingStore) Entries(
	ctx context.Context, origin string, kind Kind,
) (EntryIterator, error) {
	s.entries.Add(1)
	return s.ArtifactStore.Entries(ctx, origin, kind)
}

func (s *exactImportCountingStore) Open(
	ctx context.Context, ref Ref,
) (Entry, VerifiedReader, error) {
	s.opens.Add(1)
	return s.ArtifactStore.Open(ctx, ref)
}

type trackingRepairStore struct {
	ArtifactStore
	repairs int
}

type failingImportScheduler struct {
	err   error
	calls int
}

func (s *failingImportScheduler) RecordChanged(context.Context, Entry) error {
	s.calls++
	return s.err
}

type transientImportStore struct {
	ArtifactStore
	calls atomic.Int32
}

func (s *transientImportStore) Open(
	ctx context.Context, ref Ref,
) (Entry, VerifiedReader, error) {
	if s.calls.Add(1) == 1 {
		return Entry{}, nil, syscall.EAGAIN
	}
	return s.ArtifactStore.Open(ctx, ref)
}

func (s *trackingRepairStore) RepairContent(
	context.Context, Identity, io.Reader,
) error {
	s.repairs++
	return nil
}

func (s *cancelAfterMetadataOpenStore) Open(
	_ context.Context, ref Ref,
) (Entry, VerifiedReader, error) {
	entry, reader, err := s.ArtifactStore.Open(context.Background(), ref)
	if err == nil && ref.Kind == KindMeta {
		s.opens++
		if s.opens == 1 {
			s.cancel()
		}
	}
	return entry, reader, err
}

func (s *checkpointStatOverrideStore) Stat(ctx context.Context, ref Ref) (Entry, error) {
	if ref.Kind == Kind(KindCheckpoints) {
		if s.err != nil {
			return Entry{}, s.err
		}
		if s.identity != nil {
			return Entry{Ref: ref, Identity: *s.identity}, nil
		}
	}
	return s.ArtifactStore.Stat(ctx, ref)
}

func (s *catalogCheckpointStore) Stat(context.Context, Ref) (Entry, error) {
	s.statCalls++
	return s.entry, nil
}

func (s *catalogCheckpointStore) Open(
	context.Context, Ref,
) (Entry, VerifiedReader, error) {
	s.openCalls++
	return Entry{}, nil, errors.New("checkpoint body must not be opened")
}

func (s *checkpointReadCountingStore) Entries(
	ctx context.Context, origin string, kind Kind,
) (EntryIterator, error) {
	if kind == Kind(KindCheckpoints) {
		s.lists++
	}
	return s.ArtifactStore.Entries(ctx, origin, kind)
}

func (s *checkpointReadCountingStore) Open(
	ctx context.Context, ref Ref,
) (Entry, VerifiedReader, error) {
	if ref.Kind == Kind(KindCheckpoints) {
		s.opens++
	}
	return s.ArtifactStore.Open(ctx, ref)
}

func (s *recordingArtifactStore) Create(
	ctx context.Context,
	ref Ref,
	identity Identity,
	mediaType string,
	body io.Reader,
) (CreateResult, error) {
	result, err := s.ArtifactStore.Create(ctx, ref, identity, mediaType, body)
	if err == nil {
		s.creates = append(s.creates, recordedArtifactCreate{
			Ref: ref, Identity: identity, Created: result.Created,
		})
	}
	return result, err
}

func TestExportToStorePublishesDependenciesBeforeCheckpointAndSkipsUnchanged(t *testing.T) {
	database := testDB(t)
	seedSession(t, database, "sess-1", "alpha")
	filesystem, err := newProtocolTestStore(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, filesystem.Close()) })
	store := &recordingArtifactStore{ArtifactStore: filesystem}

	result, err := ExportToStore(t.Context(), database, store, ExportOptions{
		Origin: contractOrigin,
	})
	require.NoError(t, err)
	assert.Equal(t, 1, result.ExportedSessions)
	assert.True(t, result.CheckpointCreated)
	require.Len(t, store.creates, 3)
	assert.Equal(t, Kind(KindSegments), store.creates[0].Ref.Kind)
	assert.Equal(t, Kind(KindManifests), store.creates[1].Ref.Kind)
	assert.Equal(t, Kind(KindCheckpoints), store.creates[2].Ref.Kind)
	assert.True(t, store.creates[0].Created)
	assert.True(t, store.creates[1].Created)
	assert.True(t, store.creates[2].Created)
	checkpointBytes := readContractArtifact(t, store, store.creates[2].Ref)
	var published checkpoint
	require.NoError(t, json.Unmarshal(checkpointBytes, &published))
	canonicalCheckpoint, err := canonicalJSON(published)
	require.NoError(t, err)
	assert.Equal(t, canonicalCheckpoint, checkpointBytes,
		"streaming checkpoint encoding must remain byte-compatible")

	store.creates = nil
	result, err = ExportToStore(t.Context(), database, store, ExportOptions{
		Origin: contractOrigin,
		Full:   true,
	})
	require.NoError(t, err)
	assert.Zero(t, result.ExportedSessions)
	assert.False(t, result.CheckpointCreated)
	require.Len(t, store.creates, 2,
		"full export verifies immutable dependencies without minting a version")
	assert.Equal(t, Kind(KindSegments), store.creates[0].Ref.Kind)
	assert.Equal(t, Kind(KindManifests), store.creates[1].Ref.Kind)
	assert.False(t, store.creates[0].Created)
	assert.False(t, store.creates[1].Created)
}

func TestExportToStoreFullRepairsMissingDependencyWithoutNewCheckpoint(t *testing.T) {
	database := testDB(t)
	seedSession(t, database, "sess-1", "alpha")
	filesystem, err := newProtocolTestStore(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, filesystem.Close()) })
	first, err := ExportToStore(t.Context(), database, filesystem, ExportOptions{
		Origin: contractOrigin,
	})
	require.NoError(t, err)
	require.True(t, first.CheckpointCreated)
	segments, err := firstStoreEntryPage(t.Context(), filesystem, contractOrigin, KindSegments, 10)
	require.NoError(t, err)
	require.Len(t, segments.Items, 1)
	require.NoError(t, filesystem.Trash(t.Context(), segments.Items[0].Ref))
	_, err = filesystem.Stat(t.Context(), segments.Items[0].Ref)
	require.ErrorIs(t, err, ErrArtifactNotFound)
	headBefore, ok, err := database.GetArtifactCheckpointHead(t.Context(), contractOrigin)
	require.NoError(t, err)
	require.True(t, ok)

	result, err := ExportToStore(t.Context(), database, filesystem, ExportOptions{
		Origin: contractOrigin, Full: true,
	})
	require.NoError(t, err)
	assert.False(t, result.CheckpointCreated)
	_, err = filesystem.Stat(t.Context(), segments.Items[0].Ref)
	require.NoError(t, err, "full export recreates a missing dependency even when its manifest survives")
	headAfter, ok, err := database.GetArtifactCheckpointHead(t.Context(), contractOrigin)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, headBefore, headAfter)
}

func TestExportToStoreRecreatesMissingRecordedCheckpointAfterVaultReset(t *testing.T) {
	database := testDB(t)
	seedSession(t, database, "sess-1", "alpha")
	first, err := newProtocolTestStore(t.TempDir())
	require.NoError(t, err)
	_, err = ExportToStore(t.Context(), database, first, ExportOptions{
		Origin: contractOrigin,
	})
	require.NoError(t, err)
	require.NoError(t, first.Close())

	replacement, err := newProtocolTestStore(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, replacement.Close()) })
	result, err := ExportToStore(t.Context(), database, replacement, ExportOptions{
		Origin: contractOrigin,
		Full:   true,
	})
	require.NoError(t, err)
	assert.True(t, result.CheckpointCreated)
	assert.Equal(t, 1, result.CheckpointSequence,
		"vault reset recreates the recorded immutable checkpoint without consuming a new sequence")
	checkpointRef := requireContractRef(t, contractOrigin, KindCheckpoints,
		"cp-0000000001.json")
	checkpointBytes := readContractArtifact(t, replacement, checkpointRef)
	var published checkpoint
	require.NoError(t, json.Unmarshal(checkpointBytes, &published))
	assert.Equal(t, map[string]string{
		contractOrigin + "~sess-1": published.Sessions[contractOrigin+"~sess-1"],
	}, published.Sessions)
}

func TestExportToStoreChangedBatchDoesNotScanCheckpointHistory(t *testing.T) {
	database := testDB(t)
	seedSession(t, database, "sess-1", "alpha")
	filesystem, err := newProtocolTestStore(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, filesystem.Close()) })
	store := &checkpointReadCountingStore{ArtifactStore: filesystem}
	_, err = ExportToStore(t.Context(), database, store, ExportOptions{
		Origin: contractOrigin,
	})
	require.NoError(t, err)
	store.lists = 0
	store.opens = 0
	require.NoError(t, database.ReplaceSessionMessages("sess-1", []db.Message{{
		SessionID: "sess-1", Ordinal: 0, Role: "user", Content: "changed",
	}}))

	result, err := ExportToStore(t.Context(), database, store, ExportOptions{
		Origin: contractOrigin,
	})
	require.NoError(t, err)
	assert.True(t, result.CheckpointCreated)
	assert.Equal(t, 2, result.CheckpointSequence)
	assert.Zero(t, store.lists,
		"a recorded pre-apply head avoids checkpoint-history traversal")
	assert.Zero(t, store.opens,
		"normal changed export does not read an old checkpoint body")
}

func TestExportToStoreUnchangedCheckpointUsesCatalogIdentityOnly(t *testing.T) {
	for _, size := range []int64{128, 64 << 20} {
		t.Run(fmt.Sprintf("size-%d", size), func(t *testing.T) {
			database := testDB(t)
			ref := requireContractRef(t, contractOrigin, KindCheckpoints, "cp-0000000001.json")
			identity := Identity{SHA256: strings64("a"), Size: size}
			require.NoError(t, database.RecordArtifactCheckpointHead(t.Context(), db.ArtifactCheckpointHead{
				Origin: contractOrigin, Sequence: 1,
				SessionMapSHA256: strings64("b"), CheckpointSHA256: identity.SHA256,
				CheckpointSize: identity.Size,
			}, nil))
			store := &catalogCheckpointStore{entry: Entry{Ref: ref, Identity: identity}}

			result, err := ExportToStore(t.Context(), database, store, ExportOptions{
				Origin: contractOrigin,
			})
			require.NoError(t, err)
			assert.False(t, result.CheckpointCreated)
			assert.Equal(t, 1, store.statCalls)
			assert.Zero(t, store.openCalls,
				"unchanged periodic export cannot drain the checkpoint body")
		})
	}
}

func TestExportToStoreRecordedCheckpointStatRecovery(t *testing.T) {
	database := testDB(t)
	seedSession(t, database, "sess-1", "alpha")
	filesystem, err := newProtocolTestStore(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, filesystem.Close()) })
	_, err = ExportToStore(t.Context(), database, filesystem, ExportOptions{
		Origin: contractOrigin,
	})
	require.NoError(t, err)

	operationalFailure := errors.New("injected checkpoint stat failure")
	_, err = ExportToStore(t.Context(), database, &checkpointStatOverrideStore{
		ArtifactStore: filesystem, err: operationalFailure,
	}, ExportOptions{Origin: contractOrigin})
	require.ErrorIs(t, err, operationalFailure)

	mismatch := Identity{SHA256: strings64("c"), Size: 1}
	result, err := ExportToStore(t.Context(), database, &checkpointStatOverrideStore{
		ArtifactStore: filesystem, identity: &mismatch,
	}, ExportOptions{Origin: contractOrigin})
	require.NoError(t, err)
	assert.True(t, result.CheckpointCreated)
	assert.Equal(t, 1, result.CheckpointSequence,
		"catalog identity mismatch quarantines and reconstructs the recorded checkpoint")
}

type maxReadReader struct {
	reader io.Reader
	max    int
}

func (r *maxReadReader) Read(p []byte) (int, error) {
	if len(p) > r.max {
		r.max = len(p)
	}
	return r.reader.Read(p)
}

func TestExportCheckpointBootstrapStreamsLargeSessionMap(t *testing.T) {
	sessions := make(map[string]string, 2000)
	for i := range 2000 {
		sessions[fmt.Sprintf("%s~session-%04d", contractOrigin, i)] = strings64("a")
	}
	body, err := canonicalJSON(checkpoint{
		Version: formatVersion, Origin: contractOrigin, Sequence: 42, Sessions: sessions,
	})
	require.NoError(t, err)
	reader := &maxReadReader{reader: strings.NewReader(string(body))}
	head, err := decodeCanonicalCheckpointHead(reader, contractOrigin,
		"cp-0000000042.json", identityForBytes(t, body))
	require.NoError(t, err)
	mapBytes, err := canonicalJSON(sessions)
	require.NoError(t, err)
	assert.Equal(t, hashHex(mapBytes), head.SessionMapSHA256)
	assert.Less(t, reader.max, len(body)/4,
		"bootstrap must tokenize the checkpoint instead of reading its full body")
}

type hiddenCheckpointHeadDB struct {
	*db.DB
}

func (s *hiddenCheckpointHeadDB) GetArtifactCheckpointHead(
	context.Context, string,
) (db.ArtifactCheckpointHead, bool, error) {
	return db.ArtifactCheckpointHead{}, false, nil
}

func TestExportToStoreBootstrapsMissingDatabaseHeadFromLatestCheckpoint(t *testing.T) {
	database := testDB(t)
	seedSession(t, database, "sess-1", "alpha")
	filesystem, err := newProtocolTestStore(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, filesystem.Close()) })
	_, err = ExportToStore(t.Context(), database, filesystem, ExportOptions{
		Origin: contractOrigin,
	})
	require.NoError(t, err)
	store := &checkpointReadCountingStore{ArtifactStore: filesystem}

	result, err := ExportToStore(t.Context(), &hiddenCheckpointHeadDB{DB: database},
		store, ExportOptions{Origin: contractOrigin})
	require.NoError(t, err)
	assert.False(t, result.CheckpointCreated)
	assert.Equal(t, 1, result.CheckpointSequence)
	assert.Positive(t, store.lists)
	assert.Equal(t, 1, store.opens)
	page, err := firstStoreEntryPage(t.Context(), filesystem, contractOrigin, KindCheckpoints, 10)
	require.NoError(t, err)
	assert.Len(t, page.Items, 1)
}

type closeErrorVerifiedReader struct {
	VerifiedReader
	err error
}

func (r *closeErrorVerifiedReader) Close() error {
	return errors.Join(r.VerifiedReader.Close(), r.err)
}

type checkpointCloseErrorStore struct {
	ArtifactStore
	err error
}

type checkpointOpenFailureStore struct {
	ArtifactStore
	err error
}

func (s *checkpointOpenFailureStore) Open(
	ctx context.Context, ref Ref,
) (Entry, VerifiedReader, error) {
	if ref.Kind == Kind(KindCheckpoints) {
		return Entry{}, nil, s.err
	}
	return s.ArtifactStore.Open(ctx, ref)
}

func TestExportCheckpointBootstrapPropagatesOperationalOpenErrors(t *testing.T) {
	for _, test := range []struct {
		name string
		err  error
	}{
		{name: "canceled", err: context.Canceled},
		{name: "unavailable", err: errors.New("checkpoint store unavailable")},
	} {
		t.Run(test.name, func(t *testing.T) {
			database := testDB(t)
			seedSession(t, database, "sess-1", "alpha")
			filesystem, err := newProtocolTestStore(t.TempDir())
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, filesystem.Close()) })
			_, err = ExportToStore(t.Context(), database, filesystem, ExportOptions{
				Origin: contractOrigin,
			})
			require.NoError(t, err)

			_, err = ExportToStore(t.Context(), &hiddenCheckpointHeadDB{DB: database},
				&checkpointOpenFailureStore{ArtifactStore: filesystem, err: test.err},
				ExportOptions{Origin: contractOrigin})
			require.ErrorIs(t, err, test.err)
		})
	}
}

func TestLatestValidCheckpointFallsBackPastSemanticCandidateAndDefersFuture(t *testing.T) {
	filesystem, err := newProtocolTestStore(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, filesystem.Close()) })
	for _, candidate := range []struct {
		name string
		body string
	}{
		{name: "cp-0000000001.json", body: `{"origin":"contract-a1b2c3","seq":1,"sessions":{},"v":1}` + "\n"},
		{name: "cp-0000000002.json", body: `{"origin":"contract-a1b2c3","seq":99,"sessions":{},"v":1}` + "\n"},
	} {
		body := []byte(candidate.body)
		ref := requireContractRef(t, contractOrigin, KindCheckpoints, candidate.name)
		_, err := filesystem.Create(t.Context(), ref, identityForBytes(t, body),
			canonicalArtifactMediaType(KindCheckpoints), strings.NewReader(candidate.body))
		require.NoError(t, err)
	}

	head, ok, err := latestValidCheckpointHead(t.Context(), filesystem, contractOrigin)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, 1, head.Sequence)

	future := []byte(`{"origin":"contract-a1b2c3","seq":3,"sessions":{},"v":2}` + "\n")
	ref := requireContractRef(t, contractOrigin, KindCheckpoints, "cp-0000000003.json")
	_, err = filesystem.Create(t.Context(), ref, identityForBytes(t, future),
		canonicalArtifactMediaType(KindCheckpoints), strings.NewReader(string(future)))
	require.NoError(t, err)
	_, _, err = latestValidCheckpointHead(t.Context(), filesystem, contractOrigin)
	require.ErrorIs(t, err, errFutureArtifactVersion)
}

func TestExportSpoolConstructionJoinsCleanupFailures(t *testing.T) {
	chmodFailure := errors.New("injected spool setup failure")
	cleanupFailure := errors.New("injected spool cleanup failure")
	previousChmod := exportSpoolChmod
	previousCleanup := exportSpoolCleanup
	t.Cleanup(func() {
		exportSpoolChmod = previousChmod
		exportSpoolCleanup = previousCleanup
	})
	exportSpoolChmod = func(*os.File) error { return chmodFailure }
	exportSpoolCleanup = func(file *os.File) error {
		return errors.Join(closeAndRemoveExportSpool(file), cleanupFailure)
	}

	_, _, _, err := spoolArtifactPublicationMap(
		t.Context(), testDB(t), contractOrigin,
	)
	require.ErrorIs(t, err, chmodFailure)
	require.ErrorIs(t, err, cleanupFailure)

	mapSpool, err := os.CreateTemp("", "agentsview-artifact-test-map-*")
	require.NoError(t, err)
	t.Cleanup(func() { _ = closeAndRemoveExportSpool(mapSpool) })
	_, err = io.WriteString(mapSpool, "{}\n")
	require.NoError(t, err)
	_, _, err = spoolArtifactCheckpoint(t.Context(), mapSpool, contractOrigin, 1)
	require.ErrorIs(t, err, chmodFailure)
	require.ErrorIs(t, err, cleanupFailure)
}

func (s *checkpointCloseErrorStore) Open(
	ctx context.Context, ref Ref,
) (Entry, VerifiedReader, error) {
	entry, reader, err := s.ArtifactStore.Open(ctx, ref)
	if err != nil || ref.Kind != Kind(KindCheckpoints) {
		return entry, reader, err
	}
	return entry, &closeErrorVerifiedReader{VerifiedReader: reader, err: s.err}, nil
}

func TestExportToStorePropagatesCheckpointCloseErrorWithoutAcknowledging(t *testing.T) {
	database := testDB(t)
	seedSession(t, database, "sess-1", "alpha")
	filesystem, err := newProtocolTestStore(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, filesystem.Close()) })
	_, err = ExportToStore(t.Context(), database, filesystem, ExportOptions{
		Origin: contractOrigin,
	})
	require.NoError(t, err)
	require.NoError(t, database.ReplaceSessionMessages("sess-1", []db.Message{{
		SessionID: "sess-1", Ordinal: 0, Role: "user", Content: "changed",
	}}))
	closeFailure := errors.New("injected checkpoint close failure")

	_, err = ExportToStore(t.Context(), &hiddenCheckpointHeadDB{DB: database}, &checkpointCloseErrorStore{
		ArtifactStore: filesystem, err: closeFailure,
	}, ExportOptions{Origin: contractOrigin})
	require.ErrorIs(t, err, closeFailure)
	pending, err := database.PendingArtifactExports(t.Context(), 10)
	require.NoError(t, err)
	assert.Len(t, pending, 1)
}

func TestExportToStoreCancellationLeavesQueuePending(t *testing.T) {
	database := testDB(t)
	seedSession(t, database, "sess-1", "alpha")
	filesystem, err := newProtocolTestStore(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, filesystem.Close()) })
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, err = ExportToStore(ctx, database, filesystem, ExportOptions{
		Origin: contractOrigin,
	})
	require.ErrorIs(t, err, context.Canceled)
	pending, err := database.PendingArtifactExports(t.Context(), 10)
	require.NoError(t, err)
	assert.Len(t, pending, 1)
}

type failingArtifactStore struct {
	ArtifactStore
	failKind Kind
	failErr  error
	failed   bool
	calls    []Kind
}

func (s *failingArtifactStore) Create(
	ctx context.Context,
	ref Ref,
	identity Identity,
	mediaType string,
	body io.Reader,
) (CreateResult, error) {
	s.calls = append(s.calls, ref.Kind)
	if !s.failed && ref.Kind == s.failKind {
		s.failed = true
		return CreateResult{}, s.failErr
	}
	return s.ArtifactStore.Create(ctx, ref, identity, mediaType, body)
}

func TestExportToStoreFailureKeepsClaimAndCheckpointLast(t *testing.T) {
	failure := errors.New("injected artifact create failure")
	tests := []struct {
		name         string
		failKind     Kind
		wantCalls    []Kind
		wantRetrySeq int
	}{
		{name: "dependency", failKind: Kind(KindSegments),
			wantCalls: []Kind{Kind(KindSegments)}, wantRetrySeq: 1},
		{name: "manifest", failKind: Kind(KindManifests),
			wantCalls: []Kind{Kind(KindSegments), Kind(KindManifests)}, wantRetrySeq: 1},
		{name: "checkpoint", failKind: Kind(KindCheckpoints),
			wantCalls: []Kind{Kind(KindSegments), Kind(KindManifests), Kind(KindCheckpoints)}, wantRetrySeq: 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			database := testDB(t)
			seedSession(t, database, "sess-1", "alpha")
			filesystem, err := newProtocolTestStore(t.TempDir())
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, filesystem.Close()) })
			store := &failingArtifactStore{
				ArtifactStore: filesystem, failKind: tt.failKind, failErr: failure,
			}

			_, err = ExportToStore(t.Context(), database, store, ExportOptions{
				Origin: contractOrigin,
			})
			require.ErrorIs(t, err, failure)
			assert.Equal(t, tt.wantCalls, store.calls)
			pending, err := database.PendingArtifactExports(t.Context(), 10)
			require.NoError(t, err)
			require.Len(t, pending, 1, "failed export must retain its exact claim")
			page, err := firstStoreEntryPage(t.Context(), store, contractOrigin, KindCheckpoints, 10)
			require.NoError(t, err)
			assert.Empty(t, page.Items, "checkpoint cannot precede a failed dependency")

			store.calls = nil
			result, err := ExportToStore(t.Context(), database, store, ExportOptions{
				Origin: contractOrigin,
			})
			require.NoError(t, err)
			assert.True(t, result.CheckpointCreated)
			assert.Equal(t, tt.wantRetrySeq, result.CheckpointSequence)
			pending, err = database.PendingArtifactExports(t.Context(), 10)
			require.NoError(t, err)
			assert.Empty(t, pending)
		})
	}
}

type staleClaimExportDB struct {
	*db.DB
	once sync.Once
}

func (s *staleClaimExportDB) ApplyArtifactPublicationChanges(
	ctx context.Context,
	origin string,
	changes []db.ArtifactPublicationChange,
) (int64, bool, error) {
	s.once.Do(func() {
		_ = s.ReplaceSessionMessages("sess-1", []db.Message{{
			SessionID: "sess-1", Ordinal: 0, Role: "user", Content: "newer",
		}})
	})
	return s.DB.ApplyArtifactPublicationChanges(ctx, origin, changes)
}

func TestExportToStoreStaleGenerationDoesNotPublishOrAcknowledge(t *testing.T) {
	database := testDB(t)
	seedSession(t, database, "sess-1", "alpha")
	stale := &staleClaimExportDB{DB: database}
	filesystem, err := newProtocolTestStore(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, filesystem.Close()) })

	_, err = ExportToStore(t.Context(), stale, filesystem, ExportOptions{
		Origin: contractOrigin,
	})
	require.ErrorIs(t, err, db.ErrArtifactExportClaimStale)
	_, ok, err := database.GetArtifactCheckpointHead(t.Context(), contractOrigin)
	require.NoError(t, err)
	assert.False(t, ok)
	pending, err := database.PendingArtifactExports(t.Context(), 10)
	require.NoError(t, err)
	require.Len(t, pending, 1)
	assert.Greater(t, pending[0].Generation, int64(1))
	page, err := firstStoreEntryPage(t.Context(), filesystem, contractOrigin, KindCheckpoints, 10)
	require.NoError(t, err)
	assert.Empty(t, page.Items)
}

type mutateAfterCheckpointStore struct {
	ArtifactStore
	database *db.DB
	once     sync.Once
}

func (s *mutateAfterCheckpointStore) Create(
	ctx context.Context,
	ref Ref,
	identity Identity,
	mediaType string,
	body io.Reader,
) (CreateResult, error) {
	result, err := s.ArtifactStore.Create(ctx, ref, identity, mediaType, body)
	if err == nil && ref.Kind == Kind(KindCheckpoints) {
		s.once.Do(func() {
			_ = s.database.ReplaceSessionMessages("sess-1", []db.Message{{
				SessionID: "sess-1", Ordinal: 0, Role: "user", Content: "newest",
			}})
		})
	}
	return result, err
}

func TestExportToStoreStaleGenerationAfterCheckpointDoesNotAdvanceHead(t *testing.T) {
	database := testDB(t)
	seedSession(t, database, "sess-1", "alpha")
	filesystem, err := newProtocolTestStore(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, filesystem.Close()) })
	store := &mutateAfterCheckpointStore{ArtifactStore: filesystem, database: database}

	_, err = ExportToStore(t.Context(), database, store, ExportOptions{
		Origin: contractOrigin,
	})
	require.ErrorIs(t, err, db.ErrArtifactExportClaimStale)
	_, ok, err := database.GetArtifactCheckpointHead(t.Context(), contractOrigin)
	require.NoError(t, err)
	assert.False(t, ok)
	pending, err := database.PendingArtifactExports(t.Context(), 10)
	require.NoError(t, err)
	require.Len(t, pending, 1)
	firstPage, err := firstStoreEntryPage(t.Context(), filesystem, contractOrigin, KindCheckpoints, 10)
	require.NoError(t, err)
	require.Len(t, firstPage.Items, 1,
		"the immutable stale checkpoint is harmless while no head references it")

	result, err := ExportToStore(t.Context(), database, store, ExportOptions{
		Origin: contractOrigin,
	})
	require.NoError(t, err)
	assert.Equal(t, 2, result.CheckpointSequence)
	head, ok, err := database.GetArtifactCheckpointHead(t.Context(), contractOrigin)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, 2, head.Sequence)
	pending, err = database.PendingArtifactExports(t.Context(), 10)
	require.NoError(t, err)
	assert.Empty(t, pending)
}

func TestExportToStorePublicationRevisionRejectsPhysicallyCreatedStaleCheckpoint(t *testing.T) {
	path := filepath.Join(t.TempDir(), "archive.db")
	first, err := db.Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, first.Close()) })
	second, err := db.Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, second.Close()) })
	filesystem, err := newProtocolTestStore(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, filesystem.Close()) })
	ctx := t.Context()
	seedSession(t, first, "session-a", "project")
	claimA, err := first.ArtifactExportClaims(ctx, []string{"session-a"})
	require.NoError(t, err)
	require.Len(t, claimA, 1)
	sessionA, err := first.GetSessionFull(ctx, "session-a")
	require.NoError(t, err)
	messagesA, err := first.GetAllMessages(ctx, "session-a")
	require.NoError(t, err)
	usageA, err := first.GetUsageEvents(ctx, "session-a")
	require.NoError(t, err)
	manifestA, _, err := exportLoadedSessionToStore(
		ctx, filesystem, contractOrigin, sessionA, messagesA, usageA, productionArtifactLimits(),
	)
	require.NoError(t, err)
	revisionA, changed, err := first.ApplyArtifactPublicationChanges(ctx, contractOrigin,
		[]db.ArtifactPublicationChange{{
			SessionID: "session-a", Generation: claimA[0].Generation,
			ManifestHash: manifestA, SourceFingerprint: manifestA,
		}})
	require.NoError(t, err)
	require.True(t, changed)
	mapA, digestA, snapshotA, err := spoolArtifactPublicationMap(ctx, first, contractOrigin)
	require.NoError(t, err)
	require.Equal(t, revisionA, snapshotA)
	t.Cleanup(func() { _ = closeAndRemoveExportSpool(mapA) })

	seedSession(t, second, "session-b", "project")
	claimB, err := second.ArtifactExportClaims(ctx, []string{"session-b"})
	require.NoError(t, err)
	require.Len(t, claimB, 1)
	sessionB, err := second.GetSessionFull(ctx, "session-b")
	require.NoError(t, err)
	messagesB, err := second.GetAllMessages(ctx, "session-b")
	require.NoError(t, err)
	usageB, err := second.GetUsageEvents(ctx, "session-b")
	require.NoError(t, err)
	manifestB, _, err := exportLoadedSessionToStore(
		ctx, filesystem, contractOrigin, sessionB, messagesB, usageB, productionArtifactLimits(),
	)
	require.NoError(t, err)
	revisionB, changed, err := second.ApplyArtifactPublicationChanges(ctx, contractOrigin,
		[]db.ArtifactPublicationChange{{
			SessionID: "session-b", Generation: claimB[0].Generation,
			ManifestHash: manifestB, SourceFingerprint: manifestB,
		}})
	require.NoError(t, err)
	require.True(t, changed)
	mapB, digestB, snapshotB, err := spoolArtifactPublicationMap(ctx, second, contractOrigin)
	require.NoError(t, err)
	require.Equal(t, revisionB, snapshotB)
	sequenceB, err := reserveCheckpointSequenceFromStore(ctx, second, filesystem, contractOrigin)
	require.NoError(t, err)
	checkpointB, identityB, err := spoolArtifactCheckpoint(ctx, mapB, contractOrigin, sequenceB)
	require.NoError(t, err)
	refB := requireContractRef(t, contractOrigin, KindCheckpoints,
		fmt.Sprintf("cp-%010d.json", sequenceB))
	_, err = filesystem.Create(ctx, refB, identityB,
		canonicalArtifactMediaType(KindCheckpoints), checkpointB)
	require.NoError(t, err)
	require.NoError(t, closeAndRemoveExportSpool(mapB))
	require.NoError(t, closeAndRemoveExportSpool(checkpointB))
	require.NoError(t, second.RecordArtifactCheckpointHead(ctx, db.ArtifactCheckpointHead{
		Origin: contractOrigin, Sequence: sequenceB, PublicationRevision: snapshotB,
		SessionMapSHA256: digestB, CheckpointSHA256: identityB.SHA256,
		CheckpointSize: identityB.Size,
	}, claimB))

	sequenceA, err := reserveCheckpointSequenceFromStore(ctx, first, filesystem, contractOrigin)
	require.NoError(t, err)
	require.Greater(t, sequenceA, sequenceB)
	checkpointA, identityA, err := spoolArtifactCheckpoint(ctx, mapA, contractOrigin, sequenceA)
	require.NoError(t, err)
	refA := requireContractRef(t, contractOrigin, KindCheckpoints,
		fmt.Sprintf("cp-%010d.json", sequenceA))
	_, err = filesystem.Create(ctx, refA, identityA,
		canonicalArtifactMediaType(KindCheckpoints), checkpointA)
	require.NoError(t, err)
	require.NoError(t, closeAndRemoveExportSpool(checkpointA))
	err = first.RecordArtifactCheckpointHead(ctx, db.ArtifactCheckpointHead{
		Origin: contractOrigin, Sequence: sequenceA, PublicationRevision: snapshotA,
		SessionMapSHA256: digestA, CheckpointSHA256: identityA.SHA256,
		CheckpointSize: identityA.Size,
	}, claimA)
	require.ErrorIs(t, err, db.ErrArtifactExportClaimStale)
	_, err = filesystem.Stat(ctx, refA)
	require.NoError(t, err, "the stale immutable checkpoint may exist without becoming authoritative")
	head, ok, err := first.GetArtifactCheckpointHead(ctx, contractOrigin)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, sequenceB, head.Sequence)
	pending, err := first.ArtifactExportClaims(ctx, []string{"session-a"})
	require.NoError(t, err)
	require.Equal(t, claimA, pending)

	result, err := ExportToStore(ctx, first, filesystem, ExportOptions{Origin: contractOrigin})
	require.NoError(t, err)
	assert.False(t, result.CheckpointCreated)
	pending, err = first.ArtifactExportClaims(ctx, []string{"session-a"})
	require.NoError(t, err)
	assert.Empty(t, pending)
}

func (s *countingQueuedExportStore) PendingArtifactExports(
	ctx context.Context, limit int,
) ([]db.ArtifactExportQueueItem, error) {
	s.queueQueries++
	return s.database.PendingArtifactExports(ctx, limit)
}

func (s *countingQueuedExportStore) GetSessionFull(
	ctx context.Context, id string,
) (*db.Session, error) {
	s.sessionLoads++
	return s.database.GetSessionFull(ctx, id)
}

func (s *countingQueuedExportStore) GetAllMessages(
	ctx context.Context, id string,
) ([]db.Message, error) {
	s.messageLoads++
	return s.database.GetAllMessages(ctx, id)
}

func (s *countingQueuedExportStore) GetUsageEvents(
	ctx context.Context, id string,
) ([]db.UsageEvent, error) {
	s.usageLoads++
	return s.database.GetUsageEvents(ctx, id)
}

func TestArtifactExportCardinalityLoadsOnlyDirtyBatch(t *testing.T) {
	for _, archiveSize := range []int{20, 2000} {
		t.Run(fmt.Sprintf("archive-%d", archiveSize), func(t *testing.T) {
			database := testDB(t)
			for i := range archiveSize {
				require.NoError(t, database.UpsertSession(db.Session{
					ID: fmt.Sprintf("peer-%04d", i), Project: "project",
					Machine: "peer-a1b2c3", Agent: "claude",
				}))
			}
			require.NoError(t, database.UpsertSession(db.Session{
				ID: "dirty", Project: "project", Machine: "local", Agent: "claude",
			}))
			require.NoError(t, database.ReplaceSessionMessages("dirty", []db.Message{{
				SessionID: "dirty", Ordinal: 0, Role: "user", Content: "changed",
			}}))
			require.NoError(t, database.ReplaceSessionUsageEvents("dirty", []db.UsageEvent{{
				SessionID: "dirty", Source: "event", Model: "model", DedupKey: "one",
			}}))

			store := &countingQueuedExportStore{database: database}
			visited := 0
			err := forEachQueuedArtifactExport(t.Context(), store, 1, func(work queuedArtifactExport) error {
				visited++
				assert.Equal(t, "dirty", work.Item.SessionID)
				require.NotNil(t, work.Session)
				assert.Len(t, work.Messages, 1)
				assert.Len(t, work.UsageEvents, 1)
				return nil
			})
			require.NoError(t, err)
			assert.Equal(t, 1, visited)
			assert.Equal(t, 1, store.queueQueries)
			assert.Equal(t, 1, store.sessionLoads)
			assert.Equal(t, 1, store.messageLoads)
			assert.Equal(t, 1, store.usageLoads)
		})
	}
}

func TestExportToStoreCardinalityIgnoresUnrelatedArchiveBodies(t *testing.T) {
	for _, archiveSize := range []int{20, 2000} {
		t.Run(fmt.Sprintf("archive-%d", archiveSize), func(t *testing.T) {
			database := testDB(t)
			for i := range archiveSize {
				require.NoError(t, database.UpsertSession(db.Session{
					ID: fmt.Sprintf("peer-%04d", i), Project: "project",
					Machine: "peer-a1b2c3", Agent: "claude",
				}))
			}
			seedSession(t, database, "dirty", "project")
			counted := &countingCanonicalExportDB{DB: database}
			filesystem, err := newProtocolTestStore(t.TempDir())
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, filesystem.Close()) })

			result, err := ExportToStore(t.Context(), counted, filesystem, ExportOptions{
				Origin: contractOrigin,
			})
			require.NoError(t, err)
			assert.Equal(t, 1, result.ExportedSessions)
			assert.Equal(t, 1, counted.sessionLoads)
			assert.Equal(t, 1, counted.messageLoads)
			assert.Equal(t, 1, counted.usageLoads)
		})
	}
}

func TestExportToStoreIncrementalBatchIsBoundedAndFullStreamsAllBodies(t *testing.T) {
	database := testDB(t)
	const total = artifactExportBatchSize + 5
	for i := range total {
		seedSession(t, database, fmt.Sprintf("session-%03d", i), "project")
	}
	filesystem, err := newProtocolTestStore(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, filesystem.Close()) })

	result, err := ExportToStore(t.Context(), database, filesystem, ExportOptions{
		Origin: contractOrigin,
	})
	require.NoError(t, err)
	assert.Equal(t, artifactExportBatchSize, result.ExportedSessions)
	pending, err := database.PendingArtifactExports(t.Context(), 1024)
	require.NoError(t, err)
	assert.Len(t, pending, 5)
	firstBytes := readContractArtifact(t, filesystem,
		requireContractRef(t, contractOrigin, KindCheckpoints, "cp-0000000001.json"))
	var first checkpoint
	require.NoError(t, json.Unmarshal(firstBytes, &first))
	assert.Len(t, first.Sessions, artifactExportBatchSize)

	result, err = ExportToStore(t.Context(), database, filesystem, ExportOptions{
		Origin: contractOrigin,
		Full:   true,
	})
	require.NoError(t, err)
	assert.Equal(t, 5, result.ExportedSessions,
		"full pass idempotently recreates clean dependencies and creates only missing manifests")
	pending, err = database.PendingArtifactExports(t.Context(), 1024)
	require.NoError(t, err)
	assert.Empty(t, pending)
	secondBytes := readContractArtifact(t, filesystem,
		requireContractRef(t, contractOrigin, KindCheckpoints, "cp-0000000002.json"))
	var second checkpoint
	require.NoError(t, json.Unmarshal(secondBytes, &second))
	assert.Len(t, second.Sessions, total)
}

func TestExportToStoreFullDrainsMoreThanOneClaimPage(t *testing.T) {
	database := testDB(t)
	const total = 1025
	for i := range total {
		require.NoError(t, database.UpsertSession(db.Session{
			ID: fmt.Sprintf("session-%04d", i), Project: "project",
			Machine: "local", Agent: "claude", CreatedAt: "2026-06-14T01:02:03Z",
		}))
	}
	counted := &countingCanonicalExportDB{DB: database}
	filesystem, err := newProtocolTestStore(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, filesystem.Close()) })

	result, err := ExportToStore(t.Context(), counted, filesystem, ExportOptions{
		Origin: contractOrigin, Full: true,
	})
	require.NoError(t, err)
	assert.Equal(t, total, result.ExportedSessions)
	assert.Equal(t, total, counted.sessionLoads,
		"full export loads each body once instead of reloading the archive per page")
	pending, err := database.PendingArtifactExports(t.Context(), 1024)
	require.NoError(t, err)
	assert.Empty(t, pending)
	head, ok, err := database.GetArtifactCheckpointHead(t.Context(), contractOrigin)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, 2, head.Sequence)
	body := readContractArtifact(t, filesystem, requireContractRef(
		t, contractOrigin, KindCheckpoints, "cp-0000000002.json",
	))
	var published checkpoint
	require.NoError(t, json.Unmarshal(body, &published))
	assert.Len(t, published.Sessions, total)
}

func TestExportToStoreExplicitSessionIDsClaimBeyondOldestQueuePage(t *testing.T) {
	database := testDB(t)
	const total = 1025
	for i := range total {
		require.NoError(t, database.UpsertSession(db.Session{
			ID: fmt.Sprintf("session-%04d", i), Project: "project",
			Machine: "local", Agent: "claude",
		}))
	}
	filesystem, err := newProtocolTestStore(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, filesystem.Close()) })

	result, err := ExportToStore(t.Context(), database, filesystem, ExportOptions{
		Origin: contractOrigin, SessionIDs: []string{"session-1024"},
	})
	require.NoError(t, err)
	assert.Equal(t, 1, result.ExportedSessions)
	pending, err := database.PendingArtifactExports(t.Context(), 1024)
	require.NoError(t, err)
	assert.Len(t, pending, 1024)
	body := readContractArtifact(t, filesystem,
		requireContractRef(t, contractOrigin, KindCheckpoints, "cp-0000000001.json"))
	var published checkpoint
	require.NoError(t, json.Unmarshal(body, &published))
	assert.Equal(t, map[string]string{
		contractOrigin + "~session-1024": published.Sessions[contractOrigin+"~session-1024"],
	}, published.Sessions)
}

func TestExportToStorePublishesEmptyAndDeletionSets(t *testing.T) {
	t.Run("empty", func(t *testing.T) {
		database := testDB(t)
		filesystem, err := newProtocolTestStore(t.TempDir())
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, filesystem.Close()) })

		result, err := ExportToStore(t.Context(), database, filesystem, ExportOptions{
			Origin: contractOrigin, Full: true,
		})
		require.NoError(t, err)
		assert.True(t, result.CheckpointCreated)
		body := readContractArtifact(t, filesystem,
			requireContractRef(t, contractOrigin, KindCheckpoints, "cp-0000000001.json"))
		assert.Equal(t,
			`{"origin":"contract-a1b2c3","seq":1,"sessions":{},"v":1}`+"\n",
			string(body))
	})

	t.Run("deletion", func(t *testing.T) {
		database := testDB(t)
		seedSession(t, database, "sess-1", "project")
		filesystem, err := newProtocolTestStore(t.TempDir())
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, filesystem.Close()) })
		_, err = ExportToStore(t.Context(), database, filesystem, ExportOptions{
			Origin: contractOrigin,
		})
		require.NoError(t, err)
		require.NoError(t, database.SoftDeleteSession("sess-1"))

		result, err := ExportToStore(t.Context(), database, filesystem, ExportOptions{
			Origin: contractOrigin,
		})
		require.NoError(t, err)
		assert.True(t, result.CheckpointCreated)
		body := readContractArtifact(t, filesystem,
			requireContractRef(t, contractOrigin, KindCheckpoints, "cp-0000000002.json"))
		assert.Contains(t, string(body), `"sessions":{}`)
	})
}

func TestExactImportDefersCheckpointWithAbsentDependency(t *testing.T) {
	origin := "peer-a1b2c3"
	source := testDB(t)
	seedSession(t, source, "session-1", "alpha")
	store, err := newProtocolTestStore(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	_, err = ExportToStore(t.Context(), source, store, ExportOptions{
		Origin: origin,
		Full:   true,
	})
	require.NoError(t, err)

	segments, err := firstStoreEntryPage(t.Context(), store, origin, KindSegments, 10)
	require.NoError(t, err)
	require.Len(t, segments.Items, 1)
	require.NoError(t, store.Trash(t.Context(), segments.Items[0].Ref))

	target := testDB(t)
	result, err := importResultFromTestStore(
		t.Context(), target, store, "local-d4e5f6",
	)
	require.NoError(t, err)
	assert.Equal(t, 1, result.Deferred)
	assert.False(t, result.Changed())
	landed, landedMap, ok, err := target.GetArtifactCheckpointLanding(t.Context(), origin)
	require.NoError(t, err)
	assert.False(t, ok)
	assert.Empty(t, landed)
	assert.Empty(t, landedMap)
}

func TestImportCoordinatorRetriesExactCheckpointAfterRestartAndDependencyArrival(t *testing.T) {
	origin := "peer-a1b2c3"
	source := testDB(t)
	seedSession(t, source, "session-1", "alpha")
	store, err := newProtocolTestStore(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	_, err = ExportToStore(t.Context(), source, store, ExportOptions{
		Origin: origin,
		Full:   true,
	})
	require.NoError(t, err)

	segments, err := firstStoreEntryPage(t.Context(), store, origin, KindSegments, 10)
	require.NoError(t, err)
	require.Len(t, segments.Items, 1)
	segment := segments.Items[0]
	segmentBody := readContractArtifact(t, store, segment.Ref)
	require.NoError(t, store.Trash(t.Context(), segment.Ref))
	checkpoints, err := firstStoreEntryPage(t.Context(), store, origin, KindCheckpoints, 10)
	require.NoError(t, err)
	require.Len(t, checkpoints.Items, 1)

	databasePath := filepath.Join(t.TempDir(), "target.db")
	target, err := db.Open(databasePath)
	require.NoError(t, err)
	t.Cleanup(func() {
		if target != nil {
			require.NoError(t, target.Close())
		}
	})
	coordinator := NewStoreImportCoordinator(target, store, "local-d4e5f6")
	require.NoError(t, coordinator.RecordChanged(t.Context(), checkpoints.Items[0]))
	result, err := coordinator.Finalize(t.Context())
	require.NoError(t, err)
	assert.Equal(t, 1, result.Deferred)
	assert.False(t, result.Changed())

	require.NoError(t, target.Close())
	target = nil
	target, err = db.Open(databasePath)
	require.NoError(t, err)
	pending, err := target.PendingArtifactImports(t.Context(), formatVersion, 10)
	require.NoError(t, err)
	require.Len(t, pending, 1, "unfinished checkpoint work must survive restart")

	created, err := store.Create(
		t.Context(), segment.Ref, segment.Identity, "application/x-ndjson",
		bytes.NewReader(segmentBody),
	)
	require.NoError(t, err)
	coordinator = NewStoreImportCoordinator(target, store, "local-d4e5f6")
	require.NoError(t, coordinator.RecordChanged(t.Context(), created.Entry))
	result, err = coordinator.Finalize(t.Context())
	require.NoError(t, err)
	assert.Equal(t, 1, result.Sessions)
	assert.Equal(t, 2, result.Messages)
	assert.Zero(t, result.Deferred)
	pending, err = target.PendingArtifactImports(t.Context(), formatVersion, 10)
	require.NoError(t, err)
	assert.Empty(t, pending)
	landed, err := target.GetSession(t.Context(), origin+"~session-1")
	require.NoError(t, err)
	require.NotNil(t, landed)
}

type corruptUntilRepairedStore struct {
	ArtifactStore
	corruptRef  Ref
	repaired    bool
	originLists int
}

func (s *corruptUntilRepairedStore) Origins(ctx context.Context) (OriginIterator, error) {
	s.originLists++
	return s.ArtifactStore.Origins(ctx)
}

func (s *corruptUntilRepairedStore) Open(
	ctx context.Context, ref Ref,
) (Entry, VerifiedReader, error) {
	if ref == s.corruptRef && !s.repaired {
		entry, err := s.Stat(ctx, ref)
		if err != nil {
			return Entry{}, nil, err
		}
		return entry, &alwaysCorruptVerifiedReader{}, nil
	}
	return s.ArtifactStore.Open(ctx, ref)
}

func (s *corruptUntilRepairedStore) RepairContent(
	ctx context.Context, identity Identity, trusted io.Reader,
) error {
	repairer, ok := s.ArtifactStore.(interface {
		RepairContent(context.Context, Identity, io.Reader) error
	})
	if !ok {
		return errors.New("wrapped store cannot repair content")
	}
	if err := repairer.RepairContent(ctx, identity, trusted); err != nil {
		return err
	}
	s.repaired = true
	return nil
}

type alwaysCorruptVerifiedReader struct{}

func (*alwaysCorruptVerifiedReader) Read([]byte) (int, error) {
	return 0, ErrArtifactCorrupt
}

func (*alwaysCorruptVerifiedReader) Verify() error { return ErrArtifactCorrupt }

func (*alwaysCorruptVerifiedReader) Close() error { return nil }

func TestExactImportRepairsPhysicalCorruptionFromTrustedPeer(t *testing.T) {
	origin := "peer-a1b2c3"
	source := testDB(t)
	seedSession(t, source, "session-1", "alpha")
	_, canonical := newTestDocbankStore(t, docbank.Config{})
	_, err := ExportToStore(t.Context(), source, canonical, ExportOptions{
		Origin: origin,
		Full:   true,
	})
	require.NoError(t, err)
	segments, err := firstStoreEntryPage(t.Context(), canonical, origin, KindSegments, 10)
	require.NoError(t, err)
	require.Len(t, segments.Items, 1)
	segment := segments.Items[0]
	_, trustedReader, err := canonical.Open(t.Context(), segment.Ref)
	require.NoError(t, err)
	trusted, err := io.ReadAll(trustedReader)
	require.NoError(t, err)
	require.NoError(t, trustedReader.Verify())
	require.NoError(t, trustedReader.Close())

	store := &corruptUntilRepairedStore{
		ArtifactStore: canonical,
		corruptRef:    segment.Ref,
	}
	target := testDB(t)
	_, err = importResultFromTestStore(t.Context(), target, store, "local-d4e5f6")
	require.ErrorIs(t, err, ErrArtifactCorrupt)
	pending, err := target.PendingArtifactRepairs(t.Context(), 10)
	require.NoError(t, err)
	require.Len(t, pending, 1)
	assert.Equal(t, db.ArtifactRepair{
		Origin: origin,
		Kind:   string(KindSegments),
		Name:   segment.Ref.Name,
		SHA256: segment.Identity.SHA256,
		Size:   segment.Identity.Size,
	}, db.ArtifactRepair{
		Origin: pending[0].Origin,
		Kind:   pending[0].Kind,
		Name:   pending[0].Name,
		SHA256: pending[0].SHA256,
		Size:   pending[0].Size,
	})

	coordinator := NewStoreImportCoordinator(target, store, "local-d4e5f6")
	require.NoError(t, RepairArtifactFromTrustedPeer(
		t.Context(), target, store, pending[0], bytes.NewReader(trusted), coordinator,
	))
	pending, err = target.PendingArtifactRepairs(t.Context(), 10)
	require.NoError(t, err)
	assert.Empty(t, pending)
	coordinator.requestDrain()

	result, err := coordinator.Finalize(t.Context())
	require.NoError(t, err)
	assert.Equal(t, 1, result.Sessions)
	assert.Equal(t, 2, result.Messages)
	assert.Equal(t, 1, store.originLists,
		"repair retry must open the queued checkpoint without another origin scan")

	result, err = coordinator.Finalize(t.Context())
	require.NoError(t, err)
	assert.False(t, result.Changed())
	assert.Equal(t, 1, store.originLists, "a drained coordinator must not run another import")
}

func TestImportCoordinatorIgnoresLandedHistory(t *testing.T) {
	for _, historySize := range []int{10, 10_000} {
		t.Run(fmt.Sprintf("history-%d", historySize), func(t *testing.T) {
			base, err := newProtocolTestStore(t.TempDir())
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, base.Close()) })
			store := &exactImportCountingStore{ArtifactStore: base}
			coordinator := NewStoreImportCoordinator(
				testDB(t), store, "local-d4e5f6",
			)
			require.NoError(t, coordinator.requestDrain())

			result, err := coordinator.Finalize(t.Context())
			require.NoError(t, err)
			assert.False(t, result.Changed())
			assert.Zero(t, store.origins.Load())
			assert.Zero(t, store.entries.Load())
			assert.Zero(t, store.opens.Load())
		})
	}
}

func TestImportCoordinatorProcessesChangedMetadata(t *testing.T) {
	for _, historySize := range []int{10, 10_000} {
		t.Run(fmt.Sprintf("history-%d", historySize), func(t *testing.T) {
			origin := "peer-a1b2c3"
			localOrigin := "local-d4e5f6"
			database := testDB(t)
			for i := range historySize {
				require.NoError(t, database.MarkMetadataEventApplied(
					t.Context(), origin, fmt.Sprintf("history-%010d", i), strings.Repeat("a", 64),
				))
			}
			gid := origin + "~session-1"
			require.NoError(t, database.UpsertSession(db.Session{
				ID: gid, Project: "project", Machine: origin, Agent: "claude",
			}))
			base, err := newProtocolTestStore(t.TempDir())
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, base.Close()) })
			ref := createStoreMetadataEvent(t, base, origin, replayRenameEvent(
				t, origin, gid, replayTestHLC(0, 0), "renamed",
			))
			entry, err := base.Stat(t.Context(), ref)
			require.NoError(t, err)
			store := &exactImportCountingStore{ArtifactStore: base}
			coordinator := NewStoreImportCoordinator(database, store, localOrigin)
			require.NoError(t, coordinator.RecordChanged(t.Context(), entry))

			result, err := coordinator.Finalize(t.Context())
			require.NoError(t, err)
			assert.Equal(t, 1, result.Metadata)
			assert.Zero(t, store.origins.Load())
			assert.Zero(t, store.entries.Load())
			assert.Equal(t, int32(1), store.opens.Load())
			updated, err := database.GetSession(t.Context(), gid)
			require.NoError(t, err)
			require.NotNil(t, updated)
			require.NotNil(t, updated.DisplayName)
			assert.Equal(t, "renamed", *updated.DisplayName)
		})
	}
}

func TestStoreImportCoordinatorSerializesFinalizersAndRetainsMidRunWork(t *testing.T) {
	origin := "peer-a1b2c3"
	localOrigin := "local-d4e5f6"
	database := testDB(t)
	gid := origin + "~session-1"
	require.NoError(t, database.UpsertSession(db.Session{
		ID: gid, Project: "project", Machine: origin, Agent: "claude",
	}))
	filesystem, err := newProtocolTestStore(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, filesystem.Close()) })
	ref := createStoreMetadataEvent(t, filesystem, origin, replayRenameEvent(
		t, origin, gid, replayTestHLC(0, 0), "serialized",
	))
	entry, err := filesystem.Stat(t.Context(), ref)
	require.NoError(t, err)
	store := &exactImportGateStore{
		ArtifactStore: filesystem,
		entered:       make(chan struct{}),
		release:       make(chan struct{}),
	}
	coordinator := NewStoreImportCoordinator(database, store, localOrigin)
	require.NoError(t, coordinator.RecordChanged(t.Context(), entry))

	firstDone := make(chan error, 1)
	go func() {
		_, err := coordinator.Finalize(t.Context())
		firstDone <- err
	}()
	<-store.entered
	coordinator.requestDrain()
	secondStarted := make(chan struct{})
	secondDone := make(chan error, 1)
	go func() {
		close(secondStarted)
		_, err := coordinator.Finalize(t.Context())
		secondDone <- err
	}()
	<-secondStarted
	assert.Never(t, func() bool {
		return store.maximum.Load() > 1
	}, 100*time.Millisecond, time.Millisecond,
		"finalizers must not overlap exact artifact reads")
	close(store.release)
	require.NoError(t, <-firstDone)
	require.NoError(t, <-secondDone)
	assert.Equal(t, int32(1), store.maximum.Load())
	assert.Equal(t, int32(1), store.opens.Load(),
		"the mid-run generation drains after the first exact claim is acknowledged")
}

func TestStoreImportCoordinatorRetriesTransientFinalizeOnceAndDrains(t *testing.T) {
	origin := "peer-a1b2c3"
	localOrigin := "local-d4e5f6"
	database := testDB(t)
	gid := origin + "~session-1"
	require.NoError(t, database.UpsertSession(db.Session{
		ID: gid, Project: "project", Machine: origin, Agent: "claude",
	}))
	filesystem, err := newProtocolTestStore(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, filesystem.Close()) })
	ref := createStoreMetadataEvent(t, filesystem, origin, replayRenameEvent(
		t, origin, gid, replayTestHLC(0, 0), "retry",
	))
	entry, err := filesystem.Stat(t.Context(), ref)
	require.NoError(t, err)
	store := &transientImportStore{ArtifactStore: filesystem}
	coordinator := NewStoreImportCoordinator(database, store, localOrigin)
	require.NoError(t, coordinator.RecordChanged(t.Context(), entry))

	_, err = coordinator.Finalize(t.Context())
	require.ErrorIs(t, err, syscall.EAGAIN)
	result, err := coordinator.Finalize(t.Context())
	require.NoError(t, err)
	assert.Equal(t, 1, result.Metadata)
	assert.Equal(t, int32(2), store.calls.Load(),
		"one failed exact open and one successful retry are expected")

	result, err = coordinator.Finalize(t.Context())
	require.NoError(t, err)
	assert.False(t, result.Changed())
	assert.Equal(t, int32(2), store.calls.Load(), "successful retry must drain pending work")
}

func TestRepairArtifactFromTrustedPeerRejectsTypedNilRetryCoordinator(t *testing.T) {
	database := testDB(t)
	repair := db.ArtifactRepair{
		Origin: "peer-a1b2c3",
		Kind:   string(KindRaw),
		Name:   strings.Repeat("a", 64),
		SHA256: strings.Repeat("a", 64),
		Size:   1,
	}
	require.NoError(t, database.EnqueueArtifactRepair(t.Context(), repair))
	store := &trackingRepairStore{}
	var retry *StoreImportCoordinator

	err := RepairArtifactFromTrustedPeer(
		t.Context(), database, store, repair, strings.NewReader("x"), retry,
	)
	require.Error(t, err)
	assert.Zero(t, store.repairs, "invalid retry coordination must fail before repair side effects")
	pending, pendingErr := database.PendingArtifactRepairs(t.Context(), 10)
	require.NoError(t, pendingErr)
	assert.Len(t, pending, 1, "an unobservable repair must not acknowledge its durable claim")
}

func TestRepairArtifactFromTrustedPeerLeavesClaimPendingWhenSchedulingFails(t *testing.T) {
	database := testDB(t)
	repair := db.ArtifactRepair{
		Origin: "peer-a1b2c3",
		Kind:   string(KindRaw),
		Name:   strings.Repeat("a", 64),
		SHA256: strings.Repeat("a", 64),
		Size:   1,
	}
	require.NoError(t, database.EnqueueArtifactRepair(t.Context(), repair))
	store := &trackingRepairStore{}
	scheduleErr := errors.New("retry scheduler unavailable")
	scheduler := &failingImportScheduler{err: scheduleErr}

	err := RepairArtifactFromTrustedPeer(
		t.Context(), database, store, repair, strings.NewReader("x"), scheduler,
	)
	assert.ErrorIs(t, err, scheduleErr)
	assert.Equal(t, 1, store.repairs, "physical repair completes before retry scheduling")
	assert.Equal(t, 1, scheduler.calls)
	pending, pendingErr := database.PendingArtifactRepairs(t.Context(), 10)
	require.NoError(t, pendingErr)
	assert.Len(t, pending, 1, "failed scheduling must prevent durable claim acknowledgement")
}

func TestExactImportReplaysMetadataAfterSessionContent(t *testing.T) {
	origin := "peer-a1b2c3"
	source := testDB(t)
	seedSession(t, source, "session-1", "alpha")
	store, err := newProtocolTestStore(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	_, err = ExportToStore(t.Context(), source, store, ExportOptions{
		Origin: origin,
		Full:   true,
	})
	require.NoError(t, err)
	recorder := NewMetadataRecorder(source, MetadataRecorderOptions{
		Store:  store,
		Origin: origin,
		Now:    fixedHLCTime,
	})
	_, err = recorder.Append(t.Context(), MetadataEventInput{
		SessionID: "session-1",
		Op:        MetadataOpRename,
		Value:     json.RawMessage(`{"display_name":"Renamed by peer"}`),
	})
	require.NoError(t, err)

	target := testDB(t)
	result, err := importResultFromTestStore(
		t.Context(), target, store, "local-d4e5f6",
	)
	require.NoError(t, err)
	assert.Equal(t, 1, result.Sessions)
	assert.Equal(t, 1, result.Metadata)
	imported, err := target.GetSessionFull(t.Context(), origin+"~session-1")
	require.NoError(t, err)
	require.NotNil(t, imported)
	require.NotNil(t, imported.DisplayName)
	assert.Equal(t, "Renamed by peer", *imported.DisplayName)
}

func TestCheckpointLandingStatusUsesExactRecordedCheckpointMap(t *testing.T) {
	origin := "peer-a1b2c3"
	store := newTestArtifactStore(t)
	source := testDB(t)
	seedSession(t, source, "alpha", "project")
	seedSession(t, source, "bravo", "project")
	_, err := ExportToStore(t.Context(), source, store, ExportOptions{Origin: origin, Full: true})
	require.NoError(t, err)
	_, cp, err := latestStoreCheckpointSummary(t.Context(), store, origin)
	require.NoError(t, err)
	require.NotNil(t, cp)

	target := testDB(t)
	for gid, manifestHash := range cp.Sessions {
		require.NoError(t, target.SetSyncState(importStateKey(origin, gid), manifestHash))
	}
	status, err := CheckpointLandingStatusFromStore(t.Context(), store, origin, target, false)
	require.NoError(t, err)
	require.True(t, status.Found)
	assert.Zero(t, status.LandedSessionCount, "legacy sync state is not exact landing provenance")

	require.NoError(t, target.RecordArtifactCheckpointLanding(
		t.Context(),
		db.ArtifactCheckpointLanding{Origin: origin, Sequence: cp.Sequence},
		cp.Sessions,
	))

	status, err = CheckpointLandingStatusFromStore(t.Context(), store, origin, target, false)
	require.NoError(t, err)
	require.True(t, status.Found)
	assert.Equal(t, cp.Sequence, status.Sequence)
	assert.Equal(t, len(cp.Sessions), status.LandedSessionCount)
}

func TestCheckpointLandingStatusUsesExactLocalPublicationMap(t *testing.T) {
	origin := "local-a1b2c3"
	store := newTestArtifactStore(t)
	database := testDB(t)
	seedSession(t, database, "alpha", "project")
	seedSession(t, database, "bravo", "project")
	_, err := ExportToStore(t.Context(), database, store, ExportOptions{Origin: origin, Full: true})
	require.NoError(t, err)

	status, err := CheckpointLandingStatusFromStore(t.Context(), store, origin, database, true)
	require.NoError(t, err)
	require.True(t, status.Found)
	assert.Equal(t, 2, status.LandedSessionCount)
}

type legacyCheckpointStatusStore struct {
	values map[string]string
}

func (s *legacyCheckpointStatusStore) SyncStateValues(keys []string) (map[string]string, error) {
	result := make(map[string]string, len(keys))
	for _, key := range keys {
		result[key] = s.values[key]
	}
	return result, nil
}

type publicationRevisionRaceStore struct {
	*db.DB
}

func (s *publicationRevisionRaceStore) StreamArtifactPublications(
	ctx context.Context, origin string, visit func(db.ArtifactPublication) error,
) (int64, error) {
	revision, err := s.DB.StreamArtifactPublications(ctx, origin, visit)
	return revision + 1, err
}

type streamingLandingStatusStore struct {
	*db.DB
	streamed int
}

func (s *streamingLandingStatusStore) GetArtifactCheckpointLanding(
	context.Context, string,
) (db.ArtifactCheckpointLanding, map[string]string, bool, error) {
	return db.ArtifactCheckpointLanding{}, nil, false,
		errors.New("checkpoint status must not materialize the landing map")
}

func (s *streamingLandingStatusStore) StreamArtifactCheckpointLanding(
	ctx context.Context, origin string, visit func(string, string) error,
) (db.ArtifactCheckpointLanding, bool, error) {
	landing, manifests, found, err := s.DB.GetArtifactCheckpointLanding(ctx, origin)
	if err != nil || !found {
		return landing, found, err
	}
	keys := make([]string, 0, len(manifests))
	for gid := range manifests {
		keys = append(keys, gid)
	}
	sort.Strings(keys)
	for _, gid := range keys {
		if err := visit(gid, manifests[gid]); err != nil {
			return db.ArtifactCheckpointLanding{}, false, err
		}
		s.streamed++
	}
	return landing, true, nil
}

func TestCheckpointLandingStatusDoesNotFallbackToLegacySyncState(t *testing.T) {
	origin := "peer-a1b2c3"
	store := newTestArtifactStore(t)
	source := testDB(t)
	seedSession(t, source, "alpha", "project")
	_, err := ExportToStore(t.Context(), source, store, ExportOptions{Origin: origin, Full: true})
	require.NoError(t, err)
	_, cp, err := latestStoreCheckpointSummary(t.Context(), store, origin)
	require.NoError(t, err)
	require.NotNil(t, cp)
	legacy := &legacyCheckpointStatusStore{values: map[string]string{}}
	for gid, manifestHash := range cp.Sessions {
		legacy.values[importStateKey(origin, gid)] = manifestHash
	}

	status, err := CheckpointLandingStatusFromStore(t.Context(), store, origin, legacy, false)
	require.NoError(t, err)
	assert.Zero(t, status.LandedSessionCount)
}

func TestCheckpointLandingStatusRejectsMixedPublicationRevisions(t *testing.T) {
	origin := "local-a1b2c3"
	store := newTestArtifactStore(t)
	database := testDB(t)
	seedSession(t, database, "alpha", "project")
	_, err := ExportToStore(t.Context(), database, store, ExportOptions{Origin: origin, Full: true})
	require.NoError(t, err)

	status, err := CheckpointLandingStatusFromStore(
		t.Context(), store, origin, &publicationRevisionRaceStore{DB: database}, true,
	)
	require.NoError(t, err)
	assert.Zero(t, status.LandedSessionCount,
		"rows from a different publication revision are not coherent landing evidence")
}

func TestCheckpointLandingStatusStreamsLargeForeignLanding(t *testing.T) {
	origin := "peer-a1b2c3"
	artifactStore := newTestArtifactStore(t)
	manifests := make(map[string]string, artifactImportPageSize*3)
	for i := range artifactImportPageSize * 3 {
		manifests[fmt.Sprintf("%s~session-%04d", origin, i)] = fmt.Sprintf("%064x", i+1)
	}
	cp := checkpoint{
		Version: formatVersion, Origin: origin, Sequence: 1, Sessions: manifests,
	}
	data, err := canonicalJSON(cp)
	require.NoError(t, err)
	ref, err := NewRef(origin, KindCheckpoints, "cp-0000000001.json")
	require.NoError(t, err)
	createContractArtifact(t, artifactStore, ref, data)
	database := testDB(t)
	require.NoError(t, database.RecordArtifactCheckpointLanding(
		t.Context(), db.ArtifactCheckpointLanding{Origin: origin, Sequence: 1}, manifests,
	))
	store := &streamingLandingStatusStore{DB: database}

	status, err := CheckpointLandingStatusFromStore(t.Context(), artifactStore, origin, store, false)
	require.NoError(t, err)
	assert.Equal(t, len(manifests), status.LandedSessionCount)
	assert.Equal(t, len(manifests), store.streamed)
}

func TestCheckpointLandingStatusHonorsCanceledRequestContext(t *testing.T) {
	origin := "peer-a1b2c3"
	store := newTestArtifactStore(t)
	source := testDB(t)
	seedSession(t, source, "alpha", "project")
	_, err := ExportToStore(t.Context(), source, store, ExportOptions{Origin: origin, Full: true})
	require.NoError(t, err)
	target := testDB(t)
	_, cp, err := latestStoreCheckpointSummary(t.Context(), store, origin)
	require.NoError(t, err)
	require.NotNil(t, cp)
	require.NoError(t, target.RecordArtifactCheckpointLanding(
		t.Context(), db.ArtifactCheckpointLanding{Origin: origin, Sequence: cp.Sequence}, cp.Sessions,
	))
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, err = CheckpointLandingStatusFromStore(ctx, store, origin, target, false)
	require.ErrorIs(t, err, context.Canceled)
}

func TestExactImportQuarantinesInvalidManifestAndSegment(t *testing.T) {
	tests := []struct {
		name       string
		invalidRef func(*testing.T, ArtifactStore, string) (Ref, string)
	}{
		{
			name: "manifest",
			invalidRef: func(t *testing.T, store ArtifactStore, origin string) (Ref, string) {
				data := []byte("not-json\n")
				hash := hashHex(data)
				ref, err := NewRef(origin, KindManifests, hash+".json")
				require.NoError(t, err)
				identity, err := NewIdentity(hash, int64(len(data)))
				require.NoError(t, err)
				_, err = store.Create(t.Context(), ref, identity,
					canonicalArtifactMediaType(KindManifests), bytes.NewReader(data))
				require.NoError(t, err)
				return ref, hash
			},
		},
		{
			name: "segment",
			invalidRef: func(t *testing.T, store ArtifactStore, origin string) (Ref, string) {
				segmentData := []byte("not-ndjson\n")
				segmentHash := hashHex(segmentData)
				segmentRef, err := NewRef(origin, KindSegments, segmentHash+".ndjson")
				require.NoError(t, err)
				identity, err := NewIdentity(segmentHash, int64(len(segmentData)))
				require.NoError(t, err)
				_, err = store.Create(t.Context(), segmentRef, identity,
					canonicalArtifactMediaType(KindSegments), bytes.NewReader(segmentData))
				require.NoError(t, err)
				m := manifest{
					Version: formatVersion, Origin: origin, NativeSessionID: "session-1",
					Session:  manifestSession{ID: "session-1", Machine: origin, Agent: "claude", Project: "alpha"},
					Segments: []string{segmentHash},
				}
				manifestData, err := canonicalJSON(m)
				require.NoError(t, err)
				manifestHash := hashHex(manifestData)
				manifestRef, err := NewRef(origin, KindManifests, manifestHash+".json")
				require.NoError(t, err)
				manifestIdentity, err := NewIdentity(manifestHash, int64(len(manifestData)))
				require.NoError(t, err)
				_, err = store.Create(t.Context(), manifestRef, manifestIdentity,
					canonicalArtifactMediaType(KindManifests), bytes.NewReader(manifestData))
				require.NoError(t, err)
				return segmentRef, manifestHash
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			origin := "peer-a1b2c3"
			store, err := newProtocolTestStore(t.TempDir())
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, store.Close()) })
			invalidRef, manifestHash := tt.invalidRef(t, store, origin)
			cp := checkpoint{
				Version: formatVersion, Origin: origin, Sequence: 1,
				Sessions: map[string]string{origin + "~session-1": manifestHash},
			}
			checkpointData, err := canonicalJSON(cp)
			require.NoError(t, err)
			checkpointRef, err := NewRef(origin, KindCheckpoints, "cp-0000000001.json")
			require.NoError(t, err)
			checkpointIdentity, err := NewIdentity(hashHex(checkpointData), int64(len(checkpointData)))
			require.NoError(t, err)
			_, err = store.Create(t.Context(), checkpointRef, checkpointIdentity,
				canonicalArtifactMediaType(KindCheckpoints), bytes.NewReader(checkpointData))
			require.NoError(t, err)

			result, err := importResultFromTestStore(
				t.Context(), testDB(t), store, "local-d4e5f6",
			)
			require.NoError(t, err)
			assert.Equal(t, 1, result.Deferred)
			_, err = store.Stat(t.Context(), invalidRef)
			assert.ErrorIs(t, err, ErrArtifactNotFound)
		})
	}
}

func TestExactImportProcessesOriginPagesIncrementally(t *testing.T) {
	base, err := newProtocolTestStore(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, base.Close()) })
	store := &originTraversalGateStore{ArtifactStore: base}

	_, err = importResultFromTestStore(t.Context(), testDB(t), store, "local-d4e5f6")
	require.NoError(t, err)
	assert.True(t, store.processedFirstPage)
}

func TestExactImportEnumeratesCheckpointHeadersBeforeBodies(t *testing.T) {
	origin := "peer-a1b2c3"
	base, err := newProtocolTestStore(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, base.Close()) })
	for sequence := 1; sequence <= artifactImportPageSize+1; sequence++ {
		cp := checkpoint{
			Version: formatVersion, Origin: origin, Sequence: sequence,
			Sessions: map[string]string{},
		}
		data, marshalErr := canonicalJSON(cp)
		require.NoError(t, marshalErr)
		ref, refErr := NewRef(origin, KindCheckpoints,
			fmt.Sprintf("cp-%010d.json", sequence))
		require.NoError(t, refErr)
		identity, identityErr := NewIdentity(hashHex(data), int64(len(data)))
		require.NoError(t, identityErr)
		_, createErr := base.Create(t.Context(), ref, identity,
			canonicalArtifactMediaType(KindCheckpoints), bytes.NewReader(data))
		require.NoError(t, createErr)
	}
	store := &checkpointHeaderFirstStore{ArtifactStore: base}
	target := testDB(t)

	_, err = importResultFromTestStore(t.Context(), target, store, "local-d4e5f6")
	require.NoError(t, err)
	assert.True(t, store.listedAll)
	landing, _, found, err := target.GetArtifactCheckpointLanding(t.Context(), origin)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, artifactImportPageSize+1, landing.Sequence)
}

func TestExactImportNewestCheckpointSkipsHistoricalClosures(t *testing.T) {
	origin := "peer-a1b2c3"
	source := testDB(t)
	seedSession(t, source, "session-1", "alpha")
	base, err := newProtocolTestStore(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, base.Close()) })
	_, err = ExportToStore(t.Context(), source, base, ExportOptions{
		Origin: origin,
		Full:   true,
	})
	require.NoError(t, err)
	firstPage, err := firstStoreEntryPage(t.Context(), base, origin, KindCheckpoints, 1)
	require.NoError(t, err)
	require.Len(t, firstPage.Items, 1)
	_, reader, err := base.Open(t.Context(), firstPage.Items[0].Ref)
	require.NoError(t, err)
	data, err := io.ReadAll(reader)
	require.NoError(t, err)
	require.NoError(t, reader.Verify())
	require.NoError(t, reader.Close())
	var cp checkpoint
	require.NoError(t, json.Unmarshal(data, &cp))
	for sequence := 2; sequence <= artifactImportPageSize*2+1; sequence++ {
		cp.Sequence = sequence
		checkpointData, marshalErr := canonicalJSON(cp)
		require.NoError(t, marshalErr)
		ref, refErr := NewRef(origin, KindCheckpoints,
			fmt.Sprintf("cp-%010d.json", sequence))
		require.NoError(t, refErr)
		identity, identityErr := NewIdentity(hashHex(checkpointData), int64(len(checkpointData)))
		require.NoError(t, identityErr)
		_, createErr := base.Create(t.Context(), ref, identity,
			canonicalArtifactMediaType(KindCheckpoints), bytes.NewReader(checkpointData))
		require.NoError(t, createErr)
	}
	store := &dependencyOpenCountingStore{
		ArtifactStore: base,
		opens:         make(map[Kind]int),
	}

	result, err := importResultFromTestStore(t.Context(), testDB(t), store, "local-d4e5f6")
	require.NoError(t, err)
	assert.Equal(t, 1, result.Sessions)
	assert.LessOrEqual(t, store.opens[KindManifests], 2,
		"a complete newest checkpoint must not open historical manifests")
	assert.LessOrEqual(t, store.opens[KindSegments], 2,
		"a complete newest checkpoint must not open historical segments")
	assert.LessOrEqual(t, store.opens[KindCheckpoints], 1,
		"checkpoint bodies must be inspected newest-first")
}

func createFutureMetadataEntries(
	t *testing.T, store ArtifactStore, origin string, count int,
) {
	t.Helper()
	for i := range count {
		stamp := HLCTimestamp{
			WallTime: fixedHLCTime().Add(time.Duration(i) * time.Nanosecond),
		}
		event := metadataEvent{
			Version: formatVersion + 1,
			HLC:     stamp.String(), Origin: origin,
			SessionGID: origin + "~future", Op: "future-op",
		}
		data, err := canonicalJSON(event)
		require.NoError(t, err)
		hash := hashHex(data)
		ref, err := NewRef(origin, KindMeta,
			stamp.OrderingKey(hash)+metadataEventExtension)
		require.NoError(t, err)
		identity, err := NewIdentity(hash, int64(len(data)))
		require.NoError(t, err)
		_, err = store.Create(t.Context(), ref, identity,
			canonicalArtifactMediaType(KindMeta), bytes.NewReader(data))
		require.NoError(t, err)
	}
}

func TestExactImportCancelsBetweenMetadataEntries(t *testing.T) {
	origin := "peer-a1b2c3"
	base, err := newProtocolTestStore(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, base.Close()) })
	createFutureMetadataEntries(t, base, origin, 2)
	ctx, cancel := context.WithCancel(t.Context())
	store := &cancelAfterMetadataOpenStore{ArtifactStore: base, cancel: cancel}

	_, err = importResultFromTestStore(ctx, testDB(t), store, "local-d4e5f6")
	require.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, 1, store.opens, "cancellation must stop before opening another event")
}

func TestExactImportDefersFutureManifestWithoutQuarantine(t *testing.T) {
	origin := "peer-a1b2c3"
	store, err := newProtocolTestStore(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	m := manifest{Version: formatVersion + 1, Origin: origin, NativeSessionID: "session-1"}
	manifestData, err := canonicalJSON(m)
	require.NoError(t, err)
	manifestHash := hashHex(manifestData)
	manifestRef, err := NewRef(origin, KindManifests, manifestHash+".json")
	require.NoError(t, err)
	manifestIdentity, err := NewIdentity(manifestHash, int64(len(manifestData)))
	require.NoError(t, err)
	_, err = store.Create(t.Context(), manifestRef, manifestIdentity,
		canonicalArtifactMediaType(KindManifests), bytes.NewReader(manifestData))
	require.NoError(t, err)
	cp := checkpoint{
		Version: formatVersion, Origin: origin, Sequence: 1,
		Sessions: map[string]string{origin + "~session-1": manifestHash},
	}
	checkpointData, err := canonicalJSON(cp)
	require.NoError(t, err)
	checkpointRef, err := NewRef(origin, KindCheckpoints, "cp-0000000001.json")
	require.NoError(t, err)
	checkpointIdentity, err := NewIdentity(hashHex(checkpointData), int64(len(checkpointData)))
	require.NoError(t, err)
	_, err = store.Create(t.Context(), checkpointRef, checkpointIdentity,
		canonicalArtifactMediaType(KindCheckpoints), bytes.NewReader(checkpointData))
	require.NoError(t, err)

	result, err := importResultFromTestStore(
		t.Context(), testDB(t), store, "local-d4e5f6",
	)
	require.NoError(t, err)
	assert.Equal(t, 1, result.Deferred)
	_, err = store.Stat(t.Context(), manifestRef)
	assert.NoError(t, err, "future protocol artifacts must remain live")
}

func TestExactImportDefersFutureSegmentWithoutQuarantine(t *testing.T) {
	origin := "peer-a1b2c3"
	store, err := newProtocolTestStore(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	segmentData, err := canonicalJSON(segmentMessage{
		Version: formatVersion + 1, Ordinal: 0, Role: "user", Content: "future",
	})
	require.NoError(t, err)
	segmentHash := hashHex(segmentData)
	segmentRef, err := NewRef(origin, KindSegments, segmentHash+".ndjson")
	require.NoError(t, err)
	segmentIdentity, err := NewIdentity(segmentHash, int64(len(segmentData)))
	require.NoError(t, err)
	_, err = store.Create(t.Context(), segmentRef, segmentIdentity,
		canonicalArtifactMediaType(KindSegments), bytes.NewReader(segmentData))
	require.NoError(t, err)
	m := manifest{
		Version: formatVersion, Origin: origin, NativeSessionID: "session-1",
		Session:  manifestSession{ID: "session-1", Machine: origin, Agent: "claude", Project: "alpha"},
		Segments: []string{segmentHash},
	}
	manifestData, err := canonicalJSON(m)
	require.NoError(t, err)
	manifestHash := hashHex(manifestData)
	manifestRef, err := NewRef(origin, KindManifests, manifestHash+".json")
	require.NoError(t, err)
	manifestIdentity, err := NewIdentity(manifestHash, int64(len(manifestData)))
	require.NoError(t, err)
	_, err = store.Create(t.Context(), manifestRef, manifestIdentity,
		canonicalArtifactMediaType(KindManifests), bytes.NewReader(manifestData))
	require.NoError(t, err)
	cp := checkpoint{
		Version: formatVersion, Origin: origin, Sequence: 1,
		Sessions: map[string]string{origin + "~session-1": manifestHash},
	}
	checkpointData, err := canonicalJSON(cp)
	require.NoError(t, err)
	checkpointRef, err := NewRef(origin, KindCheckpoints, "cp-0000000001.json")
	require.NoError(t, err)
	checkpointIdentity, err := NewIdentity(hashHex(checkpointData), int64(len(checkpointData)))
	require.NoError(t, err)
	_, err = store.Create(t.Context(), checkpointRef, checkpointIdentity,
		canonicalArtifactMediaType(KindCheckpoints), bytes.NewReader(checkpointData))
	require.NoError(t, err)

	result, err := importResultFromTestStore(
		t.Context(), testDB(t), store, "local-d4e5f6",
	)
	require.NoError(t, err)
	assert.Equal(t, 1, result.Deferred)
	_, err = store.Stat(t.Context(), segmentRef)
	assert.NoError(t, err, "future protocol segments must remain live")
}

func TestExactImportCountsFutureCheckpointAsDeferred(t *testing.T) {
	origin := "peer-a1b2c3"
	store, err := newProtocolTestStore(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	future := checkpoint{
		Version: formatVersion + 1, Origin: origin, Sequence: 1,
		Sessions: map[string]string{origin + "~future": strings.Repeat("f", 64)},
	}
	data, err := canonicalJSON(future)
	require.NoError(t, err)
	ref, err := NewRef(origin, KindCheckpoints, "cp-0000000001.json")
	require.NoError(t, err)
	identity, err := NewIdentity(hashHex(data), int64(len(data)))
	require.NoError(t, err)
	_, err = store.Create(t.Context(), ref, identity,
		canonicalArtifactMediaType(KindCheckpoints), bytes.NewReader(data))
	require.NoError(t, err)

	result, err := importResultFromTestStore(
		t.Context(), testDB(t), store, "local-d4e5f6",
	)
	require.NoError(t, err)
	assert.Equal(t, 1, result.Deferred)
	_, err = store.Stat(t.Context(), ref)
	assert.NoError(t, err)
}

func TestExactImportLandsUpdatedLocallyTrashedSession(t *testing.T) {
	origin := "peer-a1b2c3"
	source := testDB(t)
	seedSession(t, source, "session-1", "alpha")
	store, err := newProtocolTestStore(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	_, err = ExportToStore(t.Context(), source, store, ExportOptions{Origin: origin, Full: true})
	require.NoError(t, err)
	target := testDB(t)
	_, err = importResultFromTestStore(t.Context(), target, store, "local-d4e5f6")
	require.NoError(t, err)
	importedID := origin + "~session-1"
	require.NoError(t, target.SoftDeleteSession(importedID))

	seedSession(t, source, "session-1", "updated")
	_, err = ExportToStore(t.Context(), source, store, ExportOptions{Origin: origin, Full: true})
	require.NoError(t, err)
	result, err := importResultFromTestStore(t.Context(), target, store, "local-d4e5f6")
	require.NoError(t, err)
	assert.False(t, result.Changed(), "import must not resurrect a locally trashed replica")
	trashed, err := target.GetSessionFull(t.Context(), importedID)
	require.NoError(t, err)
	require.NotNil(t, trashed)
	assert.NotNil(t, trashed.DeletedAt)
	assert.Equal(t, "alpha", trashed.Project,
		"the newer peer manifest must not rewrite locally trashed content")
	landing, manifests, ok, err := target.GetArtifactCheckpointLanding(t.Context(), origin)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, 2, landing.Sequence)
	assert.NotEmpty(t, manifests[importedID])
}

func TestQueuedArtifactExportTreatsOwnershipLossAsDeletion(t *testing.T) {
	database := testDB(t)
	require.NoError(t, database.UpsertSession(db.Session{
		ID: "moved", Project: "project", Machine: "local", Agent: "claude",
	}))
	require.NoError(t, database.ReplaceSessionMessages("moved", []db.Message{{
		SessionID: "moved", Ordinal: 0, Role: "user", Content: "local content",
	}}))
	require.NoError(t, database.UpsertSession(db.Session{
		ID: "moved", Project: "project", Machine: "peer-a1b2c3", Agent: "claude",
	}))

	store := &countingQueuedExportStore{database: database}
	visited := 0
	err := forEachQueuedArtifactExport(t.Context(), store, 1, func(work queuedArtifactExport) error {
		visited++
		assert.Nil(t, work.Session)
		assert.Empty(t, work.Messages)
		assert.Empty(t, work.UsageEvents)
		return nil
	})
	require.NoError(t, err)
	assert.Equal(t, 1, visited)
	assert.Equal(t, 1, store.sessionLoads)
	assert.Zero(t, store.messageLoads)
	assert.Zero(t, store.usageLoads)
}

func (r *recordingSyncStateValueReader) SyncStateValues(
	keys []string,
) (map[string]string, error) {
	r.calls++
	r.keys = append([]string(nil), keys...)
	result := make(map[string]string)
	for _, key := range keys {
		if value := r.states[key]; value != "" {
			result[key] = value
		}
	}
	return result, nil
}

func TestImportedSessionIDsReadsOnlyCandidateProvenance(t *testing.T) {
	reader := &recordingSyncStateValueReader{states: map[string]string{
		"artifact_import:desk-a1b2c3:desk-a1b2c3~one":     "manifest-one",
		"artifact_import:laptop-d4e5f6:laptop-d4e5f6~two": "manifest-two",
	}}

	got, err := ImportedSessionIDs(reader, []string{
		"desk-a1b2c3~one",
		"local-session",
		"phone-112233~missing",
	})
	require.NoError(t, err)
	assert.Equal(t, map[string]struct{}{"desk-a1b2c3~one": {}}, got)
	assert.Equal(t, []string{
		"artifact_import:desk-a1b2c3:desk-a1b2c3~one",
		"artifact_import:phone-112233:phone-112233~missing",
	}, reader.keys)
	assert.Equal(t, 1, reader.calls)

	empty, err := ImportedSessionIDs(reader, nil)
	require.NoError(t, err)
	assert.Empty(t, empty)
	assert.Equal(t, 1, reader.calls,
		"a no-candidate push must not query artifact provenance")
}

func TestLoadImportStatesReadsCheckpointInOneBatch(t *testing.T) {
	reader := &recordingSyncStateValueReader{states: map[string]string{
		"artifact_import:desk-a1b2c3:desk-a1b2c3~one": "manifest-one",
		"artifact_import:desk-a1b2c3:desk-a1b2c3~two": "manifest-two",
	}}

	states, err := loadImportStates(reader, "desk-a1b2c3", []string{
		"desk-a1b2c3~one", "desk-a1b2c3~two", "desk-a1b2c3~three",
	})
	require.NoError(t, err)
	assert.Equal(t, map[string]string{
		"artifact_import:desk-a1b2c3:desk-a1b2c3~one": "manifest-one",
		"artifact_import:desk-a1b2c3:desk-a1b2c3~two": "manifest-two",
	}, states)
	assert.Equal(t, []string{
		"artifact_import:desk-a1b2c3:desk-a1b2c3~one",
		"artifact_import:desk-a1b2c3:desk-a1b2c3~two",
		"artifact_import:desk-a1b2c3:desk-a1b2c3~three",
	}, reader.keys)
	assert.Equal(t, 1, reader.calls)
}

func TestIsFolderTargetAcceptsWindowsDrivePaths(t *testing.T) {
	tests := []struct {
		name   string
		target string
		want   bool
	}{
		{"windows backslash path", `C:\Users\runner\artifacts`, true},
		{"windows slash path", `C:/Users/runner/artifacts`, true},
		{"posix path", "/tmp/agentsview-artifacts", true},
		{"relative path", "artifacts", true},
		{"http peer", "https://peer.example.test/artifacts", false},
		{"s3 target", "s3://bucket/artifacts", false},
		{"host port", "localhost:8080", false},
		{"empty", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, IsFolderTarget(tt.target))
		})
	}
}

func TestAdoptOriginPersistsConfigOrigin(t *testing.T) {
	database := testDB(t)

	require.NoError(t, AdoptOrigin(database, "desk-a1b2c3"))

	stored, err := StoredOrigin(database)
	require.NoError(t, err)
	assert.Equal(t, "desk-a1b2c3", stored)

	// EnsureOrigin and its callers now agree with the adopted origin instead
	// of generating a divergent DB-only value.
	ensured, err := EnsureOrigin(database)
	require.NoError(t, err)
	assert.Equal(t, "desk-a1b2c3", ensured)
}

func TestAdoptOriginIsIdempotent(t *testing.T) {
	database := testDB(t)

	require.NoError(t, AdoptOrigin(database, "desk-a1b2c3"))
	require.NoError(t, AdoptOrigin(database, "desk-a1b2c3"))

	stored, err := StoredOrigin(database)
	require.NoError(t, err)
	assert.Equal(t, "desk-a1b2c3", stored)
}

func TestAdoptOriginOverwritesDivergentDBOrigin(t *testing.T) {
	database := testDB(t)

	// Simulate the pre-fix state: the recorder generated a DB-only origin
	// before the authoritative config origin existed.
	stale, err := EnsureOrigin(database)
	require.NoError(t, err)
	require.NotEqual(t, "desk-a1b2c3", stale)

	require.NoError(t, AdoptOrigin(database, "desk-a1b2c3"))

	stored, err := StoredOrigin(database)
	require.NoError(t, err)
	assert.Equal(t, "desk-a1b2c3", stored)
}

func TestAdoptOriginRejectsInvalidOrigin(t *testing.T) {
	database := testDB(t)

	err := AdoptOrigin(database, "../outside")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "adopting artifact origin")

	stored, err := StoredOrigin(database)
	require.NoError(t, err)
	assert.Empty(t, stored)
}

func TestEnsureOriginRejectsInvalidPersistedOrigin(t *testing.T) {
	database := testDB(t)
	require.NoError(t, database.SetSyncState(originStateKey, "../outside"))

	origin, err := EnsureOrigin(database)
	require.Error(t, err)
	assert.Empty(t, origin)
	assert.Contains(t, err.Error(), "stored artifact origin")
	assert.Contains(t, err.Error(), "invalid artifact origin")
}

func TestSyncFolderRoundTripImportsForeignSession(t *testing.T) {
	ctx := context.Background()
	share := t.TempDir()
	aData := t.TempDir()
	bData := t.TempDir()
	aDB := testDB(t)
	bDB := testDB(t)

	require.NoError(t, aDB.SetSyncState(originStateKey, "laptop-a1b2c3"))
	require.NoError(t, bDB.SetSyncState(originStateKey, "desktop-d4e5f6"))
	seedSession(t, aDB, "sess-1", "alpha")

	aRes, err := SyncFolder(ctx, aDB, SyncOptions{DataDir: aData, Target: share})
	require.NoError(t, err)
	assert.Equal(t, "laptop-a1b2c3", aRes.Origin)
	assert.Equal(t, 1, aRes.ExportedSessions)

	bRes, err := SyncFolder(ctx, bDB, SyncOptions{DataDir: bData, Target: share})
	require.NoError(t, err)
	assert.Equal(t, 1, bRes.ImportedSessions)
	assert.Equal(t, 2, bRes.ImportedMessages)

	bRes, err = SyncFolder(ctx, bDB, SyncOptions{DataDir: bData, Target: share})
	require.NoError(t, err)
	assert.Zero(t, bRes.ImportedSessions)
	assert.Zero(t, bRes.ImportedMessages)

	got, err := bDB.GetSessionFull(ctx, "laptop-a1b2c3~sess-1")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "laptop-a1b2c3", got.Machine)
	assert.Equal(t, "alpha", got.Project)

	msgs, err := bDB.GetAllMessages(ctx, got.ID)
	require.NoError(t, err)
	require.Len(t, msgs, 2)
	assert.Equal(t, "user", msgs[0].Role)
	assert.Equal(t, "hello", msgs[0].Content)
	assert.Equal(t, "assistant", msgs[1].Role)
	assert.Equal(t, "world", msgs[1].Content)
}

func TestSyncFolderRoundTripPreservesSessionName(t *testing.T) {
	ctx := context.Background()
	share := t.TempDir()
	aData := t.TempDir()
	bData := t.TempDir()
	aDB := testDB(t)
	bDB := testDB(t)

	require.NoError(t, aDB.SetSyncState(originStateKey, "laptop-a1b2c3"))
	require.NoError(t, bDB.SetSyncState(originStateKey, "desktop-d4e5f6"))
	sessionName := "Parser Provided Title"
	seedSession(t, aDB, "sess-1", "alpha", func(s *db.Session) {
		s.SessionName = &sessionName
	})

	_, err := SyncFolder(ctx, aDB, SyncOptions{DataDir: aData, Target: share})
	require.NoError(t, err)

	bRes, err := SyncFolder(ctx, bDB, SyncOptions{DataDir: bData, Target: share})
	require.NoError(t, err)
	require.Equal(t, 1, bRes.ImportedSessions)

	got, err := bDB.GetSessionFull(ctx, "laptop-a1b2c3~sess-1")
	require.NoError(t, err)
	require.NotNil(t, got)
	require.NotNil(t, got.SessionName)
	assert.Equal(t, sessionName, *got.SessionName)
}

func TestSyncFolderInitBaselineMetadataConvergesCuration(t *testing.T) {
	ctx := context.Background()
	share := t.TempDir()
	aData := t.TempDir()
	bData := t.TempDir()
	aDB := testDB(t)
	bDB := testDB(t)

	require.NoError(t, AdoptOrigin(aDB, "laptop-a1b2c3"))
	require.NoError(t, AdoptOrigin(bDB, "desktop-d4e5f6"))
	seedSession(t, aDB, "sess-1", "alpha")
	require.NoError(t, aDB.ReplaceSessionMessages("sess-1", []db.Message{
		{
			SessionID:     "sess-1",
			Ordinal:       0,
			Role:          "user",
			Content:       "hello",
			ContentLength: 5,
			SourceUUID:    "uuid-question",
		},
		{
			SessionID:     "sess-1",
			Ordinal:       1,
			Role:          "assistant",
			Content:       "world",
			ContentLength: 5,
			SourceUUID:    "uuid-answer",
		},
	}))
	displayName := "Already renamed"
	require.NoError(t, aDB.RenameSession("sess-1", &displayName))
	starred, err := aDB.StarSession("sess-1")
	require.NoError(t, err)
	require.True(t, starred)
	msgs, err := aDB.GetAllMessages(ctx, "sess-1")
	require.NoError(t, err)
	require.Len(t, msgs, 2)
	note := "already pinned"
	_, err = aDB.PinMessage("sess-1", msgs[1].ID, &note)
	require.NoError(t, err)

	_, err = SyncFolder(ctx, aDB, SyncOptions{
		DataDir:          aData,
		Target:           share,
		BaselineMetadata: true,
	})
	require.NoError(t, err)
	res, err := SyncFolder(ctx, bDB, SyncOptions{
		DataDir: bData,
		Target:  share,
	})
	require.NoError(t, err)
	assert.Equal(t, 1, res.ImportedSessions)
	assert.Equal(t, 2, res.ImportedMessages)
	assert.Equal(t, 3, res.ImportedMetadata)

	gid := "laptop-a1b2c3~sess-1"
	got, err := bDB.GetSessionFull(ctx, gid)
	require.NoError(t, err)
	require.NotNil(t, got)
	require.NotNil(t, got.DisplayName)
	assert.Equal(t, displayName, *got.DisplayName)
	stars, err := bDB.ListStarredSessionIDs(ctx)
	require.NoError(t, err)
	assert.Equal(t, []string{gid}, stars)
	pins, err := bDB.ListPinnedMessages(ctx, gid, "")
	require.NoError(t, err)
	require.Len(t, pins, 1)
	assert.Equal(t, 1, pins[0].Ordinal)
	require.NotNil(t, pins[0].Note)
	assert.Equal(t, note, *pins[0].Note)

	op, ok, err := bDB.MetadataReplayStateOp(ctx, gid, "display_name")
	require.NoError(t, err)
	assert.True(t, ok)
	assert.Equal(t, MetadataOpRename, op)
	op, ok, err = bDB.MetadataReplayStateOp(ctx, gid, "starred")
	require.NoError(t, err)
	assert.True(t, ok)
	assert.Equal(t, MetadataOpStar, op)
	op, ok, err = bDB.MetadataReplayStateOp(ctx, gid, "pin:source_uuid:uuid-answer")
	require.NoError(t, err)
	assert.True(t, ok)
	assert.Equal(t, MetadataOpPin, op)
}

func TestSyncFolderInitBaselineMetadataConvergesTrash(t *testing.T) {
	ctx := context.Background()
	share := t.TempDir()
	aData := t.TempDir()
	bData := t.TempDir()
	aDB := testDB(t)
	bDB := testDB(t)

	require.NoError(t, AdoptOrigin(aDB, "laptop-a1b2c3"))
	require.NoError(t, AdoptOrigin(bDB, "desktop-d4e5f6"))
	seedSession(t, aDB, "sess-1", "alpha")

	_, err := SyncFolder(ctx, aDB, SyncOptions{
		DataDir: aData,
		Target:  share,
	})
	require.NoError(t, err)
	_, err = SyncFolder(ctx, bDB, SyncOptions{
		DataDir: bData,
		Target:  share,
	})
	require.NoError(t, err)

	require.NoError(t, aDB.SoftDeleteSession("sess-1"))

	_, err = SyncFolder(ctx, aDB, SyncOptions{
		DataDir:          aData,
		Target:           share,
		BaselineMetadata: true,
	})
	require.NoError(t, err)
	res, err := SyncFolder(ctx, bDB, SyncOptions{
		DataDir: bData,
		Target:  share,
	})
	require.NoError(t, err)
	assert.Equal(t, 1, res.ImportedMetadata)

	gid := "laptop-a1b2c3~sess-1"
	got, err := bDB.GetSessionFull(ctx, gid)
	require.NoError(t, err)
	require.NotNil(t, got)
	require.NotNil(t, got.DeletedAt)
	op, ok, err := bDB.MetadataReplayStateOp(ctx, gid, "deleted_at")
	require.NoError(t, err)
	assert.True(t, ok)
	assert.Equal(t, MetadataOpSoftDelete, op)
}

func TestSyncFolderInitBaselinesCurationOfTrashedSession(t *testing.T) {
	ctx := context.Background()
	share := t.TempDir()
	aData := t.TempDir()
	bData := t.TempDir()
	aDB := testDB(t)
	bDB := testDB(t)

	require.NoError(t, AdoptOrigin(aDB, "laptop-a1b2c3"))
	require.NoError(t, AdoptOrigin(bDB, "desktop-d4e5f6"))
	seedSession(t, aDB, "sess-1", "alpha")
	require.NoError(t, aDB.ReplaceSessionMessages("sess-1", []db.Message{{
		SessionID: "sess-1", Ordinal: 0, Role: "user", Content: "hello",
		ContentLength: 5, SourceUUID: "uuid-question",
	}}))
	displayName := "Renamed before trash"
	require.NoError(t, aDB.RenameSession("sess-1", &displayName))
	starred, err := aDB.StarSession("sess-1")
	require.NoError(t, err)
	require.True(t, starred)
	msgs, err := aDB.GetAllMessages(ctx, "sess-1")
	require.NoError(t, err)
	require.Len(t, msgs, 1)
	note := "pinned before trash"
	_, err = aDB.PinMessage("sess-1", msgs[0].ID, &note)
	require.NoError(t, err)
	require.NoError(t, aDB.SoftDeleteSession("sess-1"))

	// The session sits in trash when the machine first opts in: its curation
	// must still baseline, or a later restore reaches peers without it.
	_, err = SyncFolder(ctx, aDB, SyncOptions{
		DataDir:          aData,
		Target:           share,
		BaselineMetadata: true,
	})
	require.NoError(t, err)
	_, err = SyncFolder(ctx, bDB, SyncOptions{DataDir: bData, Target: share})
	require.NoError(t, err)

	// Restoring on A publishes the session content; B then converges on a
	// visible session that kept its pre-init name, star, and pin.
	_, err = aDB.RestoreSession("sess-1")
	require.NoError(t, err)
	repository, err := OpenRepository(ctx, aData)
	require.NoError(t, err)
	recorder := NewMetadataRecorder(aDB, MetadataRecorderOptions{
		Store:  repository.Content(),
		Origin: "laptop-a1b2c3",
	})
	_, err = recorder.Append(ctx, MetadataEventInput{
		SessionID: "sess-1",
		Op:        MetadataOpRestore,
	})
	require.NoError(t, err)
	require.NoError(t, repository.Close())
	_, err = SyncFolder(ctx, aDB, SyncOptions{DataDir: aData, Target: share})
	require.NoError(t, err)
	_, err = SyncFolder(ctx, bDB, SyncOptions{DataDir: bData, Target: share})
	require.NoError(t, err)

	gid := "laptop-a1b2c3~sess-1"
	got, err := bDB.GetSessionFull(ctx, gid)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Nil(t, got.DeletedAt)
	require.NotNil(t, got.DisplayName)
	assert.Equal(t, displayName, *got.DisplayName)
	stars, err := bDB.ListStarredSessionIDs(ctx)
	require.NoError(t, err)
	assert.Equal(t, []string{gid}, stars)
	pins, err := bDB.ListPinnedMessages(ctx, gid, "")
	require.NoError(t, err)
	require.Len(t, pins, 1)
	require.NotNil(t, pins[0].Note)
	assert.Equal(t, note, *pins[0].Note)
}

func TestSyncFolderImportClearsSourceFileState(t *testing.T) {
	ctx := context.Background()
	share := t.TempDir()
	aData := t.TempDir()
	bData := t.TempDir()
	aDB := testDB(t)
	bDB := testDB(t)
	sourcePath := filepath.Join(t.TempDir(), "shared-session.jsonl")
	peerHash := strings64("a")
	localHash := strings64("b")
	lastEntryUUID := "entry-99"

	require.NoError(t, aDB.SetSyncState(originStateKey, "laptop-a1b2c3"))
	require.NoError(t, bDB.SetSyncState(originStateKey, "desktop-d4e5f6"))
	seedSession(t, aDB, "sess-1", "alpha", func(s *db.Session) {
		s.FilePath = &sourcePath
		s.FileSize = new(int64)
		*s.FileSize = 4096
		s.FileMtime = new(int64)
		*s.FileMtime = 200
		s.NextOrdinal = 99
		s.LastEntryUUID = &lastEntryUUID
		s.FileInode = new(int64)
		*s.FileInode = 12345
		s.FileDevice = new(int64)
		*s.FileDevice = 67890
		s.FileHash = &peerHash
	})
	seedSession(t, bDB, "local-sess", "alpha", func(s *db.Session) {
		s.FilePath = &sourcePath
		s.FileMtime = new(int64)
		*s.FileMtime = 100
		s.FileHash = &localHash
	})

	_, err := SyncFolder(ctx, aDB, SyncOptions{DataDir: aData, Target: share})
	require.NoError(t, err)
	res, err := SyncFolder(ctx, bDB, SyncOptions{DataDir: bData, Target: share})
	require.NoError(t, err)
	require.Equal(t, 1, res.ImportedSessions)

	imported, err := bDB.GetSessionFull(ctx, "laptop-a1b2c3~sess-1")
	require.NoError(t, err)
	require.NotNil(t, imported)
	assert.Nil(t, imported.FilePath)
	assert.Nil(t, imported.FileSize)
	assert.Nil(t, imported.FileMtime)
	assert.Zero(t, imported.NextOrdinal)
	assert.Nil(t, imported.LastEntryUUID)
	assert.Nil(t, imported.FileInode)
	assert.Nil(t, imported.FileDevice)
	assert.Nil(t, imported.FileHash)

	ids, err := bDB.ListSessionIDsByFilePath(sourcePath, "claude")
	require.NoError(t, err)
	assert.Equal(t, []string{"local-sess"}, ids)
	gotHash, ok := bDB.GetFileHashByPath(sourcePath)
	require.True(t, ok)
	assert.Equal(t, localHash, gotHash)
}

func TestSyncFolderRoundTripPreservesSessionSignals(t *testing.T) {
	ctx := context.Background()
	share := t.TempDir()
	aData := t.TempDir()
	bData := t.TempDir()
	aDB := testDB(t)
	bDB := testDB(t)

	require.NoError(t, aDB.SetSyncState(originStateKey, "laptop-a1b2c3"))
	require.NoError(t, bDB.SetSyncState(originStateKey, "desktop-d4e5f6"))
	seedSession(t, aDB, "sess-1", "alpha")

	// Signal columns are written outside UpsertSession, so seed them through
	// the same writer paths the live app uses. These include fields the Session
	// JSON drops (json:"-"): has_tool_calls, has_context_data, and the quality
	// scalars. Secret-scan state is seeded too, but unlike the others it is
	// deliberately not carried across import (asserted below).
	require.NoError(t, aDB.UpdateSessionSignals("sess-1", db.SessionSignalUpdate{
		HasToolCalls:   true,
		HasContextData: true,
		Outcome:        "success",
		QualitySignals: db.QualitySignals{
			Version:              3,
			ShortPromptCount:     2,
			UnstructuredStart:    true,
			RunawayToolLoopCount: 1,
		},
	}))
	require.NoError(t, aDB.ReplaceSessionSecretFindings("sess-1", nil, 0, "rules-v7"))

	_, err := SyncFolder(ctx, aDB, SyncOptions{DataDir: aData, Target: share})
	require.NoError(t, err)

	bRes, err := SyncFolder(ctx, bDB, SyncOptions{DataDir: bData, Target: share})
	require.NoError(t, err)
	require.Equal(t, 1, bRes.ImportedSessions)

	got, err := bDB.GetSessionFull(ctx, "laptop-a1b2c3~sess-1")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.True(t, got.HasToolCalls, "has_tool_calls should survive the round trip")
	assert.True(t, got.HasContextData, "has_context_data should survive the round trip")
	assert.Equal(t, "success", got.Outcome)
	// Secret findings are not carried in the manifest, so the imported session
	// is treated as unscanned: the source rules version is dropped so
	// `secrets scan --backfill` rescans it with local rules.
	assert.Empty(t, got.SecretsRulesVersion,
		"secret-scan state must not be restored without findings")

	qs := got.StoredQualitySignals()
	require.NotNil(t, qs, "quality signals should survive the round trip")
	assert.Equal(t, 3, qs.Version)
	assert.Equal(t, 2, qs.ShortPromptCount)
	assert.True(t, qs.UnstructuredStart)
	assert.Equal(t, 1, qs.RunawayToolLoopCount)
}

func TestSyncFolderRoundTripRewritesForeignRelationshipIDs(t *testing.T) {
	ctx := context.Background()
	share := t.TempDir()
	aData := t.TempDir()
	bData := t.TempDir()
	aDB := testDB(t)
	bDB := testDB(t)

	require.NoError(t, aDB.SetSyncState(originStateKey, "laptop-a1b2c3"))
	require.NoError(t, bDB.SetSyncState(originStateKey, "desktop-d4e5f6"))
	seedSession(t, aDB, "source-1", "alpha")
	seedSession(t, aDB, "parent-1", "alpha")
	seedSession(t, aDB, "child-1", "alpha")
	parentID := "parent-1"
	seedSession(t, aDB, "sess-1", "alpha", func(s *db.Session) {
		s.SourceSessionID = "source-1"
		s.ParentSessionID = &parentID
	})
	require.NoError(t, aDB.ReplaceSessionMessages("sess-1", []db.Message{
		{
			SessionID:     "sess-1",
			Ordinal:       0,
			Role:          "assistant",
			Content:       "delegating",
			ContentLength: 10,
			ToolCalls: []db.ToolCall{{
				ToolName:          "Task",
				Category:          "Task",
				ToolUseID:         "toolu_1",
				SubagentSessionID: "child-1",
				ResultEvents: []db.ToolResultEvent{{
					ToolUseID:         "toolu_1",
					AgentID:           "agent-1",
					SubagentSessionID: "child-1",
					Source:            "tool_result",
					Status:            "success",
					Content:           "done",
					ContentLength:     4,
					EventIndex:        0,
				}},
			}},
		},
	}))

	_, err := SyncFolder(ctx, aDB, SyncOptions{DataDir: aData, Target: share})
	require.NoError(t, err)
	_, err = SyncFolder(ctx, bDB, SyncOptions{DataDir: bData, Target: share})
	require.NoError(t, err)

	got, err := bDB.GetSessionFull(ctx, "laptop-a1b2c3~sess-1")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "laptop-a1b2c3~source-1", got.SourceSessionID)
	require.NotNil(t, got.ParentSessionID)
	assert.Equal(t, "laptop-a1b2c3~parent-1", *got.ParentSessionID)

	msgs, err := bDB.GetAllMessages(ctx, got.ID)
	require.NoError(t, err)
	require.Len(t, msgs, 1)
	require.Len(t, msgs[0].ToolCalls, 1)
	assert.Equal(t, "laptop-a1b2c3~child-1", msgs[0].ToolCalls[0].SubagentSessionID)
	require.Len(t, msgs[0].ToolCalls[0].ResultEvents, 1)
	assert.Equal(t, "laptop-a1b2c3~child-1",
		msgs[0].ToolCalls[0].ResultEvents[0].SubagentSessionID)
}

// Recent Edits and edit file grouping read the persisted
// tool_calls.file_path column, so the segment format must carry it:
// imported foreign sessions get no parse-time re-derivation and the
// one-time file_path backfill has already run on existing databases.
func TestSyncFolderRoundTripPreservesToolCallFilePath(t *testing.T) {
	ctx := context.Background()
	share := t.TempDir()
	aData := t.TempDir()
	bData := t.TempDir()
	aDB := testDB(t)
	bDB := testDB(t)

	require.NoError(t, aDB.SetSyncState(originStateKey, "laptop-a1b2c3"))
	require.NoError(t, bDB.SetSyncState(originStateKey, "desktop-d4e5f6"))
	seedSession(t, aDB, "sess-1", "alpha")
	require.NoError(t, aDB.ReplaceSessionMessages("sess-1", []db.Message{
		{
			SessionID:     "sess-1",
			Ordinal:       0,
			Role:          "assistant",
			Content:       "editing",
			ContentLength: 7,
			ToolCalls: []db.ToolCall{{
				ToolName:  "Edit",
				Category:  "Edit",
				ToolUseID: "toolu_1",
				InputJSON: `{"file_path":"src/app.go"}`,
				FilePath:  "src/app.go",
			}},
		},
	}))

	_, err := SyncFolder(ctx, aDB, SyncOptions{DataDir: aData, Target: share})
	require.NoError(t, err)
	_, err = SyncFolder(ctx, bDB, SyncOptions{DataDir: bData, Target: share})
	require.NoError(t, err)

	msgs, err := bDB.GetAllMessages(ctx, "laptop-a1b2c3~sess-1")
	require.NoError(t, err)
	require.Len(t, msgs, 1)
	require.Len(t, msgs[0].ToolCalls, 1)
	assert.Equal(t, "src/app.go", msgs[0].ToolCalls[0].FilePath)
}

// TestSyncFolderImportLeavesScannedSessionBackfillable verifies that a session
// scanned for secrets at the source (rules version, a finding row, a nonzero
// leak count) is imported as unscanned, because the manifest carries no finding
// rows. The imported session must have no leak count and no rules version, so it
// stays a `secrets scan --backfill` candidate even when the source rules version
// is current on the importing machine. Stamping it scanned-at-source-version
// would skip a secret-bearing session, leaving a leak count with no revealable
// findings.
func TestSyncFolderImportLeavesScannedSessionBackfillable(t *testing.T) {
	ctx := context.Background()
	share := t.TempDir()
	aData := t.TempDir()
	bData := t.TempDir()
	aDB := testDB(t)
	bDB := testDB(t)

	require.NoError(t, aDB.SetSyncState(originStateKey, "laptop-a1b2c3"))
	require.NoError(t, bDB.SetSyncState(originStateKey, "desktop-d4e5f6"))
	seedSession(t, aDB, "sess-1", "alpha")

	// The source session is fully scanned: a finding row, a nonzero leak count,
	// and a rules version that is also current on the importing machine below.
	const rulesVersion = "rules-current"
	findings := []db.SecretFinding{{
		SessionID:      "sess-1",
		RuleName:       "aws-access-key",
		Confidence:     "definite",
		LocationKind:   "message",
		MessageOrdinal: 1,
		MatchStart:     4,
		MatchEnd:       24,
		RedactedMatch:  "AKIA…MPLE",
		RulesVersion:   rulesVersion,
	}}
	require.NoError(t, aDB.ReplaceSessionSecretFindings("sess-1", findings, 1, rulesVersion))

	_, err := SyncFolder(ctx, aDB, SyncOptions{DataDir: aData, Target: share})
	require.NoError(t, err)

	bRes, err := SyncFolder(ctx, bDB, SyncOptions{DataDir: bData, Target: share})
	require.NoError(t, err)
	require.Equal(t, 1, bRes.ImportedSessions)

	const importedID = "laptop-a1b2c3~sess-1"
	got, err := bDB.GetSessionFull(ctx, importedID)
	require.NoError(t, err)
	require.NotNil(t, got)
	// Imported session must be unscanned so its state is consistent with the zero
	// findings carried in the manifest.
	assert.Empty(t, got.SecretsRulesVersion, "imported session must not be stamped scanned")
	assert.Zero(t, got.SecretLeakCount, "imported session must not claim leaks without findings")

	// With the source rules version current on the importing machine, backfill
	// must still treat the imported session as a candidate (secrets_rules_version
	// "" != current) instead of skipping it.
	cands, err := bDB.SecretScanCandidates(ctx, db.SecretScanCandidateFilter{
		CurrentVersion: rulesVersion,
		OnlyStale:      true,
	})
	require.NoError(t, err)
	assert.Contains(t, cands, importedID,
		"secret-bearing imported session must be a backfill candidate, not skipped")
}

// TestSyncFolderSourceLeakCountChangeKeepsLocalFindings verifies that a
// source-side secret rescan that changes only secret_leak_count (not message
// content) does not alter the artifact manifest hash, so the importer neither
// re-imports the session nor clears the findings it scanned locally.
// secret_leak_count is the only secret field carried in the Session JSON, and
// import discards secret-scan state, so it must not influence the
// content-addressed manifest.
func TestSyncFolderSourceLeakCountChangeKeepsLocalFindings(t *testing.T) {
	ctx := context.Background()
	share := t.TempDir()
	aData := t.TempDir()
	bData := t.TempDir()
	aDB := testDB(t)
	bDB := testDB(t)

	require.NoError(t, aDB.SetSyncState(originStateKey, "laptop-a1b2c3"))
	require.NoError(t, bDB.SetSyncState(originStateKey, "desktop-d4e5f6"))
	seedSession(t, aDB, "sess-1", "alpha")

	// First round trip: A exports sess-1 (no secrets yet), B imports it.
	_, err := SyncFolder(ctx, aDB, SyncOptions{DataDir: aData, Target: share})
	require.NoError(t, err)
	bRes, err := SyncFolder(ctx, bDB, SyncOptions{DataDir: bData, Target: share})
	require.NoError(t, err)
	require.Equal(t, 1, bRes.ImportedSessions)

	const importedID = "laptop-a1b2c3~sess-1"

	// B scans the imported session locally and records a finding.
	bFinding := []db.SecretFinding{{
		SessionID: importedID, RuleName: "aws-access-key", Confidence: "definite",
		LocationKind: "message", MessageOrdinal: 0, MatchStart: 4, MatchEnd: 24,
		RedactedMatch: "AKIA…MPLE", RulesVersion: "rules-b",
	}}
	require.NoError(t, bDB.ReplaceSessionSecretFindings(importedID, bFinding, 1, "rules-b"))

	// A rescans sess-1: only secret_leak_count changes (0 -> 1); the message
	// content A exports is untouched.
	aFinding := []db.SecretFinding{{
		SessionID: "sess-1", RuleName: "aws-access-key", Confidence: "definite",
		LocationKind: "message", MessageOrdinal: 0, MatchStart: 4, MatchEnd: 24,
		RedactedMatch: "AKIA…MPLE", RulesVersion: "rules-a",
	}}
	require.NoError(t, aDB.ReplaceSessionSecretFindings("sess-1", aFinding, 1, "rules-a"))

	// Second round trip after the source-only rescan.
	_, err = SyncFolder(ctx, aDB, SyncOptions{DataDir: aData, Target: share})
	require.NoError(t, err)
	bRes, err = SyncFolder(ctx, bDB, SyncOptions{DataDir: bData, Target: share})
	require.NoError(t, err)
	assert.Zero(t, bRes.ImportedSessions,
		"a source-only leak-count change must not re-import the session")

	// B's locally scanned findings and scan state survive.
	got, err := bDB.SessionSecretFindings(ctx, importedID)
	require.NoError(t, err)
	assert.Len(t, got, 1, "importer's local secret findings must not be cleared")

	sess, err := bDB.GetSessionFull(ctx, importedID)
	require.NoError(t, err)
	require.NotNil(t, sess)
	assert.Equal(t, 1, sess.SecretLeakCount, "importer's leak count preserved")
	assert.Equal(t, "rules-b", sess.SecretsRulesVersion, "importer's scan version preserved")
}

func TestSyncFolderNotifiesWhenImportWritesData(t *testing.T) {
	ctx := context.Background()
	share := t.TempDir()
	aData := t.TempDir()
	bData := t.TempDir()
	aDB := testDB(t)
	bDB := testDB(t)

	require.NoError(t, aDB.SetSyncState(originStateKey, "laptop-a1b2c3"))
	require.NoError(t, bDB.SetSyncState(originStateKey, "desktop-d4e5f6"))
	seedSession(t, aDB, "sess-1", "alpha")

	_, err := SyncFolder(ctx, aDB, SyncOptions{DataDir: aData, Target: share})
	require.NoError(t, err)

	changes := 0
	_, err = SyncFolder(ctx, bDB, SyncOptions{
		DataDir:       bData,
		Target:        share,
		OnDataChanged: func() { changes++ },
	})
	require.NoError(t, err)
	assert.Equal(t, 1, changes)

	_, err = SyncFolder(ctx, bDB, SyncOptions{
		DataDir:       bData,
		Target:        share,
		OnDataChanged: func() { changes++ },
	})
	require.NoError(t, err)
	assert.Equal(t, 1, changes)
}

func TestSyncFolderUsesProvidedOrigin(t *testing.T) {
	ctx := context.Background()
	share := t.TempDir()
	dataDir := t.TempDir()
	database := testDB(t)
	seedSession(t, database, "sess-1", "alpha")

	res, err := SyncFolder(ctx, database, SyncOptions{
		DataDir: dataDir,
		Target:  share,
		Origin:  "configured-a1b2c3",
	})
	require.NoError(t, err)

	assert.Equal(t, "configured-a1b2c3", res.Origin)
	persisted, err := database.GetSyncState(originStateKey)
	require.NoError(t, err)
	assert.Empty(t, persisted)
	manifests := globArtifacts(t, share, "configured-a1b2c3", "manifests", "*"+manifestExtension)
	assert.Len(t, manifests, 1)
}

func TestImportMaintainsFTS(t *testing.T) {
	ctx := context.Background()
	store := newTestArtifactStore(t)
	origin := "laptop-a1b2c3"
	exportDB := testDB(t)
	importDB := testDB(t)
	if !importDB.HasFTS() {
		t.Skip("FTS unavailable")
	}
	seedSession(t, exportDB, "sess-1", "alpha")

	_, err := ExportToStore(ctx, exportDB, store, ExportOptions{Origin: origin, Full: true})
	require.NoError(t, err)
	imported, messages, err := importFromTestStore(ctx, importDB, store, "desktop-d4e5f6")
	require.NoError(t, err)
	require.Equal(t, 1, imported)
	require.Equal(t, 2, messages)

	page, err := importDB.Search(ctx, db.SearchFilter{Query: "world", Limit: 10})
	require.NoError(t, err)
	require.Len(t, page.Results, 1)
	assert.Equal(t, origin+"~sess-1", page.Results[0].SessionID)
}

func TestImportPreservesPinsAndStatsOnRewrite(t *testing.T) {
	ctx := context.Background()
	store := newTestArtifactStore(t)
	origin := "laptop-a1b2c3"
	exportDB := testDB(t)
	importDB := testDB(t)
	seedSession(t, exportDB, "sess-1", "alpha")

	_, err := ExportToStore(ctx, exportDB, store, ExportOptions{Origin: origin, Full: true})
	require.NoError(t, err)
	imported, messages, err := importFromTestStore(ctx, importDB, store, "desktop-d4e5f6")
	require.NoError(t, err)
	require.Equal(t, 1, imported)
	require.Equal(t, 2, messages)

	gid := origin + "~sess-1"
	importedMsgs, err := importDB.GetAllMessages(ctx, gid)
	require.NoError(t, err)
	require.Len(t, importedMsgs, 2)
	note := "keep this pin"
	_, err = importDB.PinMessage(gid, importedMsgs[1].ID, &note)
	require.NoError(t, err)

	require.NoError(t, exportDB.ReplaceSessionMessages("sess-1", []db.Message{
		{SessionID: "sess-1", Ordinal: 0, Role: "user", Content: "hello", ContentLength: 5},
		{SessionID: "sess-1", Ordinal: 1, Role: "assistant", Content: "planet", ContentLength: 6},
	}))
	_, err = ExportToStore(ctx, exportDB, store, ExportOptions{Origin: origin, Full: true})
	require.NoError(t, err)
	imported, messages, err = importFromTestStore(ctx, importDB, store, "desktop-d4e5f6")
	require.NoError(t, err)
	require.Equal(t, 1, imported)
	require.Equal(t, 2, messages)

	pins, err := importDB.ListPinnedMessages(ctx, gid, "")
	require.NoError(t, err)
	require.Len(t, pins, 1)
	assert.Equal(t, 1, pins[0].Ordinal)
	require.NotNil(t, pins[0].Note)
	assert.Equal(t, note, *pins[0].Note)

	allPins, err := importDB.ListPinnedMessages(ctx, "", "")
	require.NoError(t, err)
	require.Len(t, allPins, 1)
	assert.Equal(t, gid, allPins[0].SessionID)
	require.NotNil(t, allPins[0].Content)
	assert.Equal(t, "planet", *allPins[0].Content)

	stats, err := importDB.GetStats(ctx, false, false)
	require.NoError(t, err)
	assert.Equal(t, 1, stats.SessionCount)
	assert.Equal(t, 2, stats.MessageCount)
	assert.Equal(t, 1, stats.ProjectCount)
	assert.Equal(t, 1, stats.MachineCount)
}

func TestImportDoesNotAdvanceStateForExcludedOrTrashedSessions(t *testing.T) {
	ctx := context.Background()
	store := newTestArtifactStore(t)
	origin := "laptop-a1b2c3"
	exportDB := testDB(t)
	seedSession(t, exportDB, "sess-1", "alpha")
	_, err := ExportToStore(ctx, exportDB, store, ExportOptions{Origin: origin, Full: true})
	require.NoError(t, err)

	gid := origin + "~sess-1"
	tests := []struct {
		name string
		seed func(*testing.T, *db.DB)
	}{
		{
			name: "excluded",
			seed: func(t *testing.T, database *db.DB) {
				t.Helper()
				seedSession(t, database, gid, "alpha", func(s *db.Session) {
					s.Machine = origin
				})
				require.NoError(t, database.DeleteSession(gid))
			},
		},
		{
			name: "trashed",
			seed: func(t *testing.T, database *db.DB) {
				t.Helper()
				seedSession(t, database, gid, "alpha", func(s *db.Session) {
					s.Machine = origin
				})
				require.NoError(t, database.SoftDeleteSession(gid))
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			importDB := testDB(t)
			tt.seed(t, importDB)

			imported, messages, err := importFromTestStore(ctx, importDB, store, "desktop-d4e5f6")
			require.NoError(t, err)
			assert.Zero(t, imported)
			assert.Zero(t, messages)

			state, err := importDB.GetSyncState(importStateKey(origin, gid))
			require.NoError(t, err)
			assert.Empty(t, state)
		})
	}
}

func TestSyncFolderRetriesIncompleteForeignArtifacts(t *testing.T) {
	ctx := context.Background()
	share := t.TempDir()
	aData := t.TempDir()
	bData := t.TempDir()
	aDB := testDB(t)
	bDB := testDB(t)

	require.NoError(t, aDB.SetSyncState(originStateKey, "laptop-a1b2c3"))
	require.NoError(t, bDB.SetSyncState(originStateKey, "desktop-d4e5f6"))
	seedSession(t, aDB, "sess-1", "alpha")

	_, err := SyncFolder(ctx, aDB, SyncOptions{DataDir: aData, Target: share})
	require.NoError(t, err)
	segments := globArtifacts(t, share, "laptop-a1b2c3", "segments", "*"+segmentExtension)
	require.Len(t, segments, 1)
	require.NoError(t, os.Remove(segments[0]))

	res, err := SyncFolder(ctx, bDB, SyncOptions{DataDir: bData, Target: share})
	require.NoError(t, err)
	assert.Zero(t, res.ImportedSessions)
	assert.Zero(t, res.ImportedMessages)

	got, err := bDB.GetSessionFull(ctx, "laptop-a1b2c3~sess-1")
	require.NoError(t, err)
	assert.Nil(t, got)

	_, err = SyncFolder(ctx, aDB, SyncOptions{DataDir: aData, Target: share})
	require.NoError(t, err)
	res, err = SyncFolder(ctx, bDB, SyncOptions{DataDir: bData, Target: share})
	require.NoError(t, err)
	assert.Equal(t, 1, res.ImportedSessions)
	assert.Equal(t, 2, res.ImportedMessages)
}

func TestSyncFolderSkipsCheckpointWithMissingManifest(t *testing.T) {
	ctx := context.Background()
	share := t.TempDir()
	aData := t.TempDir()
	bData := t.TempDir()
	aDB := testDB(t)
	bDB := testDB(t)

	require.NoError(t, aDB.SetSyncState(originStateKey, "laptop-a1b2c3"))
	require.NoError(t, bDB.SetSyncState(originStateKey, "desktop-d4e5f6"))
	seedSession(t, aDB, "sess-1", "alpha")

	_, err := SyncFolder(ctx, aDB, SyncOptions{DataDir: aData, Target: share})
	require.NoError(t, err)
	manifests := globArtifacts(t, share, "laptop-a1b2c3", "manifests", "*"+manifestExtension)
	require.Len(t, manifests, 1)
	require.NoError(t, os.Remove(manifests[0]))

	res, err := SyncFolder(ctx, bDB, SyncOptions{DataDir: bData, Target: share})
	require.NoError(t, err)
	assert.Zero(t, res.ImportedSessions)
	assert.Zero(t, res.ImportedMessages)
}

func TestSyncFolderRejectsOverlappingRoots(t *testing.T) {
	ctx := context.Background()

	tests := []struct {
		name   string
		target func(string) string
	}{
		{
			name: "target is data dir",
			target: func(dataDir string) string {
				return dataDir
			},
		},
		{
			name: "target is artifact store",
			target: func(dataDir string) string {
				return filepath.Join(dataDir, "artifacts")
			},
		},
		{
			name: "target inside artifact store",
			target: func(dataDir string) string {
				return filepath.Join(dataDir, "artifacts", "share")
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			database := testDB(t)
			dataDir := t.TempDir()

			_, err := SyncFolder(ctx, database, SyncOptions{
				DataDir: dataDir,
				Target:  tt.target(dataDir),
			})
			require.Error(t, err)
			assert.Contains(t, err.Error(), "must not overlap")
		})
	}
}

func TestSyncFolderRejectsSymlinkedAncestorResolvingInsideArtifactStore(t *testing.T) {
	dataDir := t.TempDir()
	localRoot := filepath.Join(dataDir, "artifacts")
	targetDir := filepath.Join(localRoot, "shared")
	require.NoError(t, os.MkdirAll(targetDir, 0o755))
	alias := filepath.Join(t.TempDir(), "alias")
	require.NoError(t, os.Symlink(localRoot, alias))

	err := validateDisjointRoots(localRoot, filepath.Join(alias, "shared"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "must not overlap")
}

type batchNotificationTransport struct{ exchangeErr error }

func (t batchNotificationTransport) Prepare(context.Context, ArtifactStore) error { return nil }

func (t batchNotificationTransport) Exchange(context.Context, ArtifactStore) error {
	return t.exchangeErr
}

func TestSyncBatchNotificationRunsOnlyAfterSuccess(t *testing.T) {
	for _, suppressed := range []bool{false, true} {
		mode := "success"
		want := int32(1)
		if suppressed {
			mode = "shutdown"
			want = 0
		}
		t.Run(mode, func(t *testing.T) {
			repository, err := OpenRepository(t.Context(), t.TempDir())
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, repository.Close()) })
			var notifications atomic.Int32
			notify := func(ctx context.Context) {
				if !artifactMaintenanceSuppressed(ctx) {
					notifications.Add(1)
				}
			}
			database := testDB(t)
			seedSession(t, database, "notify-session", "project")
			ctx := t.Context()
			if suppressed {
				ctx = SuppressArtifactMaintenance(ctx)
			}

			_, err = syncContentWithTransport(
				ctx, database, nil, repository.Content(), notify,
				SyncOptions{Origin: "local-a1b2c3"}, batchNotificationTransport{},
			)
			require.NoError(t, err)
			assert.Equal(t, want, notifications.Load())
		})
	}
}

func TestSyncBatchNotificationSkipsFailedExchange(t *testing.T) {
	repository, err := OpenRepository(t.Context(), t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, repository.Close()) })
	database := testDB(t)
	seedSession(t, database, "notify-session", "project")
	var notifications atomic.Int32

	_, err = syncContentWithTransport(
		t.Context(), database, nil, repository.Content(),
		func(context.Context) { notifications.Add(1) },
		SyncOptions{Origin: "local-a1b2c3"},
		batchNotificationTransport{exchangeErr: errors.New("exchange failed")},
	)
	require.Error(t, err)
	assert.Zero(t, notifications.Load())
}

func TestSyncUnchangedArchiveDoesNotRepublish(t *testing.T) {
	for _, archiveSize := range []int{0, 500} {
		t.Run(fmt.Sprintf("archive-%d", archiveSize), func(t *testing.T) {
			database := testDB(t)
			for i := range archiveSize {
				require.NoError(t, database.UpsertSession(db.Session{
					ID: fmt.Sprintf("peer-%04d", i), Project: "project",
					Machine: "peer-a1b2c3", Agent: "claude",
				}))
			}
			seedSession(t, database, "session-0000", "project")
			base, err := newProtocolTestStore(t.TempDir())
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, base.Close()) })
			store := &recordingArtifactStore{ArtifactStore: base}
			opts := SyncOptions{Origin: "local-a1b2c3"}

			initial, err := syncContentWithTransport(
				t.Context(), database, nil, store, nil, opts, batchNotificationTransport{},
			)
			require.NoError(t, err)
			assert.Equal(t, 1, initial.ExportedSessions)

			store.creates = nil
			unchanged, err := syncContentWithTransport(
				t.Context(), database, nil, store, nil, opts, batchNotificationTransport{},
			)
			require.NoError(t, err)
			assert.Zero(t, unchanged.ExportedSessions)
			assert.Empty(t, store.creates,
				"unchanged sync work must not grow with the total archive")

			require.NoError(t, database.ReplaceSessionMessages("session-0000", []db.Message{{
				SessionID: "session-0000", Ordinal: 0, Role: "user", Content: "changed",
			}}))
			store.creates = nil
			changed, err := syncContentWithTransport(
				t.Context(), database, nil, store, nil, opts, batchNotificationTransport{},
			)
			require.NoError(t, err)
			assert.Equal(t, 1, changed.ExportedSessions)
			createdKinds := make(map[Kind]int)
			for _, create := range store.creates {
				createdKinds[create.Ref.Kind]++
			}
			assert.Equal(t, map[Kind]int{
				KindSegments: 1, KindManifests: 1, KindCheckpoints: 1,
			}, createdKinds)
		})
	}
}

func TestSyncWithStoreRejectsFolderWithoutRepositoryOwner(t *testing.T) {
	dataDir := t.TempDir()
	repository, err := OpenRepository(t.Context(), dataDir)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, repository.Close()) })

	_, err = SyncWithStore(t.Context(), testDB(t), repository.Content(), SyncOptions{
		DataDir: t.TempDir(), Target: t.TempDir(), Origin: "local-a1b2c3",
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrArtifactUnsupported)
}

func TestValidateSyncTargetRejectsSecretURLComponentsWithoutDisclosure(t *testing.T) {
	const secret = "target-secret"
	for _, target := range []string{
		"https://user:" + secret + "@example.invalid/archive",
		"https://example.invalid/archive?token=" + secret,
		"https://example.invalid/archive#" + secret,
		"s3://user:" + secret + "@bucket/archive",
		"s3://bucket/archive?token=" + secret,
		"s3://bucket/archive#" + secret,
	} {
		err := ValidateSyncTarget(target)
		require.ErrorIs(t, err, ErrArtifactInvalid)
		assert.NotContains(t, err.Error(), secret)
	}
	require.NoError(t, ValidateSyncTarget(filepath.Join(t.TempDir(), "folder?with#marks")),
		"non-URL folder targets retain platform-native path semantics")
}

func TestSyncWithRepositoryFolderUsesRetainedRepositoryIdentity(t *testing.T) {
	vaultDataDir := t.TempDir()
	repository, err := OpenRepository(t.Context(), vaultDataDir)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, repository.Close()) })
	legacyDataDir := t.TempDir()

	t.Run("actual vault and hierarchy are rejected", func(t *testing.T) {
		targets := map[string]string{
			"vault":      filepath.Join(vaultDataDir, repositoryDirectory),
			"ancestor":   vaultDataDir,
			"descendant": filepath.Join(vaultDataDir, repositoryDirectory, "share"),
		}
		alias := filepath.Join(t.TempDir(), "vault-alias")
		if err := os.Symlink(filepath.Join(vaultDataDir, repositoryDirectory), alias); err == nil {
			targets["symlink alias"] = alias
		}
		for name, target := range targets {
			t.Run(name, func(t *testing.T) {
				_, err := SyncWithRepository(t.Context(), testDB(t), repository, SyncOptions{
					DataDir: legacyDataDir, Target: target, Origin: "local-a1b2c3",
				})
				require.Error(t, err)
				assert.ErrorContains(t, err, "must not overlap")
			})
		}
	})

	t.Run("unrelated legacy path is allowed", func(t *testing.T) {
		target := filepath.Join(legacyDataDir, repositoryDirectory)
		_, err := SyncWithRepository(t.Context(), testDB(t), repository, SyncOptions{
			DataDir: legacyDataDir, Target: target, Origin: "local-a1b2c3",
		})
		require.NoError(t, err)
	})
}

func TestExportEmitsNewManifestAfterDataVersionChange(t *testing.T) {
	ctx := context.Background()
	database := testDB(t)
	store := newTestArtifactStore(t)
	origin := "laptop-a1b2c3"
	seedSession(t, database, "sess-1", "alpha")

	result, err := ExportToStore(ctx, database, store, ExportOptions{Origin: origin, Full: true})
	require.NoError(t, err)
	require.Equal(t, 1, result.ExportedSessions)
	_, cp, err := latestStoreCheckpointSummary(ctx, store, origin)
	require.NoError(t, err)
	require.NotNil(t, cp)
	firstHash := cp.Sessions[origin+"~sess-1"]
	require.NotEmpty(t, firstHash)

	require.NoError(t, database.SetSessionDataVersion("sess-1", 42))
	result, err = ExportToStore(ctx, database, store, ExportOptions{Origin: origin, Full: true})
	require.NoError(t, err)
	assert.Equal(t, 1, result.ExportedSessions)
	_, cp, err = latestStoreCheckpointSummary(ctx, store, origin)
	require.NoError(t, err)
	require.NotNil(t, cp)
	nextHash := cp.Sessions[origin+"~sess-1"]
	require.NotEmpty(t, nextHash)
	assert.NotEqual(t, firstHash, nextHash)

	ref, err := NewRef(origin, KindManifests, nextHash+".json")
	require.NoError(t, err)
	m, err := decodeManifestWithLimits(readContractArtifact(t, store, ref), productionArtifactLimits())
	require.NoError(t, err)
	assert.Equal(t, 42, m.DataVersion)
}

func TestExportIncludesLocalOwnedSessionClasses(t *testing.T) {
	ctx := context.Background()
	database := testDB(t)
	store := newTestArtifactStore(t)
	origin := "laptop-a1b2c3"
	seedSession(t, database, "file-sess", "alpha")
	seedSession(t, database, "claude-ai-sess", "bravo", func(s *db.Session) {
		s.Agent = "claude-ai"
	})
	seedSession(t, database, "upload-sess", "charlie", func(s *db.Session) {
		s.Agent = "upload"
	})
	seedSession(t, database, "orphan-sess", "delta", func(s *db.Session) {
		s.SourceSessionID = "missing-source"
	})

	result, err := ExportToStore(ctx, database, store, ExportOptions{Origin: origin, Full: true})
	require.NoError(t, err)
	assert.Equal(t, 4, result.ExportedSessions)

	_, cp, err := latestStoreCheckpointSummary(ctx, store, origin)
	require.NoError(t, err)
	require.NotNil(t, cp)
	assert.Contains(t, cp.Sessions, origin+"~file-sess")
	assert.Contains(t, cp.Sessions, origin+"~claude-ai-sess")
	assert.Contains(t, cp.Sessions, origin+"~upload-sess")
	assert.Contains(t, cp.Sessions, origin+"~orphan-sess")
}

func TestExportScrubsUnstableArtifactIDs(t *testing.T) {
	ctx := context.Background()
	database := testDB(t)
	store := newTestArtifactStore(t)
	origin := "laptop-a1b2c3"
	seedSession(t, database, "sess-1", "alpha")
	require.NoError(t, database.ReplaceSessionUsageEvents("sess-1", []db.UsageEvent{
		{
			SessionID:   "sess-1",
			Source:      "fixture",
			Model:       "claude-test",
			InputTokens: 1,
			OccurredAt:  "2026-06-14T01:02:04Z",
			DedupKey:    "usage-1",
		},
	}))

	_, err := ExportToStore(ctx, database, store, ExportOptions{Origin: origin, Full: true})
	require.NoError(t, err)
	_, cp, err := latestStoreCheckpointSummary(ctx, store, origin)
	require.NoError(t, err)
	require.NotNil(t, cp)
	manifestHash := cp.Sessions[origin+"~sess-1"]
	require.NotEmpty(t, manifestHash)

	ref, err := NewRef(origin, KindManifests, manifestHash+".json")
	require.NoError(t, err)
	m, err := decodeManifestWithLimits(readContractArtifact(t, store, ref), productionArtifactLimits())
	require.NoError(t, err)
	require.Len(t, m.UsageEvents, 1)
	manifestData, err := canonicalJSON(m)
	require.NoError(t, err)
	assert.NotContains(t, string(manifestData), `"ID"`)
	assert.NotContains(t, string(manifestData), `"SessionID"`)

	msgs := testStoreManifestMessages(t, store, origin, m)
	require.Len(t, msgs, 2)
	for _, msg := range msgs {
		assert.Zero(t, msg.ID)
		assert.Empty(t, msg.SessionID)
	}
}

func TestExportRejectsInvalidOriginBeforeCreatingPaths(t *testing.T) {
	ctx := context.Background()
	database := testDB(t)
	store := newTestArtifactStore(t)
	seedSession(t, database, "sess-1", "alpha")

	result, err := ExportToStore(ctx, database, store, ExportOptions{Origin: "../outside", Full: true})
	require.Error(t, err)
	assert.Zero(t, result.ExportedSessions)
	assert.Contains(t, err.Error(), "invalid artifact origin")
	assertNoPublishedArtifacts(t, store, "outside-a1b2c3")
}

func TestSyncFolderHealsCorruptSegmentInShare(t *testing.T) {
	ctx := context.Background()
	share := t.TempDir()
	aData := t.TempDir()
	bData := t.TempDir()
	aDB := testDB(t)
	bDB := testDB(t)

	require.NoError(t, aDB.SetSyncState(originStateKey, "laptop-a1b2c3"))
	require.NoError(t, bDB.SetSyncState(originStateKey, "desktop-d4e5f6"))
	seedSession(t, aDB, "sess-1", "alpha")

	_, err := SyncFolder(ctx, aDB, SyncOptions{DataDir: aData, Target: share})
	require.NoError(t, err)

	// Corrupt A's segment in the share, as an interrupted file-sync write would.
	segments := globArtifacts(t, share, "laptop-a1b2c3", "segments", "*"+segmentExtension)
	require.Len(t, segments, 1)
	require.NoError(t, os.Remove(segments[0]))
	require.NoError(t, os.WriteFile(segments[0], compressPeerTestData(t, []byte("tampered")), 0o644))

	// B tolerates the corrupt share: sync succeeds, the poison is not
	// mirrored into B's local store, and repeat runs stay healthy.
	res, err := SyncFolder(ctx, bDB, SyncOptions{DataDir: bData, Target: share})
	require.NoError(t, err)
	assert.Zero(t, res.ImportedSessions)
	segmentRef, err := FromWireRef("laptop-a1b2c3", KindSegments, filepath.Base(segments[0]))
	require.NoError(t, err)
	bRepository, err := OpenRepository(ctx, bData)
	require.NoError(t, err)
	_, err = bRepository.Content().Stat(ctx, segmentRef)
	assert.ErrorIs(t, err, ErrArtifactNotFound)
	require.NoError(t, bRepository.Close())
	_, err = SyncFolder(ctx, bDB, SyncOptions{DataDir: bData, Target: share})
	require.NoError(t, err)

	// A still holds the valid copy and repairs the share on its next sync.
	aRepository, err := OpenRepository(ctx, aData)
	require.NoError(t, err)
	_, reader, err := aRepository.Content().Open(ctx, segmentRef)
	require.NoError(t, err)
	var want bytes.Buffer
	require.NoError(t, EncodeWire(ctx, segmentRef, reader, &want))
	require.NoError(t, reader.Verify())
	require.NoError(t, reader.Close())
	require.NoError(t, aRepository.Close())
	_, err = SyncFolder(ctx, aDB, SyncOptions{DataDir: aData, Target: share})
	require.NoError(t, err)
	got, err := os.ReadFile(segments[0])
	require.NoError(t, err)
	require.Equal(t, want.Bytes(), got, "publisher should repair the corrupt share copy")

	// With the share healed, B converges.
	res, err = SyncFolder(ctx, bDB, SyncOptions{DataDir: bData, Target: share})
	require.NoError(t, err)
	assert.Equal(t, 1, res.ImportedSessions)
	assert.Equal(t, 2, res.ImportedMessages)
}

func globArtifacts(t *testing.T, root, origin, kind, pattern string) []string {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(root, origin, kind, pattern))
	require.NoError(t, err)
	return paths
}

func testDB(t *testing.T) *db.DB {
	t.Helper()
	database, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	require.NoError(t, err)
	t.Cleanup(func() { database.Close() })
	return database
}

func seedSession(t *testing.T, database *db.DB, id, project string, opts ...func(*db.Session)) {
	t.Helper()
	sess := db.Session{
		ID:               id,
		Project:          project,
		Machine:          "local",
		Agent:            "claude",
		MessageCount:     2,
		UserMessageCount: 1,
		FirstMessage:     new("hello"),
		StartedAt:        new("2026-06-14T01:02:03Z"),
		EndedAt:          new("2026-06-14T01:03:03Z"),
		SessionName:      new("Test Session"),
		CreatedAt:        "2026-06-14T01:02:03Z",
	}
	for _, opt := range opts {
		opt(&sess)
	}
	require.NoError(t, database.UpsertSession(sess))
	require.NoError(t, database.ReplaceSessionMessages(id, []db.Message{
		{SessionID: id, Ordinal: 0, Role: "user", Content: "hello", ContentLength: 5},
		{SessionID: id, Ordinal: 1, Role: "assistant", Content: "world", ContentLength: 5},
	}))
}
